package scanner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/k8s-ai/k8s-ai/internal/kubernetes"
	"github.com/k8s-ai/k8s-ai/internal/model"
)

func TestNodeDiagnosticCollectionReadsOnlyTarget(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}})
	target := &model.ResourceRef{Kind: "Node", Name: "node"}
	opts := model.ScanOptions{DiagnosticTarget: target, CollectEvents: true}
	snap, err := New(kubernetes.NewClientWithClientset(client)).Phase1(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	actions := client.Actions()
	q := CollectionQuality(snap, opts, nil)
	if len(actions) != 1 || !actions[0].Matches("get", "nodes") || len(snap.Nodes) != 1 || q.Planned != 1 || len(q.MissingData) == 0 {
		t.Fatalf("actions=%v nodes=%v quality=%+v", actions, snap.Nodes, q)
	}
}

func TestWorkloadDiagnosticCollectionUsesControllerUID(t *testing.T) {
	controller := true
	ownedBy := func(kind string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, UID: uid, Controller: &controller}}
	}
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "app", UID: "current"}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "app-new", UID: "active-rs", OwnerReferences: ownedBy("Deployment", "current")}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "app-old", UID: "old-rs", OwnerReferences: ownedBy("Deployment", "deleted")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "app-new-pod", UID: "target-pod", OwnerReferences: ownedBy("ReplicaSet", "active-rs")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "app-old-pod", UID: "old-pod", OwnerReferences: ownedBy("ReplicaSet", "old-rs")}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "target-event"}, InvolvedObject: corev1.ObjectReference{UID: "target-pod"}},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "old-event"}, InvolvedObject: corev1.ObjectReference{UID: "old-pod"}},
	)
	opts := model.ScanOptions{DiagnosticTarget: &model.ResourceRef{Kind: "Deployment", Namespace: "team", Name: "app"}, CollectEvents: true}
	snap, err := New(kubernetes.NewClientWithClientset(client)).Phase1(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Pods) != 1 || snap.Pods[0].Ref.UID != "target-pod" || len(snap.EventsIndex["target-pod"]) != 1 || len(snap.EventsIndex["old-pod"]) != 0 {
		t.Fatalf("pods=%+v events=%+v", snap.Pods, snap.EventsIndex)
	}
	if len(client.Actions()) != 4 || CollectionQuality(snap, opts, nil).Planned != 4 {
		t.Fatalf("unexpected collection: %v", client.Actions())
	}
}
