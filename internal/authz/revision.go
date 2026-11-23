package authz

import "github.com/k8s-ai/k8s-ai/internal/authstate"

func AuthorizationRevision() uint64         { return authstate.Revision() }
func AuthorizationChanges() <-chan struct{} { return authstate.Changes() }
func InvalidateAuthorization()              { authstate.Invalidate() }
