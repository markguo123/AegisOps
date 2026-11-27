package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_deployment。
type deploymentTool struct{ base }

func (t *deploymentTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	d, err := t.reader.GetDeployment(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	m := workloadBase("Deployment", d.Namespace, d.Name, d.Spec.Replicas, d.Status.ReadyReplicas, d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
	describeWorkloadRevision(m, d.ObjectMeta, d.Status.ObservedGeneration)
	m["conditions"] = deploymentConditions(d.Status.Conditions)
	m["containers"] = containerSpecs(d.Spec.Template.Spec.Containers)
	m["selector"] = d.Spec.Selector
	m["strategy"] = d.Spec.Strategy
	return tools.ToolResult{
		Data: m,
		Text: fmt.Sprintf("deployment %s/%s ready=%d/%d", d.Namespace, d.Name, d.Status.ReadyReplicas, derefI32(d.Spec.Replicas)),
	}, nil
}

// get_statefulset。
type statefulSetTool struct{ base }

func (t *statefulSetTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	s, err := t.reader.GetStatefulSet(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	m := workloadBase("StatefulSet", s.Namespace, s.Name, s.Spec.Replicas, s.Status.ReadyReplicas, s.Status.UpdatedReplicas, s.Status.AvailableReplicas)
	m["currentReplicas"] = s.Status.CurrentReplicas
	describeWorkloadRevision(m, s.ObjectMeta, s.Status.ObservedGeneration)
	m["containers"] = containerSpecs(s.Spec.Template.Spec.Containers)
	m["serviceName"] = s.Spec.ServiceName
	return tools.ToolResult{Data: m, Text: fmt.Sprintf("statefulset %s/%s ready=%d/%d", s.Namespace, s.Name, s.Status.ReadyReplicas, derefI32(s.Spec.Replicas))}, nil
}

// get_daemonset。
type daemonSetTool struct{ base }

func (t *daemonSetTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	d, err := t.reader.GetDaemonSet(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	m := workloadBase("DaemonSet", d.Namespace, d.Name, nil, d.Status.NumberReady, d.Status.UpdatedNumberScheduled, d.Status.NumberAvailable)
	describeWorkloadRevision(m, d.ObjectMeta, d.Status.ObservedGeneration)
	delete(m, "replicas")
	m["desiredNumberScheduled"] = d.Status.DesiredNumberScheduled
	m["containers"] = containerSpecs(d.Spec.Template.Spec.Containers)
	return tools.ToolResult{Data: m, Text: fmt.Sprintf("daemonset %s/%s ready=%d/%d", d.Namespace, d.Name, d.Status.NumberReady, d.Status.DesiredNumberScheduled)}, nil
}

// get_replicaset。
type replicaSetTool struct{ base }

func (t *replicaSetTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	r, err := t.reader.GetReplicaSet(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	m := workloadBase("ReplicaSet", r.Namespace, r.Name, r.Spec.Replicas, r.Status.ReadyReplicas, r.Status.AvailableReplicas, r.Status.AvailableReplicas)
	describeWorkloadRevision(m, r.ObjectMeta, r.Status.ObservedGeneration)
	delete(m, "updatedReplicas")
	m["containers"] = containerSpecs(r.Spec.Template.Spec.Containers)
	return tools.ToolResult{Data: m, Text: fmt.Sprintf("replicaset %s/%s ready=%d/%d", r.Namespace, r.Name, r.Status.ReadyReplicas, derefI32(r.Spec.Replicas))}, nil
}

func describeWorkloadRevision(data map[string]any, meta metav1.ObjectMeta, observedGeneration int64) {
	data["uid"] = meta.UID
	data["resourceVersion"] = meta.ResourceVersion
	data["generation"] = meta.Generation
	data["observedGeneration"] = observedGeneration
	data["currentGenerationObserved"] = meta.Generation > 0 && observedGeneration >= meta.Generation
	data["ownerReferences"] = meta.OwnerReferences
}
