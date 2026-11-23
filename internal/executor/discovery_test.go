package executor

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestResolveCommandResourceUsesTargetMapper(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.io", Version: "v1"}})
	mapper.Add(schema.GroupVersionKind{Group: "example.io", Version: "v1", Kind: "Widget"}, meta.RESTScopeNamespace)
	r := NewRunner(Options{Mapper: mapper, Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())})
	g, res, cluster, e := r.ResolveCommandResource(context.Background(), "widgets.example.io")
	if e != nil || g != "example.io" || res != "widgets" || cluster {
		t.Fatalf("%s/%s cluster=%v %v", g, res, cluster, e)
	}
}
