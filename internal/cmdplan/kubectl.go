package cmdplan

import (
	"regexp"
	"strings"
)

// readOnlyVerbs 与 approval/executor 的只读动词表保持一致：
// 分类、范围解析与执行支持必须一致，不能只为放行管道新增不受范围约束的命令。
var readOnlyVerbs = map[string]bool{
	"get": true, "describe": true, "logs": true, "events": true,
	"top": true, "explain": true, "api-resources": true, "version": true,
}

// readOnlyRolloutSubs 是 rollout 的只读子命令（history/status）。
var readOnlyRolloutSubs = map[string]bool{"history": true, "status": true}

// writeVerbs 是已知写动词：出现在只读组合里即整体不可自动执行。
var writeVerbs = map[string]bool{
	"delete": true, "apply": true, "create": true, "patch": true,
	"scale": true, "rollout": true, "set": true, "annotate": true, "label": true,
	"exec": true, "drain": true, "cordon": true, "uncordon": true,
	"autoscale": true, "expose": true, "taint": true, "reconcile": true, "certificate": true,
	"edit": true, "attach": true, "port-forward": true, "proxy": true,
	"run": true, "cp": true, "debug": true, "auth": true, "plugin": true,
}

// flagKind 是参数取值的形态。
type flagKind int

const (
	flagBool  flagKind = iota // 不带取值
	flagValue                 // 必须带一个取值
	flagNS                    // -n/--namespace
	flagAllNS                 // -A/--all-namespaces
)

// kubectlFlags 是只读组合允许的 kubectl 参数表。不认识的选项、缩写与参数形式
// 一律不自动放行（回落到人工审批），不用"看起来像只读"来猜安全性。
var kubectlFlags = map[string]flagKind{
	"-n": flagNS, "--namespace": flagNS,
	"-A": flagAllNS, "--all-namespaces": flagAllNS,
	"-o": flagValue, "--output": flagValue,
	"-l": flagValue, "--selector": flagValue,
	"--field-selector": flagValue, "--sort-by": flagValue,
	"--template": flagValue, "--subresource": flagValue,
	"--tail": flagValue, "--since": flagValue, "--since-time": flagValue,
	"--limit-bytes": flagValue, "--max-log-requests": flagValue,
	"-c": flagValue, "--container": flagValue,
	"--for": flagValue, "--types": flagValue, "--chunk-size": flagValue,
	"--timestamps": flagBool, "--previous": flagBool, "-p": flagBool,
	"--no-headers": flagBool, "--show-labels": flagBool,
	"--ignore-not-found": flagBool, "--all-containers": flagBool,
	"--prefix": flagBool, "--recursive": flagBool, "-R": flagBool,
	"--use-protocol-buffers": flagBool, "--containers": flagBool,
}

// flagScopes 限定只在特定动词下成立的参数。
var flagScopes = map[string][]string{
	"-A":                     {"get", "top", "events"},
	"--all-namespaces":       {"get", "top", "events"},
	"--previous":             {"logs"},
	"-p":                     {"logs"},
	"--tail":                 {"logs"},
	"--since":                {"logs"},
	"--since-time":           {"logs"},
	"--limit-bytes":          {"logs"},
	"--max-log-requests":     {"logs"},
	"-c":                     {"logs", "exec"},
	"--container":            {"logs", "exec"},
	"--timestamps":           {"logs"},
	"--prefix":               {"logs"},
	"--for":                  {"events"},
	"--types":                {"events"},
	"--containers":           {"top"},
	"--use-protocol-buffers": {"top"},
	"--recursive":            {"explain"},
	"-R":                     {"explain"},
	"--subresource":          {"get"},
	"--template":             {"get", "events"},
	"--ignore-not-found":     {"get"},
}

// kindPattern 允许简写资源（pods、deploy、pods/name、pods,svc、deployments.apps）。
var kindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.,-]*(\.[a-z0-9-]+)*$`)

// resourceNamePattern 与平台策略里的资源名校验保持一致。
var resourceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// kubectlCall 校验一条 kubectl 调用是否为只读并提取目标。
// 任何写动词、交互式动词、越权覆盖参数或不认识的参数都返回错误
// （unknownKindError 文案会进入审批说明）。
func kubectlCall(argv []string) (Call, error) {
	call := Call{Argv: argv, CallLine: strings.Join(argv, " ")}
	if len(argv) < 2 {
		return call, unsupportedf("kubectl 后缺少动词")
	}
	var positionals []string
	verb := ""
	i := 1
	for i < len(argv) {
		tok := argv[i]
		if strings.HasPrefix(tok, "-") && tok != "-" {
			next, err := applyFlag(argv, i, &call, verb)
			if err != nil {
				return call, err
			}
			i = next
			continue
		}
		if verb == "" {
			parsed, ok := parseVerb(tok)
			if !ok {
				return call, unsupportedf("kubectl 动词 %q 不在只读自动执行集合内", tok)
			}
			verb = parsed
			call.Verb = parsed
			i++
			continue
		}
		positionals = append(positionals, tok)
		i++
	}
	if verb == "" {
		return call, unsupportedf("kubectl 命令缺少动词")
	}
	if verb == "rollout" {
		if len(positionals) == 0 {
			return call, unsupportedf("rollout 缺少子命令")
		}
		sub := positionals[0]
		if !readOnlyRolloutSubs[sub] {
			return call, unsupportedf("rollout %s 不是只读子命令", sub)
		}
		call.Subcommand = sub
		positionals = positionals[1:]
	}
	call.Action = ActionRead
	if verb == "logs" {
		call.Action = ActionLogs
	}
	if err := applyResource(&call, positionals); err != nil {
		return call, err
	}
	return call, nil
}

// parseVerb 只接受只读动词；写动词与交互式动词返回 false。
func parseVerb(tok string) (string, bool) {
	if readOnlyVerbs[tok] {
		return tok, true
	}
	if tok == "rollout" {
		return "rollout", true
	}
	return "", false
}

// applyFlag 消费一个参数并返回下一个待处理下标。
func applyFlag(argv []string, i int, call *Call, verb string) (int, error) {
	tok := argv[i]
	name, value, hasValue := strings.Cut(tok, "=")
	kind, ok := kubectlFlags[name]
	if !ok {
		return i, unsupportedf("kubectl 参数 %q 不在只读自动执行集合内", name)
	}
	if scopes, limit := flagScopes[name]; limit {
		allowed := false
		for _, v := range scopes {
			if v == verb {
				allowed = true
				break
			}
		}
		if !allowed {
			return i, unsupportedf("kubectl 参数 %q 不适用于动词 %q", name, verb)
		}
	}
	if kind == flagBool {
		if hasValue {
			return i, unsupportedf("布尔参数 %q 不接受 = 取值", name)
		}
		return i + 1, nil
	}
	if kind == flagAllNS {
		if hasValue {
			return i, unsupportedf("参数 %q 不接受 = 取值", name)
		}
		if call.AllNamespaces || call.Namespace != "" {
			return i, unsupportedf("namespace 只能指定一次")
		}
		call.AllNamespaces = true
		return i + 1, nil
	}
	if !hasValue {
		i++
		if i >= len(argv) {
			return i, unsupportedf("kubectl 参数 %q 缺少取值", name)
		}
		value = argv[i]
	}
	if value == "" {
		return i, unsupportedf("kubectl 参数 %q 的取值为空", name)
	}
	switch kind {
	case flagNS:
		if call.Namespace != "" || call.AllNamespaces {
			return i, unsupportedf("namespace 只能指定一次")
		}
		if !resourceNamePattern.MatchString(value) {
			return i, unsupportedf("namespace %q 不是合法名称", value)
		}
		call.Namespace = value
	case flagAllNS:
		if call.AllNamespaces || call.Namespace != "" {
			return i, unsupportedf("namespace 只能指定一次")
		}
		call.AllNamespaces = true
	case flagValue:
		if name == "-o" || name == "--output" {
			if !validOutput(value) {
				return i, unsupportedf("kubectl 输出格式 %q 不在只读自动执行集合内", value)
			}
		}
	}
	return i + 1, nil
}

// validOutput 限制 -o 取值形态：只允许内置格式与内联模板/JSONPath。
// go-template-file / custom-columns-file 会读取本地文件，一律不放行。
func validOutput(value string) bool {
	switch value {
	case "json", "yaml", "wide", "name", "jsonpath", "go-template":
		return true
	}
	if strings.HasPrefix(value, "jsonpath=") || strings.HasPrefix(value, "jsonpath-as-json=") ||
		strings.HasPrefix(value, "go-template=") {
		return true
	}
	if strings.HasPrefix(value, "custom-columns=") {
		return true
	}
	return false
}

// applyResource 校验位置参数并提取 Kind/Name。
func applyResource(call *Call, positionals []string) error {
	switch call.Verb {
	case "version", "api-resources":
		if len(positionals) != 0 {
			return unsupportedf("%s 不接受位置参数", call.Verb)
		}
		return nil
	case "logs":
		if len(positionals) != 1 {
			return unsupportedf("logs 需要且只需要一个 Pod 目标")
		}
	case "events":
		if len(positionals) > 2 {
			return unsupportedf("events 位置参数过多")
		}
	case "rollout":
		if len(positionals) != 1 {
			return unsupportedf("rollout %s 需要且只需要一个资源目标", call.Subcommand)
		}
	default:
		if len(positionals) < 1 || len(positionals) > 2 {
			return unsupportedf("%s 需要 1 到 2 个位置参数", call.Verb)
		}
	}
	if len(positionals) == 0 {
		return nil
	}
	kind := strings.ToLower(positionals[0])
	name := ""
	if idx := strings.Index(kind, "/"); idx >= 0 {
		name = kind[idx+1:]
		kind = kind[:idx]
	} else if len(positionals) == 2 {
		name = positionals[1]
	} else if call.Verb == "logs" {
		// kubectl logs <pod> 省略资源类型时目标就是 Pod。
		kind, name = "pod", kind
	}
	if !kindPattern.MatchString(kind) {
		return unsupportedf("资源 %q 不是合法资源名", kind)
	}
	if name != "" && !resourceNamePattern.MatchString(name) {
		return unsupportedf("资源名 %q 不是合法名称", name)
	}
	if call.Verb == "logs" && kind != "pod" && kind != "pods" {
		return unsupportedf("logs 只支持 Pod 目标")
	}
	call.Kind = kind
	call.Name = name
	return nil
}
