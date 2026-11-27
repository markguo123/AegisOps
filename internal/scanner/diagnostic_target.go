package scanner

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/k8s-ai/k8s-ai/internal/model"
)

func (c *collector) collectDiagnosticTarget(ctx context.Context, opts model.ScanOptions) (*model.ClusterSnapshot, error) {
	target := *opts.DiagnosticTarget
	snap := &model.ClusterSnapshot{EventsIndex: map[string][]model.EventInfo{}}
	collect := &snapCollector{snap: snap}
	snap.DiagnosticReads++
	if target.Kind == "Node" {
		node, err := c.reader.GetNode(ctx, target.Name)
		if err != nil {
			return nil, err
		}
		collect.addNodes([]corev1.Node{*node})
		return snap, nil
	}
	if target.Namespace == "" {
		return nil, fmt.Errorf("workload diagnosis requires a namespace")
	}
	uid, err := c.collectWorkload(ctx, collect, target)
	if err != nil {
		return nil, err
	}
	gap := func(kind, op string, err error) {
		collect.addError(model.CollectionError{
			Resource:  model.ResourceRef{Kind: kind, Namespace: target.Namespace},
			Operation: op, Message: err.Error(), Time: time.Now().UTC().Format(time.RFC3339),
		})
	}
	owners := map[types.UID]string{}
	eventUIDs := map[types.UID]bool{}
	if uid != "" {
		eventUIDs[uid] = true
	}
	if uid == "" {
		snap.DiagnosticReads++
		gap("Pod", "resolve_owner_uid", fmt.Errorf("workload UID unavailable"))
	} else if target.Kind == "Deployment" {
		snap.DiagnosticReads++
		replicas, err := c.reader.ListReplicaSets(ctx, target.Namespace)
		if err != nil {
			gap("ReplicaSet", "list_replica_sets", err)
		} else {
			for _, replica := range replicas {
				owner := metav1.GetControllerOf(&replica)
				if replica.UID != "" && owner != nil && owner.Kind == "Deployment" && owner.UID == uid {
					owners[replica.UID] = "ReplicaSet"
					eventUIDs[replica.UID] = true
					collect.addReplicaSets([]appsv1.ReplicaSet{replica})
				}
			}
		}
	} else {
		owners[uid] = target.Kind
	}
	if len(owners) != 0 {
		snap.DiagnosticReads++
		pods, err := c.reader.ListPods(ctx, target.Namespace)
		if err != nil {
			gap("Pod", "list_pods", err)
		} else {
			for _, pod := range pods {
				owner := metav1.GetControllerOf(&pod)
				if owner == nil {
					continue
				}
				kind, owned := owners[owner.UID]
				if owned && kind == owner.Kind {
					collect.addPods([]corev1.Pod{pod})
					if pod.UID != "" {
						eventUIDs[pod.UID] = true
					}
				}
			}
		}
	}
	if opts.CollectEvents && len(eventUIDs) != 0 {
		snap.DiagnosticReads++
		events, err := c.reader.ListEvents(ctx, target.Namespace)
		if err != nil {
			gap("Event", "list_events", err)
		} else {
			for _, event := range events {
				if eventUIDs[event.InvolvedObject.UID] {
					collect.addEvents([]corev1.Event{event})
				}
			}
		}
	}
	return snap, ctx.Err()
}

func (c *collector) collectWorkload(ctx context.Context, collect *snapCollector, target model.ResourceRef) (types.UID, error) {
	switch target.Kind {
	case "Deployment":
		object, err := c.reader.GetDeployment(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", err
		}
		collect.addDeployments([]appsv1.Deployment{*object})
		return object.UID, nil
	case "StatefulSet":
		object, err := c.reader.GetStatefulSet(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", err
		}
		collect.addStatefulSets([]appsv1.StatefulSet{*object})
		return object.UID, nil
	case "DaemonSet":
		object, err := c.reader.GetDaemonSet(ctx, target.Namespace, target.Name)
		if err != nil {
			return "", err
		}
		collect.addDaemonSets([]appsv1.DaemonSet{*object})
		return object.UID, nil
	default:
		return "", fmt.Errorf("unsupported diagnostic kind %q", target.Kind)
	}
}
