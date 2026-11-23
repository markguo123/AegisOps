package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
)

func TestKubectlAuthenticatesThroughGateway(t *testing.T) {
	bin, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl is not installed")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/version" || req.Header.Get("Authorization") != "Bearer upstream-test-token" {
			http.Error(w, "unexpected upstream request", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"major":"1","minor":"29","gitVersion":"v1.29.0-gateway-test"}`)
	}))
	defer upstream.Close()
	runner := NewRunner(Options{
		KubectlBin:   bin,
		Kubeconfig:   filepath.Join(t.TempDir(), "upstream-kubeconfig"),
		TargetBound:  true,
		RESTConfig:   &rest.Config{Host: upstream.URL, BearerToken: "upstream-test-token"},
		RequestCheck: func(*http.Request) error { return nil },
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const command = "kubectl version -o json"
	ctx, err = runner.PrepareKubectl(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = runner.AuthorizeKubectl(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunKubectl(ctx, command)
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "v1.29.0-gateway-test") {
		t.Fatalf("kubectl failed through gateway: %+v, %v", result, err)
	}
}
