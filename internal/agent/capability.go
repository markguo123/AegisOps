package agent

import "github.com/k8s-ai/k8s-ai/internal/tools"

func (*questionTool) Capability() tools.Capability { return tools.Capability{Domain: "local"} }
func (*resultCache) Capability() tools.Capability  { return tools.Capability{Domain: "local"} }
