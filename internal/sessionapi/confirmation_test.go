package sessionapi

import (
	"context"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlatformConfirmationCannotPreapproveOrReplay(t *testing.T) {
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: "u-owner"}
	s := New(chat, "model").WithPlatformAccess(&service.PlatformAccess{})
	s.WithAudit(func(context.Context, store.AuditEvent) error { return nil })
	s.pending["s"] = make(chan string, 1)
	h := s.AuthenticatedHandler(ownershipResolver(map[string]string{"token": "owner"}, nil))
	submit := func(id string, want int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/sessions/s/approve", strings.NewReader(`{"choice":"yes","approvalId":"`+id+`"}`))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status %d want %d", w.Code, want)
		}
	}
	submit("", 409)
	id, ch, done := s.newConfirmation("s")
	submit("other", 409)
	submit(id, 200)
	if <-ch != "yes" {
		t.Fatal("decision lost")
	}
	submit(id, 409)
	done()
	next, _, closeNext := s.newConfirmation("s")
	defer closeNext()
	submit(id, 409)
	ticket := s.tickets["s"]
	ticket.expires = time.Now().Add(-time.Second)
	s.tickets["s"] = ticket
	submit(next, 409)
}
