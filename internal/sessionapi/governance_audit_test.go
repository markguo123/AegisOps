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
)

func TestGovernanceAuditFailurePreventsMutation(t *testing.T) {
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: "u-owner", Name: "original"}
	s := New(chat, "model").WithPlatformAccess(&service.PlatformAccess{}).WithAudit(func(context.Context, store.AuditEvent) error { return errors.New("offline") })
	handler := s.AuthenticatedHandler(ownershipResolver(map[string]string{"admin": "admin"}, map[string][]string{"admin": {"*"}}))
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/sessions/s/rename", `{"name":"changed"}`},
		{"POST", "/api/v1/sessions/s/pin", `{"pinned":true}`},
		{"DELETE", "/api/v1/sessions/s", ""},
		{"GET", "/api/v1/sessions", ""},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 503 {
			t.Fatalf("%s status=%d", tc.path, response.Code)
		}
		if chat.sessions["s"] == nil || chat.sessions["s"].Name != "original" || chat.sessions["s"].Pinned {
			t.Fatal("unaudited mutation")
		}
	}
}
