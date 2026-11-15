package sessionapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
)

func (s *Server) askQuestion(ctx context.Context, q decision.Question) (decision.Answer, error) {
	p, err := decision.New(q)
	if err != nil {
		return decision.Answer{}, err
	}
	s.mu.Lock()
	if s.questions == nil {
		s.questions = map[string]*decision.Pending{}
		s.questionViews = map[string]decision.Question{}
	}
	if s.questions[q.SessionID] != nil {
		s.mu.Unlock()
		return decision.Answer{}, decision.ErrInvalid
	}
	s.questions[q.SessionID] = p
	s.questionViews[q.SessionID] = q
	s.mu.Unlock()
	defer func() {
		p.Close()
		s.mu.Lock()
		delete(s.questions, q.SessionID)
		delete(s.questionViews, q.SessionID)
		s.mu.Unlock()
	}()
	select {
	case a := <-p.Answers():
		return a, nil
	case <-ctx.Done():
		return decision.Answer{}, ctx.Err()
	}
}

func (s *Server) handleQuestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	principal, _ := principalFrom(r)
	if principal.User == nil || sess.UserID != principal.User.ID {
		writeError(w, http.StatusForbidden, decision.ErrStale)
		return
	}
	s.mu.Lock()
	p := s.questions[id]
	q, exists := s.questionViews[id]
	s.mu.Unlock()
	if r.Method == http.MethodGet {
		if !exists || !time.Now().Before(q.ExpiresAt) {
			writeJSON(w, 200, nil)
		} else {
			writeJSON(w, 200, q)
		}
		return
	}
	var a decision.Answer
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		writeError(w, 400, err)
		return
	}
	if p == nil || a.SessionID != id {
		writeError(w, 409, decision.ErrStale)
		return
	}
	if err := p.Submit(a, time.Now()); err != nil {
		code := 409
		if err == decision.ErrInvalid {
			code = 400
		}
		writeError(w, code, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "accepted"})
}
