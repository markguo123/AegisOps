package k8sgateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
)

// RequestAuthorizer is shared by client-go and subprocess channels. The context
// loader owns identity/target binding; requests cannot select another principal.
type RequestAuthorizer interface{ AuthorizeRequest(*http.Request) error }
type RequestRecord struct {
	Method, Path string
	Allowed      bool
	Duration     time.Duration
}
type RequestObserver func(context.Context, RequestRecord) error
type AccessContext struct {
	Current func(context.Context) (*authz.EffectiveAccess, error)
	Observe RequestObserver
}

type requestReceiptKey struct{}
type requestReceipt struct {
	owner               *AccessContext
	method, path, query string
	revision            uint64
}

func (a *AccessContext) AuthorizeRequest(r *http.Request) error {
	if receipt, ok := r.Context().Value(requestReceiptKey{}).(requestReceipt); ok && receipt.owner == a && receipt.method == r.Method && receipt.path == r.URL.Path && receipt.query == r.URL.RawQuery && receipt.revision == authz.AuthorizationRevision() {
		return nil
	}
	started := time.Now()
	revision := authz.AuthorizationRevision()
	if a == nil || a.Current == nil {
		return fmt.Errorf("SCOPE_DENIED: Gateway authorization context is unavailable")
	}
	current, err := a.Current(r.Context())
	if err == nil && current != nil {
		err = current.CheckRequest(r)
	} else if err == nil {
		err = fmt.Errorf("SCOPE_DENIED: Gateway authorization context is unavailable")
	}
	if a.Observe != nil {
		if auditErr := a.Observe(r.Context(), RequestRecord{Method: r.Method, Path: r.URL.Path, Allowed: err == nil, Duration: time.Since(started)}); auditErr != nil {
			return fmt.Errorf("TOOL_UNAVAILABLE: Gateway access audit unavailable")
		}
	}
	if err == nil {
		if authz.AuthorizationRevision() != revision {
			return fmt.Errorf("SCOPE_DENIED: authorization changed while checking request")
		}
		*r = *r.WithContext(context.WithValue(r.Context(), requestReceiptKey{}, requestReceipt{owner: a, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, revision: revision}))
	}
	return err
}
