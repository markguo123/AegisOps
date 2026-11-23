package k8s

import "github.com/k8s-ai/k8s-ai/internal/tools"

func readCapability(resource string) tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: resource, Action: "read"}
}
func (*podTool) Capability() tools.Capability         { return readCapability("/pods") }
func (*podsTool) Capability() tools.Capability        { return readCapability("/pods") }
func (*nodeTool) Capability() tools.Capability        { return readCapability("/nodes") }
func (*nodesTool) Capability() tools.Capability       { return readCapability("/nodes") }
func (*deploymentTool) Capability() tools.Capability  { return readCapability("apps/deployments") }
func (*statefulSetTool) Capability() tools.Capability { return readCapability("apps/statefulsets") }
func (*daemonSetTool) Capability() tools.Capability   { return readCapability("apps/daemonsets") }
func (*replicaSetTool) Capability() tools.Capability  { return readCapability("apps/replicasets") }
func (*serviceTool) Capability() tools.Capability     { return readCapability("/services") }
func (*endpointsTool) Capability() tools.Capability   { return readCapability("/endpoints") }
func (*endpointSlicesTool) Capability() tools.Capability {
	return readCapability("discovery.k8s.io/endpointslices")
}
func (*pvcTool) Capability() tools.Capability { return readCapability("/persistentvolumeclaims") }
func (*pvTool) Capability() tools.Capability  { return readCapability("/persistentvolumes") }
func (*storageClassTool) Capability() tools.Capability {
	return readCapability("storage.k8s.io/storageclasses")
}
func (*eventsTool) Capability() tools.Capability     { return readCapability("/events") }
func (*configMapTool) Capability() tools.Capability  { return readCapability("/configmaps") }
func (*configMapsTool) Capability() tools.Capability { return readCapability("/configmaps") }
func (*namespaceTool) Capability() tools.Capability  { return readCapability("/namespaces") }
func (*nodeUsageTool) Capability() tools.Capability  { return readCapability("metrics.k8s.io/nodes") }
func (*resourcesTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Action: "read", KindArgument: true}
}
func (*logsTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: "/pods", Action: "logs"}
}
func (*secretInfoTool) Capability() tools.Capability { return readCapability("/secrets") }
func (*secretValueTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: "/secrets", Action: "secret.read"}
}
