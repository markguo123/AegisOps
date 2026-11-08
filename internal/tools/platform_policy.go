package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/manifestx"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes/scheme"
	"regexp"
	"strings"
)

var errUnproven = errors.New("SCOPE_DENIED: cannot prove the complete target; use an explicit supported resource and namespace")

func (s AccessScope) policyCheck(name string, input json.RawMessage) error {
	if localTools[name] {
		return nil
	}
	var args map[string]any
	if json.Unmarshal(input, &args) != nil {
		return errUnproven
	}
	ns := stringArg(args, "namespace")
	cluster := clusterOnlyTools[name]
	action := authz.ActionRead
	switch {
	case name == "kubectl":
		return s.commandPolicy(stringArg(args, "command"))
	case name == "create_resource":
		objs, err := manifestx.Parse(stringArg(args, "manifest"), manifestx.Options{})
		if err != nil || len(objs) == 0 {
			return errUnproven
		}
		return s.checkObjects(objs)
	case name == "scan_cluster":
		if ns == "" {
			if err := s.target("", true, action); err != nil {
				return err
			}
			return s.target("*", false, action)
		}
	case cluster:
	case name == "get_resources":
		cluster = clusterKinds[strings.ToLower(stringArg(args, "kind"))]
	case name == "get_events" || namespaceTools[name]:
		if name == "get_logs" || name == "get_previous_logs" {
			action = authz.ActionLogs
		}
		if name == "get_secret_value" {
			action = authz.ActionSecretRead
		}
	default:
		return errUnproven
	}
	if ns == "" {
		ns = "*"
	}
	return s.target(ns, cluster, action)
}
func (s AccessScope) target(ns string, cluster bool, act string) error {
	return s.Policy.Check(authz.Target{ClusterID: s.Policy.ClusterID, Namespace: ns, ClusterScoped: cluster, Actions: []string{act}})
}

var resourceName = regexp.MustCompile("^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$")

func (s AccessScope) commandPolicy(cmd string) error {
	pc, err := executor.ParseCommandDetail(cmd)
	if err != nil {
		return errUnproven
	}
	if pc.IsShell {
		if err := s.target("", true, authz.ActionShell); err != nil {
			return err
		}
		return s.shellPolicy(cmd)
	}
	return s.argvPolicy(pc.Argv)
}
func (s AccessScope) argvPolicy(a []string) error {
	if len(a) < 3 {
		return errUnproven
	}
	verb := a[1]
	act := authz.ActionRead
	switch verb {
	case "get", "describe", "logs":
	case "scale", "patch", "rollout", "cordon", "uncordon":
		act = authz.ActionOperate
	case "delete":
		act = authz.ActionDestroy
	case "exec":
		act = authz.ActionExec
	default:
		return errUnproven
	}
	var pos []string
	ns := ""
	all := false
	execOK := false
	for i := 2; i < len(a); i++ {
		f := a[i]
		if f == "--" {
			if verb != "exec" || i+1 >= len(a) {
				return errUnproven
			}
			switch a[i+1] {
			case "hostname", "uptime", "id":
				execOK = i+2 == len(a)
			case "ps":
				execOK = i+2 == len(a) || i+3 == len(a) && (a[i+2] == "-ef" || a[i+2] == "aux")
			case "df":
				execOK = i+2 == len(a) || i+3 == len(a) && (a[i+2] == "-h" || a[i+2] == "-k")
			default:
				return errUnproven
			}
			break
		}
		if !strings.HasPrefix(f, "-") {
			pos = append(pos, f)
			continue
		}
		key, val, eq := strings.Cut(f, "=")
		switch key {
		case "-A", "--all-namespaces":
			if eq || all || ns != "" || verb != "get" {
				return errUnproven
			}
			all = true
			ns = "*"
			continue
		case "--previous", "--timestamps", "--no-headers":
			if eq {
				return errUnproven
			}
			continue
		case "-n", "--namespace", "-o", "--output", "--replicas", "-p", "--patch", "--type", "--tail", "--since", "-c", "--container", "-l", "--selector":
		default:
			return errUnproven
		}
		if !eq {
			i++
			if i >= len(a) {
				return errUnproven
			}
			val = a[i]
		}
		switch key {
		case "-n", "--namespace":
			if ns != "" || !resourceName.MatchString(val) {
				return errUnproven
			}
			ns = val
		case "-o", "--output":
			switch val {
			case "json", "yaml", "wide", "name":
			default:
				return errUnproven
			}
		case "-p", "--patch", "--type":
			if verb != "patch" {
				return errUnproven
			}
		case "--replicas":
			if verb != "scale" {
				return errUnproven
			}
		case "-c", "--container", "--tail", "--since":
			if verb != "logs" && verb != "exec" {
				return errUnproven
			}
		}
	}
	if verb == "rollout" {
		if len(pos) < 2 || (pos[0] != "restart" && pos[0] != "undo") {
			return errUnproven
		}
		pos = pos[1:]
	}
	if len(pos) > 2 && verb != "logs" && verb != "exec" {
		for _, name := range pos[1:] {
			if !resourceName.MatchString(name) {
				return errUnproven
			}
			single := []string{"kubectl", verb, pos[0] + "/" + name}
			if ns != "" {
				single = append(single, "-n", ns)
			}
			if err := s.argvPolicy(single); err != nil {
				return err
			}
		}
		return nil
	}
	if len(pos) == 0 || len(pos) > 2 {
		return errUnproven
	}
	kind, name, slash := strings.Cut(pos[0], "/")
	if verb == "logs" || verb == "exec" {
		if !slash {
			kind = "pod"
			name = pos[0]
		}
		if kind != "pod" && kind != "pods" {
			return errUnproven
		}
		if len(pos) != 1 || !resourceName.MatchString(name) {
			return errUnproven
		}
		if verb == "exec" && !execOK {
			return errUnproven
		}
		if verb == "logs" {
			act = authz.ActionLogs
		}
	} else if verb == "cordon" || verb == "uncordon" {
		if len(pos) != 1 || !resourceName.MatchString(pos[0]) {
			return errUnproven
		}
		kind = "node"
		name = pos[0]
	} else {
		if slash && (len(pos) != 1 || !resourceName.MatchString(name)) {
			return errUnproven
		}
		if len(pos) == 2 && !resourceName.MatchString(pos[1]) {
			return errUnproven
		}
		if !slash && len(pos) == 1 && verb != "get" && verb != "describe" {
			return errUnproven
		}
	}
	if strings.Contains(kind, ",") {
		if len(pos) != 1 || verb != "get" {
			return errUnproven
		}
		for _, item := range strings.Split(kind, ",") {
			single := []string{"kubectl", "get", item}
			if ns != "" {
				if ns == "*" {
					single = append(single, "-A")
				} else {
					single = append(single, "-n", ns)
				}
			}
			if err := s.argvPolicy(single); err != nil {
				return err
			}
		}
		return nil
	}
	g, res, ok := kubectlResourceForKind(kind)
	if kind == "node" || kind == "nodes" {
		g, res, ok = "", "nodes", true
	}
	if !ok {
		return errUnproven
	}
	cluster, ok := authz.ProfileResource(g, res, act)
	if !ok || res == "secrets" {
		return errUnproven
	}
	if cluster && verb == "describe" {
		return errUnproven
	}
	if cluster && ns != "" || !cluster && ns == "" {
		return fmt.Errorf("%w: explicit target required", errUnproven)
	}
	if len(pos) == 2 {
		name = pos[1]
	}
	return s.Policy.Check(authz.Target{ClusterID: s.Policy.ClusterID, Namespace: ns, ClusterScoped: cluster, Actions: []string{act}, Resource: g + "/" + res, Name: name})
}

func (s AccessScope) checkObjects(objs []manifestx.Object) error {
	if len(objs) == 0 {
		return errUnproven
	}
	for _, obj := range objs {
		n := obj.Namespace
		if n == "" {
			n = "default"
		}
		gvr := obj.GVR
		if !obj.Mapped {
			if !scheme.Scheme.Recognizes(obj.GVK) {
				return errUnproven
			}
			gvr, _ = meta.UnsafeGuessKindToResource(obj.GVK)
		}
		cl, ok := authz.ProfileResource(gvr.Group, gvr.Resource, authz.ActionOperate)
		if !ok {
			return errUnproven
		}
		if err := s.Policy.Check(authz.Target{ClusterID: s.Policy.ClusterID, Namespace: n, ClusterScoped: cl, Resource: gvr.Group + "/" + gvr.Resource, Name: obj.Name, Actions: []string{authz.ActionOperate}}); err != nil {
			return err
		}
	}
	return nil
}
