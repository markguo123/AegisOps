package sessionapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	kube "github.com/k8s-ai/k8s-ai/internal/kubernetes"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// Explicit user reveal is isolated from the durable Chat/SSE pipeline.
func (s *Server) handleRevealSecret(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	var body struct {
		Namespace string   `json:"namespace"`
		Name      string   `json:"name"`
		Keys      []string `json:"keys"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || body.Namespace == "" || body.Name == "" || len(body.Keys) == 0 || len(body.Keys) > 5 {
		writeError(w, 400, fmt.Errorf("请指定 namespace、Secret 名称和 1–5 个 key"))
		return
	}
	if len(validation.IsDNS1123Label(body.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(body.Name)) != 0 {
		writeError(w, 400, fmt.Errorf("namespace 或 Secret 名称无效"))
		return
	}
	for _, key := range body.Keys {
		if len(validation.IsConfigMapKey(key)) != 0 {
			writeError(w, 400, fmt.Errorf("Secret key 无效"))
			return
		}
	}
	sess, err := s.ownedSession(r, r.PathValue("id"))
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	principal, ok := principalFrom(r)
	if !ok || s.platform == nil || principal.recheck == nil {
		writeError(w, 403, fmt.Errorf("你没有权限读取该 Secret"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var coverage authz.Coverage
	if json.Unmarshal(sess.Coverage, &coverage) != nil || coverage.ClusterID != sess.Cluster {
		writeError(w, 403, fmt.Errorf("你没有权限读取该 Secret"))
		return
	}
	bound, err := s.platform.Bind(ctx, principal.User.ID, coverage, func(ctx context.Context) error {
		user, role, e := principal.recheck(ctx)
		if e != nil || user == nil || role == nil || user.ID != principal.User.ID {
			return fmt.Errorf("authentication expired")
		}
		return nil
	})
	if err != nil {
		writeError(w, 403, fmt.Errorf("你没有权限读取该 Secret"))
		return
	}
	defer bound.Close()
	target := authz.Target{ClusterID: sess.Cluster, Namespace: body.Namespace, Resource: "secrets", Name: body.Name, Actions: []string{authz.ActionSecretRead}}
	authorize := func() error {
		access, e := bound.Access(ctx)
		if e != nil {
			return e
		}
		return access.Check(target)
	}
	if authorize() != nil {
		writeError(w, 403, fmt.Errorf("你没有权限读取 namespace %s 下的 Secret", body.Namespace))
		return
	}
	// Audit only the target and requested keys. Never pass values or Kubernetes errors.
	payload, _ := json.Marshal(map[string]any{"namespace": body.Namespace, "name": body.Name, "keys": body.Keys, "purpose": "user_reveal"})
	audit := func(status string) error {
		if s.audit == nil {
			return fmt.Errorf("audit unavailable")
		}
		return s.audit(ctx, store.AuditEvent{Action: "secret.reveal", EventType: store.AuditEventApproval, SessionID: sess.ID, UserID: principal.User.ID, ClusterID: sess.Cluster, Status: status, Payload: string(payload)})
	}
	if audit("requested") != nil {
		writeError(w, 503, fmt.Errorf("审计不可用，请稍后重试"))
		return
	}
	client, err := kube.NewClient(kube.Options{Kubeconfig: bound.Path, Timeout: 10 * time.Second})
	if err != nil {
		writeError(w, 503, fmt.Errorf("集群连接不可用"))
		return
	}
	values, err := client.GetSecretValues(ctx, body.Namespace, body.Name, body.Keys)
	if err != nil {
		_ = audit("failed")
		writeError(w, 403, fmt.Errorf("无法读取 Secret，请检查资源、key 与集群 RBAC 权限"))
		return
	}
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	total := 0
	result := map[string]any{}
	for key, value := range values {
		total += len(value)
		if len(value) > 16<<10 || total > 32<<10 {
			writeError(w, 413, fmt.Errorf("Secret 超出单次显示大小限制"))
			return
		}
		encoding, text := "utf8", string(value)
		if !utf8.Valid(value) {
			encoding, text = "base64", base64.StdEncoding.EncodeToString(value)
		}
		result[key] = map[string]string{"value": text, "encoding": encoding}
	}
	if authorize() != nil {
		writeError(w, 403, fmt.Errorf("你的授权已变更，不能显示该 Secret"))
		return
	}
	if audit("success") != nil {
		writeError(w, 503, fmt.Errorf("审计不可用，请稍后重试"))
		return
	}
	writeJSON(w, 200, map[string]any{"values": result})
}
