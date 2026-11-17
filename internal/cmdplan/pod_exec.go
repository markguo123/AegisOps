package cmdplan

// Diagnostic policy borrows the structured argv boundary and negative-case
// categories from robusta-dev/holmes-mcp-integrations (kubernetes-remediation).
// It is implemented against AegisOps' per-call human approval contract.
import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/syntax"
)

const PodExecPolicyVersion = "pod-diagnostics-v1"

type PodExecRequest struct {
	Namespace      string   `json:"namespace"`
	Pod            string   `json:"pod"`
	Container      string   `json:"container,omitempty"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

var execIdentifier = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func DecodePodExec(raw []byte) (PodExecRequest, error) {
	var r PodExecRequest
	if len(raw) > 8192 {
		return r, fmt.Errorf("INVALID_ARGUMENT: exec request exceeds 8KiB")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, fmt.Errorf("INVALID_ARGUMENT: %v", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return r, fmt.Errorf("INVALID_ARGUMENT: expected one exec request")
	}
	return NormalizePodExec(r)
}

func NormalizePodExec(r PodExecRequest) (PodExecRequest, error) {
	for _, v := range []string{r.Namespace, r.Pod} {
		if len(v) > 253 || !execIdentifier.MatchString(v) {
			return r, fmt.Errorf("INVALID_ARGUMENT: explicit namespace and Pod name required")
		}
	}
	if r.Container != "" && (len(r.Container) > 63 || !execIdentifier.MatchString(r.Container)) {
		return r, fmt.Errorf("INVALID_ARGUMENT: invalid container")
	}
	if r.TimeoutSeconds < 0 || r.TimeoutSeconds > 30 {
		return r, fmt.Errorf("INVALID_ARGUMENT: timeout must be 1..30 seconds")
	}
	if len(r.Argv) == 0 || len(r.Argv) > 64 {
		return r, fmt.Errorf("INVALID_ARGUMENT: exec argv must contain 1..64 items")
	}
	r.Argv = append([]string(nil), r.Argv...)
	total := 0
	for _, v := range r.Argv {
		total += len(v)
		if len(v) > 2048 || strings.IndexFunc(v, unicode.IsControl) >= 0 || strings.ContainsAny(v, "|&;<>$`\\*?[]{}()") {
			return r, fmt.Errorf("POLICY_DENIED: unsupported exec argument construct")
		}
	}
	if total > 8192 {
		return r, fmt.Errorf("INVALID_ARGUMENT: exec argv exceeds 8KiB")
	}
	args := r.Argv[1:]
	ok := false
	switch r.Argv[0] {
	case "hostname", "uptime", "id", "pwd":
		ok = len(args) == 0
	case "ps":
		ok = len(args) == 0 || strings.Join(args, " ") == "-e -o pid,ppid,comm"
		if ok {
			r.Argv = []string{"ps", "-e", "-o", "pid,ppid,comm"}
		}
	case "ss":
		ok = len(args) == 1 && (args[0] == "-s" || args[0] == "-lnt" || args[0] == "-lnu")
	case "netstat":
		ok = len(args) == 1 && (args[0] == "-lnt" || args[0] == "-lnu")
	case "ls", "stat", "df", "du":
		flags := map[string]bool{}
		paths := 0
		end := false
		ok = true
		for _, a := range args {
			if a == "--" && !end {
				end = true
				continue
			}
			if strings.HasPrefix(a, "-") && !end {
				allowed := a == "-h" || a == "-k"
				switch r.Argv[0] {
				case "ls":
					allowed = a == "-a" || a == "-l" || a == "-h"
				case "stat":
					allowed = a == "-L"
				case "du":
					allowed = allowed || a == "-s"
				}
				if !allowed || flags[a] {
					ok = false
				}
				flags[a] = true
				continue
			}
			paths++
			if err := ValidateDiagnosticPath(a); err != nil {
				return r, err
			}
		}
		ok = ok && paths <= 1 && !(flags["-h"] && flags["-k"])
		if r.Argv[0] != "df" {
			ok = ok && paths == 1
		}
		if r.Argv[0] == "du" {
			ok = ok && flags["-s"]
		}
	default:
		return r, fmt.Errorf("UNSUPPORTED: EXEC_COMMAND_UNSUPPORTED: use a supported Pod diagnostic; file reads and interpreters are unavailable")
	}
	if !ok {
		return r, fmt.Errorf("UNSUPPORTED: EXEC_ARGUMENT_UNSUPPORTED: arguments do not match %s diagnostic rule", r.Argv[0])
	}
	return r, nil
}

func ValidateDiagnosticPath(p string) error {
	if !path.IsAbs(p) || strings.Contains(p, "\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return fmt.Errorf("INVALID_ARGUMENT: diagnostic path must be absolute")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("POLICY_DENIED: path traversal")
		}
	}
	clean := path.Clean(p)
	lower := strings.ToLower(clean)
	for _, root := range []string{"/proc", "/sys", "/dev", "/var/run/secrets", "/run/secrets", "/var/secrets", "/etc/secrets", "/vault/secrets"} {
		if PathUnder(lower, root) {
			return fmt.Errorf("POLICY_DENIED: SENSITIVE_PATH")
		}
	}
	for _, part := range strings.Split(lower, "/") {
		if part == ".kube" || part == ".ssh" || part == ".aws" || part == "gcloud" || part == ".env" || strings.Contains(part, "credential") || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") || strings.HasPrefix(part, "id_rsa") {
			return fmt.Errorf("POLICY_DENIED: SENSITIVE_PATH")
		}
	}
	return nil
}

func PathUnder(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimRight(root, "/")+"/")
}

// ParsePodExec uses the existing Bash AST, but accepts only one literal argv.
// matched reports an exec-shaped request even when invalid, preventing fallback.
func ParsePodExec(command string) (r PodExecRequest, matched bool, err error) {
	if len(command) > 8192 {
		return r, false, nil
	}
	f, e := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if e != nil || len(f.Stmts) != 1 {
		return r, false, nil
	}
	st := f.Stmts[0]
	call, ok := st.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return r, false, nil
	}
	var a []string
	for _, w := range call.Args {
		v, ok := literalWord(w, false)
		if !ok {
			return r, false, nil
		}
		a = append(a, v)
	}
	if a[0] != "kubectl" {
		return r, false, nil
	}
	for i := 1; i < len(a); i++ {
		v := a[i]
		if v == "-n" || v == "--namespace" {
			i++
			continue
		}
		if strings.HasPrefix(v, "--namespace=") {
			continue
		}
		matched = v == "exec"
		break
	}
	if !matched {
		return r, false, nil
	}
	bad := func() (PodExecRequest, bool, error) {
		return r, true, fmt.Errorf("UNSUPPORTED: exec requires a single literal kubectl exec POD -n NS [-c CONTAINER] -- COMMAND")
	}
	if len(st.Redirs) > 0 || st.Background || st.Negated || len(call.Assigns) > 0 {
		return bad()
	}
	seenVerb := false
	seenNS := false
	seenC := false
	for i := 1; i < len(a); i++ {
		v := a[i]
		if v == "exec" && !seenVerb {
			seenVerb = true
			continue
		}
		if v == "--" {
			if !seenVerb || r.Pod == "" {
				return bad()
			}
			r.Argv = a[i+1:]
			normalized, e := NormalizePodExec(r)
			return normalized, true, e
		}
		k, val, eq := strings.Cut(v, "=")
		if k == "-n" || k == "--namespace" || k == "-c" || k == "--container" {
			if !eq {
				i++
				if i >= len(a) {
					return bad()
				}
				val = a[i]
			}
			if k == "-n" || k == "--namespace" {
				if seenNS {
					return bad()
				}
				seenNS = true
				r.Namespace = val
			} else {
				if seenC {
					return bad()
				}
				seenC = true
				r.Container = val
			}
			continue
		}
		if strings.HasPrefix(v, "-") || !seenVerb || r.Pod != "" {
			return bad()
		}
		r.Pod = v
		if prefix, name, slash := strings.Cut(v, "/"); slash {
			if (prefix != "pod" && prefix != "pods") || strings.Contains(name, "/") {
				return bad()
			}
			r.Pod = name
		}
	}
	return bad()
}
