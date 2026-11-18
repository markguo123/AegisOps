// Package k8sgateway owns the Kubernetes API access boundary. Agent/tools depend
// on the Gateway contract; target credentials remain with its resolver.
package k8sgateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/rest"
)

// OpenRequest is transport-neutral: no upstream credentials, client configs or
// tool names. A future remote implementation authenticates the caller and
// resolves TargetID/ExecutionID against its server-owned execution records.
type OpenRequest struct {
	TargetID    string
	ExecutionID string
	ReadOnly    bool
}
type Lease struct {
	Endpoint string
	Token    string
	Close    func()
}
type Gateway interface {
	Open(context.Context, OpenRequest) (Lease, error)
}

// BoundTarget is private control-plane configuration, never an RPC payload.
type BoundTarget struct {
	Config    *rest.Config
	Authorize func(*http.Request) error
	Validate  func(context.Context) error
}
type TargetResolver func(context.Context, string) (BoundTarget, error)
type inProcess struct{ resolve TargetResolver }

func NewInProcess(resolve TargetResolver) Gateway { return &inProcess{resolve: resolve} }

func (g *inProcess) Open(ctx context.Context, request OpenRequest) (Lease, error) {
	if g.resolve == nil || request.TargetID == "" || request.ExecutionID == "" {
		return Lease{}, fmt.Errorf("POLICY_DENIED: incomplete execution context")
	}
	target, err := g.resolve(ctx, request.TargetID)
	if err != nil {
		return Lease{}, err
	}
	if target.Config == nil || target.Authorize == nil {
		return Lease{}, fmt.Errorf("TOOL_UNAVAILABLE: Gateway Authorizer is unavailable")
	}
	if target.Validate != nil {
		if err := target.Validate(ctx); err != nil {
			return Lease{}, err
		}
	}
	upstream, err := url.Parse(target.Config.Host)
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") {
		return Lease{}, fmt.Errorf("TARGET_ISOLATION_DENIED: invalid API endpoint")
	}
	transport, err := rest.TransportFor(target.Config)
	if err != nil {
		return Lease{}, err
	}
	token := rand.Text()
	runCtx, cancel := context.WithCancel(ctx)
	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = transport
	director := reverse.Director
	reverse.Director = func(r *http.Request) {
		director(r)
		r.Host = upstream.Host
		for key := range r.Header {
			lower := strings.ToLower(key)
			if lower == "authorization" || lower == "cookie" || lower == "proxy-authorization" || strings.HasPrefix(lower, "impersonate-") || strings.HasPrefix(lower, "x-remote-") || strings.HasPrefix(lower, "x-forwarded-") {
				r.Header.Del(key)
			}
		}
	}
	reverse.ModifyResponse = func(response *http.Response) error {
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return fmt.Errorf("POLICY_DENIED: upstream redirect")
		}
		return nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		proxyError(w, 502, "CLUSTER_UNAVAILABLE: bound upstream request failed")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if runCtx.Err() != nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			proxyError(w, 401, "POLICY_DENIED: invalid execution channel")
			return
		}
		// Preserve downstream cancellation while binding the full lease lifetime.
		callCtx, callCancel := context.WithCancel(r.Context())
		defer callCancel()
		stop := context.AfterFunc(runCtx, callCancel)
		defer stop()
		r = r.WithContext(callCtx)
		if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" || r.URL.IsAbs() || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "..") {
			proxyError(w, 403, "POLICY_DENIED: unsupported API transport")
			return
		}
		if target.Validate != nil {
			if err := target.Validate(callCtx); err != nil {
				proxyError(w, 403, "SCOPE_DENIED: execution context invalidated")
				return
			}
		}
		if err := proxyRequestPolicy(r, request.ReadOnly); err != nil {
			proxyError(w, 403, err.Error())
			return
		}
		if err := target.Authorize(r); err != nil {
			proxyError(w, 403, err.Error())
			return
		}
		reverse.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		return Lease{}, err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768}
	var once sync.Once
	closeLease := func() { once.Do(func() { cancel(); _ = server.Close() }) }
	go func() { _ = server.Serve(listener) }()
	context.AfterFunc(runCtx, func() { _ = server.Close() })
	return Lease{Endpoint: "http://" + listener.Addr().String(), Token: token, Close: closeLease}, nil
}
