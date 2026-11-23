package sessionapi

import (
	"context"
	"errors"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApprovalAuditFailureAllowsRetry(t *testing.T) {
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: "u-owner"}
	s := New(chat, "model").WithPlatformAccess(&service.PlatformAccess{})
	s.pending["s"] = make(chan string, 1)
	id, reply, done := s.newConfirmation("s", "")
	defer done()
	failing := true
	s.WithAudit(func(context.Context, store.AuditEvent) error {
		if failing {
			return errors.New("audit unavailable")
		}
		return nil
	})
	h := s.AuthenticatedHandler(ownershipResolver(map[string]string{"token": "owner"}, nil))
	for _, want := range []int{503, 200} {
		r := httptest.NewRequest("POST", "/api/v1/sessions/s/approve", strings.NewReader(`{"choice":"yes","approvalId":"`+id+`"}`))
		r.Header.Set("Authorization", "Bearer token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
		if failing {
			select {
			case <-reply:
				t.Fatal("approval woke before audit succeeded")
			default:
			}
		}
		failing = false
	}
	if <-reply != "yes" {
		t.Fatal("retry lost the approved answer")
	}
}

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
	id, ch, done := s.newConfirmation("s", "")
	submit("other", 409)
	submit(id, 200)
	if <-ch != "yes" {
		t.Fatal("decision lost")
	}
	submit(id, 409)
	done()
	next, _, closeNext := s.newConfirmation("s", "")
	defer closeNext()
	submit(id, 409)
	ticket := s.tickets["s"]
	ticket.expires = time.Now().Add(-time.Second)
	s.tickets["s"] = ticket
	submit(next, 409)
}

type conflictingApprovalChat struct{ *fakeChat }

func (conflictingApprovalChat) CommitApproval(context.Context, string, string, string, uint64) error {
	return store.ErrChatHeadConflict
}

func TestApprovalCommitConflictDoesNotAuditSuccess(t *testing.T) {
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: "u-owner"}
	server := New(conflictingApprovalChat{chat}, "model").WithPlatformAccess(&service.PlatformAccess{})
	server.pending["s"] = make(chan string, 1)
	id, reply, done := server.newConfirmation("s", "")
	defer done()
	var events []store.AuditEvent
	server.WithAudit(func(_ context.Context, event store.AuditEvent) error {
		events = append(events, event)
		return nil
	})
	handler := server.AuthenticatedHandler(ownershipResolver(map[string]string{"token": "owner"}, nil))
	request := httptest.NewRequest("POST", "/api/v1/sessions/s/approve", strings.NewReader(`{"choice":"yes","approvalId":"`+id+`"}`))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 409 || len(events) != 1 || events[0].Action != "approval.answer.attempt" || events[0].Status != "pending" || len(reply) != 0 {
		t.Fatalf("conflicting approval must only audit an attempt: status=%d events=%+v", response.Code, events)
	}
}
