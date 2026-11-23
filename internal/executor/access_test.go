package executor

import (
	"context"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCanIUsesSelfSubjectAccessReview(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})

	r := NewRunner(Options{Clientset: cs})
	allowed, err := r.CanI(context.Background(), "patch", "apps", "deployments", "test-complex", "app-backend")
	if err != nil {
		t.Fatal(err)
	}
	if !allowed {
		t.Fatal("SelfSubjectAccessReview allowed result was ignored")
	}
}

func TestCanISeparatesExecSubresource(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		a := review.Spec.ResourceAttributes
		if a.Resource != "pods" || a.Subresource != "exec" || a.Verb != "create" {
			t.Fatalf("invalid resource attributes: %+v", a)
		}
		review.Status.Allowed = true
		return true, review, nil
	})
	r := NewRunner(Options{Clientset: cs})
	if ok, e := r.CanI(context.Background(), "create", "", "pods/exec", "team", "api"); e != nil || !ok {
		t.Fatalf("%v %v", ok, e)
	}
}
