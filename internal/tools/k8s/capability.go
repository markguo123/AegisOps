package k8s

import "github.com/k8s-ai/k8s-ai/internal/tools"

func readCapability(resource string) tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: resource, Action: "read"}
}
func (*eventsTool) Capability() tools.Capability    { return readCapability("/events") }
func (*nodeUsageTool) Capability() tools.Capability { return readCapability("metrics.k8s.io/nodes") }
func (*logsTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: "/pods", Action: "logs"}
}
func (*secretInfoTool) Capability() tools.Capability { return readCapability("/secrets") }
func (*secretValueTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: "/secrets", Action: "secret.read"}
}
