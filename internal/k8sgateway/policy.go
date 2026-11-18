package k8sgateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func proxyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "message": message, "reason": "Forbidden", "code": status})
}

// Security policy is distinct from the platform resource Authorizer. Creation
// retains the existing proposal/dry-run channel; exec retains its own transport.
func proxyRequestPolicy(req *http.Request, readOnly bool) error {
	path := strings.ToLower(req.URL.Path)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "secrets", "exec", "attach", "portforward", "proxy":
			return fmt.Errorf("POLICY_DENIED: resource requires its dedicated controlled channel")
		}
	}
	if req.Method == http.MethodGet {
		return nil
	}
	if req.Method == http.MethodPost && strings.HasSuffix(path, "/selfsubjectaccessreviews") {
		return nil
	}
	if readOnly {
		return fmt.Errorf("POLICY_DENIED: read-only execution cannot issue mutations")
	}
	switch req.Method {
	case http.MethodPatch, http.MethodPut, http.MethodDelete:
	case http.MethodPost:
		return fmt.Errorf("POLICY_DENIED: creation requires create_resource dry-run and proposal")
	default:
		return fmt.Errorf("POLICY_DENIED: unsupported API method")
	}
	if req.Body != nil && (req.Method == http.MethodPatch || req.Method == http.MethodPut) {
		raw, err := io.ReadAll(io.LimitReader(req.Body, 1024*1024+1))
		req.Body.Close()
		if err != nil || len(raw) > 1024*1024 {
			return fmt.Errorf("POLICY_DENIED: mutation body exceeds limit")
		}
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return fmt.Errorf("POLICY_DENIED: unsupported mutation body")
		}
		if dangerousMutation(value) {
			return fmt.Errorf("POLICY_DENIED: privileged mutation requires a dedicated reviewed workflow")
		}
		req.Body = io.NopCloser(strings.NewReader(string(raw)))
		req.ContentLength = int64(len(raw))
	}
	return nil
}

func dangerousMutation(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			lower := strings.ToLower(k)
			if lower == "path" || lower == "from" {
				if s, ok := v.(string); ok {
					for _, part := range strings.Split(strings.ToLower(s), "/") {
						if dangerousMutationKey(part) {
							return true
						}
					}
				}
			}
			if dangerousMutationKey(lower) || dangerousMutation(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if dangerousMutation(v) {
				return true
			}
		}
	}
	return false
}
func dangerousMutationKey(k string) bool {
	switch k {
	case "privileged", "hostnetwork", "hostpid", "hostipc", "hostpath", "capabilities", "serviceaccount", "serviceaccountname", "automountserviceaccounttoken", "volumes", "securitycontext":
		return true
	}
	return false
}
