package sessionapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func (s *Server) handleBranch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	principal, ok := principalFrom(r)
	if !ok || principal.User == nil {
		writeSessionAccessError(w, session.ErrForbidden)
		return
	}
	source, err := s.readableSession(r, id)
	if err != nil || source.UserID != principal.User.ID {
		writeSessionAccessError(w, session.ErrNotFound)
		return
	}
	var body struct {
		TurnID string `json:"turnId"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil ||
		strings.TrimSpace(body.TurnID) == "" || len(body.TurnID) > 128 {
		writeError(w, http.StatusBadRequest, errors.New("请选择已完成的历史回合"))
		return
	}
	brancher, ok := s.chat.(interface {
		BranchSession(context.Context, *session.Session, string, string, func() error) error
	})
	if !ok {
		writeError(w, http.StatusNotImplemented, errors.New("会话分叉未接入"))
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, errors.New("需要有效的分叉请求标识"))
		return
	}
	hash := sha256.Sum256([]byte(principal.User.ID + "\x00" + id + "\x00" + body.TurnID + "\x00" + key))
	branchID := fmt.Sprintf("branch_%x", hash[:16])
	s.mu.Lock()
	if _, running := s.pending[id]; running || s.branching[id] {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, service.ErrBranchSourceRunning)
		return
	}
	if s.branching == nil {
		s.branching = make(map[string]bool)
	}
	s.branching[id] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.branching, id)
		s.mu.Unlock()
	}()
	recheck := func() error {
		if principal.recheck != nil {
			user, role, err := principal.recheck(r.Context())
			if err != nil || user == nil || role == nil || user.ID != source.UserID {
				return session.ErrForbidden
			}
		}
		current, err := s.chat.GetSession(id)
		if err != nil || current.UserID != source.UserID || current.Cluster != source.Cluster ||
			(s.platform != nil && (!s.covered(r.Context(), source, source.UserID) || !s.covered(r.Context(), current, source.UserID))) {
			return session.ErrForbidden
		}
		return nil
	}
	if err := brancher.BranchSession(r.Context(), source, body.TurnID, branchID, recheck); err != nil {
		switch {
		case errors.Is(err, session.ErrForbidden):
			writeSessionAccessError(w, err)
		case errors.Is(err, service.ErrBranchCutUnavailable), errors.Is(err, service.ErrBranchSourceRunning), errors.Is(err, store.ErrExists):
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, store.ErrSessionTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, errors.New("继承的问答超过会话容量"))
		default:
			writeError(w, http.StatusInternalServerError, errors.New("无法创建分支会话"))
		}
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"sessionId": branchID})
}

func (s *Server) handleBranchOrigin(w http.ResponseWriter, r *http.Request) {
	sess, err := s.readableSession(r, r.PathValue("id"))
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	if sess.Branch == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	if _, err := s.readableSession(r, sess.Branch.SessionID); err != nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, sess.Branch)
}
