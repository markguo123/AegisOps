package sessionapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
	"github.com/k8s-ai/k8s-ai/internal/session"
)

func TestQuestionOwnerBindingReplayAndDuplicate(t *testing.T) {
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: "u-owner"}
	s := New(chat, "model")
	q := decision.Question{ID: "q", SessionID: "s", TurnID: "t", Question: "选择？", ExpiresAt: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan decision.Answer, 1)
	go func() { a, _ := s.askQuestion(ctx, q); done <- a }()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		ready := s.questions["s"] != nil
		s.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("question not registered")
		}
		time.Sleep(time.Millisecond)
	}
	h := s.AuthenticatedHandler(ownershipResolver(map[string]string{"token": "owner"}, nil))
	request := func(method, body string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, "/api/v1/sessions/s/question", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	request("GET", "", 200)
	a := decision.Answer{ID: "q", SessionID: "s", TurnID: "wrong", Status: "skipped"}
	b, _ := json.Marshal(a)
	request("POST", string(b), 409)
	a.TurnID = "t"
	b, _ = json.Marshal(a)
	request("POST", string(b), 200)
	request("POST", string(b), 409)
	select {
	case got := <-done:
		if got.Status != "skipped" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("answer not delivered")
	}
}
