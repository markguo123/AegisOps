package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestKubectlProxyPermitAndCredentialIsolation(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "Bearer real-cluster-credential" {
			t.Error("missing upstream credentials")
		}
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	runner := &Runner{targetBound: true, restConfig: &rest.Config{Host: upstream.URL, BearerToken: "real-cluster-credential"}, requestCheck: func(r *http.Request) error { return nil }}
	command := "kubectl set image deployment/api api=example:v2 -n team"
	if _, _, err := runner.startKubectlProxy(context.Background(), command); err == nil {
		t.Fatal("permit bypass")
	}
	ctx, err := runner.PrepareKubectl(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.startKubectlProxy(ctx, command); err == nil {
		t.Fatal("approval bypass")
	}
	ctx, err = runner.AuthorizeKubectl(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.startKubectlProxy(ctx, command+"x"); err == nil {
		t.Fatal("changed command accepted")
	}
	ctx, closeLease, err := runner.startKubectlProxy(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLease()
	file := ctx.Value(proxyConfigKey{}).(string)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "real-cluster-credential") || strings.Contains(string(raw), upstream.URL) {
		t.Fatal("real target credentials escaped")
	}
	loader, err := clientcmd.NewClientConfigFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("PATCH", cfg.Host+"/apis/apps/v1/namespaces/team/deployments/api", strings.NewReader(`{"spec":{"replicas":2}}`))
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || hits != 1 {
		t.Fatalf("status=%d hits=%d", response.StatusCode, hits)
	}
	if _, _, err := runner.startKubectlProxy(ctx, command); err == nil {
		t.Fatal("permit replay accepted")
	}
	closeLease()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("temporary credentials retained")
	}
	if response, err := client.Do(req); err == nil {
		response.Body.Close()
		t.Fatal("closed lease still usable")
	}
}

func TestProxyCommandEnvelopeAllowsGeneralOperations(t *testing.T) {
	for _, command := range []string{"kubectl set image deployment/api api=example:v2 -n team", "kubectl label pod/api owner=ops -n team", "kubectl annotate deployment/api note=test -n team"} {
		if err := validateProxyCommand(command); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
	for _, command := range []string{"kubectl get pods --server=https://other", "kubectl get pods -o=go-template-file=/etc/passwd", "kubectl patch deployment/api -f local.json", "kubectl get pods; curl https://example.com"} {
		if err := validateProxyCommand(command); err == nil {
			t.Errorf("unsafe mechanics accepted: %s", command)
		}
	}
}
