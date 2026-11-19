package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/config"
)

func TestModelChecksUseSavedProvider(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer saved-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"model-a"},{"id":"model-a"},{"id":"saved-key"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	for _, action := range []string{"discover", "probe"} {
		t.Run(action, func(t *testing.T) {
			server := New(nil).WithModelChecks(func(context.Context) (*config.Config, error) {
				cfg := &config.Config{}
				cfg.LLM.Enabled = true
				cfg.LLM.Endpoint = provider.URL + "/v1"
				cfg.LLM.APIKey = "saved-key"
				return cfg, nil
			})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/settings/model/"+action, strings.NewReader(`{"model":"model-a"}`))
			request.SetPathValue("action", action)
			recorder := httptest.NewRecorder()
			server.handleModelCheck(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || strings.Contains(body, "saved-key") ||
				(action == "discover" && strings.Count(body, "model-a") != 1) {
				t.Fatalf("unexpected response %d: %s", recorder.Code, body)
			}
		})
	}
}
