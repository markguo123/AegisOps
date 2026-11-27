package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_pvc。
type pvcTool struct{ base }

func (t *pvcTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	p, err := t.reader.GetPersistentVolumeClaim(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	data := summarizePVC(p)
	if p.Spec.VolumeName != "" {
		data["bindingBasis"] = "PVC.spec.volumeName"
		volume, err := t.reader.GetPersistentVolume(ctx, p.Spec.VolumeName)
		if err != nil {
			data["partial"] = true
			data["relatedVolumeError"] = readError(err).ErrorCode
		} else {
			data["relatedVolume"] = map[string]any{"kind": "PersistentVolume", "name": volume.Name, "uid": volume.UID, "resourceVersion": volume.ResourceVersion}
			ref := volume.Spec.ClaimRef
			data["reciprocalBindingConfirmed"] = p.UID != "" && ref != nil && ref.UID == p.UID && ref.Namespace == p.Namespace && ref.Name == p.Name
		}
	}
	return tools.ToolResult{Data: data, Text: fmt.Sprintf("pvc %s/%s phase=%s", p.Namespace, p.Name, p.Status.Phase)}, nil
}

// get_pv。
type pvTool struct{ base }

func (t *pvTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	p, err := t.reader.GetPersistentVolume(ctx, args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	data := summarizePV(p)
	if ref := p.Spec.ClaimRef; ref != nil {
		data["bindingBasis"] = "PV.spec.claimRef"
		data["claim"] = map[string]any{"kind": "PersistentVolumeClaim", "namespace": ref.Namespace, "name": ref.Name, "uid": ref.UID}
		if ref.Namespace != "" && ref.Name != "" {
			claim, err := t.reader.GetPersistentVolumeClaim(ctx, ref.Namespace, ref.Name)
			if err != nil {
				data["partial"] = true
				data["relatedClaimError"] = readError(err).ErrorCode
			} else {
				data["relatedClaim"] = map[string]any{"kind": "PersistentVolumeClaim", "namespace": claim.Namespace, "name": claim.Name, "uid": claim.UID, "resourceVersion": claim.ResourceVersion}
				data["reciprocalBindingConfirmed"] = ref.UID != "" && ref.UID == claim.UID && claim.Spec.VolumeName == p.Name
			}
		}
	}
	return tools.ToolResult{Data: data, Text: fmt.Sprintf("pv %s phase=%s", p.Name, p.Status.Phase)}, nil
}

// get_storageclass。
type storageClassTool struct{ base }

func (t *storageClassTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	sc, err := t.reader.GetStorageClass(ctx, args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ToolResult{Data: summarizeStorageClass(sc), Text: fmt.Sprintf("storageclass %s provisioner=%s", sc.Name, sc.Provisioner)}, nil
}
