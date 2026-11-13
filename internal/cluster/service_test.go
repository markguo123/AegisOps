package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

// parseConnector 复用真实 ParseKubeconfig 做连测（测试隔离点内仍按真实解析口径校验）。
type parseConnector struct {
	version  string
	health   string
	identity string
	err      error
	calls    int
}

func (p *parseConnector) Inspect(ctx context.Context, data []byte) (*Info, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	info, err := ParseKubeconfig(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	info.Version = p.version
	info.Health = p.health
	info.Identity = p.identity
	return info, nil
}

// kubeconfigFor 构造最小可解析 kubeconfig。token 是凭据哨兵值：不加密落库、
// 不回读、不进审计时即视为泄漏。
func kubeconfigFor(server, token string) string {
	ctxName := "test-ctx"
	if server == "" {
		server = "https://10.0.0.1:6443"
	}
	if token == "" {
		token = "SECRET-TOKEN-default"
	}
	return "apiVersion: v1\n" +
		"kind: Config\n" +
		"clusters:\n" +
		"- name: c1\n" +
		"  cluster:\n" +
		"    server: " + server + "\n" +
		"contexts:\n" +
		"- name: " + ctxName + "\n" +
		"  context:\n" +
		"    cluster: c1\n" +
		"    user: u1\n" +
		"current-context: " + ctxName + "\n" +
		"users:\n" +
		"- name: u1\n" +
		"  user:\n" +
		"    token: " + token + "\n"
}

func newTestService(t *testing.T, p *parseConnector) (*Service, store.Store, *[]store.AuditEvent) {
	t.Helper()
	st, err := store.OpenSQLite("file:cluster-svc-" + time.Now().Format("150405.000000000") + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	base := time.Now().UTC()
	var events []store.AuditEvent
	svc := New(st.Clusters(), st.Credentials(), mustCipher(t),
		WithConnector(p),
		WithClock(func() time.Time { base = base.Add(time.Second); return base }),
		WithAudit(func(ctx context.Context, e store.AuditEvent) error {
			events = append(events, e)
			return nil
		}))
	return svc, st, &events
}

func mustCipher(t *testing.T) *Cipher {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	ci, err := NewCipher(key)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return ci
}

func TestServiceCreateListGetDelete(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", health: "ok"}
	svc, st, events := newTestService(t, p)

	c, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "SECRET-TOKEN-9f2b")})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.ID == "" || c.Status != string(StatusConnected) || c.Server == "" {
		t.Fatalf("Create 元数据不完整: %+v", c)
	}

	// 落库的凭据必须是密文：库里不得出现明文 kubeconfig。
	cred, err := st.Credentials().GetByCluster(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("GetByCluster: %v", err)
	}
	if strings.Contains(cred.KubeconfigSecret, "SECRET-TOKEN-9f2b") {
		t.Fatal("凭据落库不应包含明文 kubeconfig")
	}

	// List/Get 只回元数据，不暴露凭据对象字段。
	list, err := svc.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v, %d", err, len(list))
	}
	got, err := svc.Get(context.Background(), c.ID)
	if err != nil || got.ID != c.ID {
		t.Fatalf("Get: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%+v", got), "Kubeconfig") {
		t.Fatal("Get 不应暴露凭据对象字段")
	}

	// 审计事件只含集群标识与 server，不含 kubeconfig 明文凭据。
	if len(*events) == 0 {
		t.Fatal("应写入 CLUSTER_CHANGE 审计")
	}
	for _, ev := range *events {
		if ev.EventType != store.AuditEventClusterChange {
			continue
		}
		if strings.Contains(ev.ResultSummary+ev.ParameterSummary, "SECRET-TOKEN-9f2b") {
			t.Fatal("审计不得携带 kubeconfig 明文凭据")
		}
	}

	// Delete 同时移除集群元数据与加密凭据。
	if err := svc.Delete(context.Background(), c.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(context.Background(), c.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get 应 ErrNotFound, got %v", err)
	}
	if _, err := st.Credentials().GetByCluster(context.Background(), c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("删除后凭据应一并移除, got %v", err)
	}
}

func TestServiceCreateDuplicate(t *testing.T) {
	svc, _, _ := newTestService(t, &parseConnector{version: "v1.30.0"})
	req := CreateRequest{Name: "cluster-a", Kubeconfig: kubeconfigFor("", "")}
	if _, err := svc.Create(context.Background(), req); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := svc.Create(context.Background(), req); !errors.Is(err, ErrExists) {
		t.Fatalf("重复创建应 ErrExists, got %v", err)
	}
}

func TestServiceCreateInvalid(t *testing.T) {
	svc, _, _ := newTestService(t, &parseConnector{err: fmt.Errorf("%w: connect failed", ErrInvalid)})
	if _, err := svc.Create(context.Background(), CreateRequest{Kubeconfig: kubeconfigFor("", "")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("连接失败应 ErrInvalid, got %v", err)
	}
	if _, err := svc.Create(context.Background(), CreateRequest{Kubeconfig: "   "}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空 kubeconfig 应 ErrInvalid, got %v", err)
	}
}

func TestServiceTestDoesNotPersist(t *testing.T) {
	svc, st, _ := newTestService(t, &parseConnector{version: "v1.29.2", health: "ok"})
	info, err := svc.Test(context.Background(), TestRequest{Kubeconfig: kubeconfigFor("", "")})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if info.Version != "v1.29.2" || info.Health != "ok" || info.Context == "" {
		t.Fatalf("Test 返回值不完整: %+v", info)
	}
	list, err := st.Clusters().List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("Test 不应落库, got %d", len(list))
	}
}

// TestServiceCurrentPicksLatest 验证 GET /clusters/current 取最近创建的受管集群。
func TestServiceCurrentPicksLatest(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", health: "ok"}
	svc, _, _ := newTestService(t, p)
	first, err := svc.Create(context.Background(), CreateRequest{Name: "first", Kubeconfig: kubeconfigFor("https://10.0.0.2:6443", "")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Create(context.Background(), CreateRequest{Name: "second", Kubeconfig: kubeconfigFor("https://10.0.0.3:6443", "")})
	if err != nil {
		t.Fatal(err)
	}
	view, err := svc.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if view.Cluster.ID == first.ID {
		t.Fatal("Current 应取最近创建的集群，而不是旧集群")
	}
	if view.Cluster.ID != second.ID || view.Info.Server == "" {
		t.Fatalf("Current 状态不符: %+v", view)
	}
}

func TestServiceRefreshDisconnected(t *testing.T) {
	p := &parseConnector{version: "v1.30.0"}
	svc, _, _ := newTestService(t, p)
	c, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "")})
	if err != nil {
		t.Fatal(err)
	}
	p.err = fmt.Errorf("%w: connect failed: dial tcp", ErrInvalid)
	view, err := svc.Refresh(context.Background(), c.ID)
	if err != nil {
		t.Fatalf("Refresh 连测失败不应视为内部错误, got %v", err)
	}
	if view.Cluster.Status != string(StatusDisconnected) || view.Info.Health != "down" {
		t.Fatalf("刷新失败应置 disconnected/down: %+v", view)
	}
}

func TestServiceGetNotFound(t *testing.T) {
	svc, _, _ := newTestService(t, &parseConnector{})
	if _, err := svc.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未知 id 应 ErrNotFound, got %v", err)
	}
	if _, err := svc.Get(context.Background(), " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("空 id 应 ErrInvalid, got %v", err)
	}
}

// TestServiceUpdateCredentialReplacesInPlace 验证 P12.3 主路径：ClusterID 不变，
// 凭据密文被替换，版本变化，元数据与检查时间更新，审计不含明文。
func TestServiceUpdateCredentialReplacesInPlace(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: "identity-a"}
	svc, st, events := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("https://a.example:6443", "OLD-TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	oldCred, err := st.Credentials().GetByCluster(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion, err := svc.ExecutionVersion(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := svc.Update(context.Background(), created.ID, UpdateRequest{
		Kubeconfig: kubeconfigFor("https://a.example:6443", "NEW-TOKEN"),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if view.Cluster.ID != created.ID {
		t.Fatal("原位更新必须保留 ClusterID")
	}
	if view.Cluster.Identity != "identity-a" || view.Cluster.CheckedAt == nil || view.Cluster.CheckSummary == "" {
		t.Fatalf("更新后元数据不完整: %+v", view.Cluster)
	}
	newCred, err := st.Credentials().GetByCluster(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newCred.ID != oldCred.ID {
		t.Fatal("原位更新应保留凭据行 ID")
	}
	if newCred.KubeconfigSecret == oldCred.KubeconfigSecret {
		t.Fatal("凭据密文必须被替换")
	}
	newVersion, err := svc.ExecutionVersion(context.Background(), created.ID)
	if err != nil || newVersion == oldVersion {
		t.Fatalf("凭据版本必须变化以作废旧调用/审批, old=%s new=%s err=%v", oldVersion, newVersion, err)
	}
	for _, ev := range *events {
		if ev.EventType == store.AuditEventClusterChange && strings.Contains(ev.ResultSummary, "NEW-TOKEN") {
			t.Fatal("审计不得携带凭据明文")
		}
	}
}

// TestServiceUpdateRejectsIdentityMismatch 验证候选凭据指向另一集群时拒绝原位
// 替换并保留旧值（不得静默把 B 继承为 A 的 ClusterID）。
func TestServiceUpdateRejectsIdentityMismatch(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: "identity-a"}
	svc, st, _ := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "OLD-TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Credentials().GetByCluster(context.Background(), created.ID)
	p.identity = "identity-b"
	_, err = svc.Update(context.Background(), created.ID, UpdateRequest{Kubeconfig: kubeconfigFor("", "NEW-TOKEN")})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("身份不一致必须 ErrIdentityMismatch, got %v", err)
	}
	after, _ := st.Credentials().GetByCluster(context.Background(), created.ID)
	if after.KubeconfigSecret != before.KubeconfigSecret {
		t.Fatal("拒绝更新时必须保留旧凭据")
	}
}

// TestServiceUpdateRejectsUnknownIdentity 验证身份缺失/不可验证时阻止原位替换，
// 而不是用名称或地址相同来猜。
func TestServiceUpdateRejectsUnknownIdentity(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: ""}
	svc, _, _ := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "OLD-TOKEN")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(context.Background(), created.ID, UpdateRequest{Kubeconfig: kubeconfigFor("", "NEW-TOKEN")})
	if !errors.Is(err, ErrIdentityUnverified) {
		t.Fatalf("身份不可验证必须 ErrIdentityUnverified, got %v", err)
	}
}

// TestServiceUpdateServerChangeNeedsConfirmation 验证 API 地址变化必须显式确认。
func TestServiceUpdateServerChangeNeedsConfirmation(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: "identity-a"}
	svc, _, _ := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("https://old.example:6443", "OLD")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Update(context.Background(), created.ID, UpdateRequest{Kubeconfig: kubeconfigFor("https://new.example:6443", "NEW")})
	if !errors.Is(err, ErrServerChanged) {
		t.Fatalf("地址变化未确认必须 ErrServerChanged, got %v", err)
	}
	view, err := svc.Update(context.Background(), created.ID, UpdateRequest{
		Kubeconfig:          kubeconfigFor("https://new.example:6443", "NEW"),
		ConfirmServerChange: true,
	})
	if err != nil || view.Cluster.Server != "https://new.example:6443" {
		t.Fatalf("确认后应完成替换: %v %+v", err, view)
	}
}

// TestServiceUpdateKeepsOldValueOnCandidateFailure 验证候选连测失败不覆盖旧凭据。
func TestServiceUpdateKeepsOldValueOnCandidateFailure(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: "identity-a"}
	svc, st, _ := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "OLD")})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.Credentials().GetByCluster(context.Background(), created.ID)
	p.err = fmt.Errorf("%w: connect failed", ErrInvalid)
	_, err = svc.Update(context.Background(), created.ID, UpdateRequest{Kubeconfig: kubeconfigFor("", "NEW")})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("候选连测失败应返回 ErrInvalid, got %v", err)
	}
	after, _ := st.Credentials().GetByCluster(context.Background(), created.ID)
	if after.KubeconfigSecret != before.KubeconfigSecret {
		t.Fatal("候选失败必须保留旧凭据")
	}
}

func TestServiceRefreshRecordsCheckTimeAndReason(t *testing.T) {
	p := &parseConnector{version: "v1.30.0", identity: "identity-a"}
	svc, st, _ := newTestService(t, p)
	created, err := svc.Create(context.Background(), CreateRequest{Name: "prod", Kubeconfig: kubeconfigFor("", "OLD")})
	if err != nil {
		t.Fatal(err)
	}
	p.err = fmt.Errorf("%w: dial tcp 10.0.0.1:6443: connect: connection refused", ErrInvalid)
	if _, err := svc.Refresh(context.Background(), created.ID); err != nil {
		t.Fatalf("Refresh 连测失败不应视为内部错误: %v", err)
	}
	got, err := st.Clusters().Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckedAt == nil || got.Status != string(StatusDisconnected) || !strings.Contains(got.CheckSummary, "网络") {
		t.Fatalf("失败连测必须记录时间与分类原因: %+v", got)
	}
}

func TestServiceContextsListsExplicitChoices(t *testing.T) {
	svc, _, _ := newTestService(t, &parseConnector{})
	view, err := svc.Contexts(context.Background(), ContextsRequest{Kubeconfig: kubeconfigFor("", "")})
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if view.Current != "test-ctx" || len(view.Contexts) != 1 || view.Contexts[0] != "test-ctx" {
		t.Fatalf("context 列表不符: %+v", view)
	}
}

type connectorFunc func(context.Context, []byte) (*Info, error)

func (f connectorFunc) Inspect(ctx context.Context, data []byte) (*Info, error) { return f(ctx, data) }

func TestRefreshRejectsStaleCredentialResult(t *testing.T) {
	ctx := context.Background()
	svc, st, _ := newTestService(t, &parseConnector{version: "old", identity: "same"})
	c, err := svc.Create(ctx, CreateRequest{Name: "test", Kubeconfig: kubeconfigFor("https://old.example", "old")})
	if err != nil {
		t.Fatal(err)
	}
	svc.connector = connectorFunc(func(ctx context.Context, _ []byte) (*Info, error) {
		cred, err := st.Credentials().GetByCluster(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		current, err := st.Clusters().Get(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		cred.KubeconfigSecret, err = svc.cipher.Encrypt([]byte(kubeconfigFor("https://new.example", "new")))
		if err != nil {
			return nil, err
		}
		current.Server, current.Context, current.Version = "https://new.example", "new-context", "new-version"
		if err := st.Credentials().(store.ClusterCredentialRotator).RotateClusterCredential(ctx, current, cred); err != nil {
			return nil, err
		}
		return &Info{Server: "https://old.example", Version: "stale-version"}, nil
	})
	if _, err := svc.Refresh(ctx, c.ID); err == nil {
		t.Fatal("stale probe must fail after credential rotation")
	}
	got, err := st.Clusters().Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server != "https://new.example" || got.Context != "new-context" || got.Version != "new-version" {
		t.Fatalf("stale probe overwrote rotated metadata: %+v", got)
	}
}
