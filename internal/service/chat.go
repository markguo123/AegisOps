package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/agent"
	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/audit"
	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/decision"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/kubernetes"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/manifestx"
	"github.com/k8s-ai/k8s-ai/internal/mcp"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/scanner"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/skills"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"github.com/k8s-ai/k8s-ai/internal/tools"
	k8stools "github.com/k8s-ai/k8s-ai/internal/tools/k8s"
)

// ChatEventType 是 ChatService 事件类型（CLI/Web 消费，ADR-007 的 Agent 版）。
type ChatEventType string

const (
	ChatEventState          ChatEventType = "state"
	ChatEventText           ChatEventType = "text"            // 用户输入回显（CLI 交互模式）
	ChatEventDelta          ChatEventType = "delta"           // 兼容旧客户端的 LLM 增量事件
	ChatEventReasoningDelta ChatEventType = "reasoning-delta" // 模型隐藏思考增量（实时推给 Web“分析过程”面板展示）
	ChatEventProgress       ChatEventType = "progress"        // 阶段进度摘要（可见输出的简短说明）
	ChatEventAnswerDelta    ChatEventType = "answer-delta"    // 最终回答增量
	ChatEventToolCall       ChatEventType = "tool-call"
	ChatEventToolResponse   ChatEventType = "tool-response"
	ChatEventAnswer         ChatEventType = "answer" // 最终回答（携带全文，流式气泡以此收尾）
	ChatEventError          ChatEventType = "error"
	ChatEventChoice         ChatEventType = "choice-request"
)

// ChatToolCallView 是工具调用在事件中的展示视图。
type ChatToolCallView struct {
	ResultStatus string         `json:"resultStatus,omitempty"`
	Name         string         `json:"name"`
	Arguments    map[string]any `json:"arguments,omitempty"`
	Risk         string         `json:"risk,omitempty"`
	Result       string         `json:"result,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Ok           *bool          `json:"ok,omitempty"` // nil=旧客户端/旧会话无此字段，前端回退正则兜底
	Truncated    bool           `json:"truncated,omitempty"`
	Data         any            `json:"data,omitempty"` // 结构化结果（仅 UI 回放展示，不参与 LLM 载荷）
	// P7.4：工具结果同源（ADR-034），随 tool-response 一并下发（旧客户端兼容）。
	DurationMs  int64  `json:"durationMs,omitempty"`
	DisplayType string `json:"displayType,omitempty"`
	Command     string `json:"command,omitempty"`
	Stdout      string `json:"stdout,omitempty"`
	Stderr      string `json:"stderr,omitempty"`
	ExitCode    int    `json:"exitCode,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
}

// ChatChoiceOption 是审批/选择项。
type ChatChoiceOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ChatChoice 是审批请求（写操作确认）。
type ChatChoice struct {
	Prompt   string             `json:"prompt"`
	Options  []ChatChoiceOption `json:"options"`
	Approval *sse.Interaction   `json:"approval,omitempty"`
	// Secret 为 true 时该请求要收集 Secret 值：前端用密码框，值不回显、不入会话与审计。
	Secret bool `json:"secret,omitempty"`
}

// ChatEvent 是一轮对话中的增量事件。
type ChatEvent struct {
	Type   ChatEventType     `json:"type"`
	Text   string            `json:"text,omitempty"`
	Tool   *ChatToolCallView `json:"tool,omitempty"`
	Choice *ChatChoice       `json:"choice,omitempty"`
	State  string            `json:"state,omitempty"`
	Usage  *llm.TokenUsage   `json:"usage,omitempty"`
}

// ChatRequest 是一次对话请求。
// Confirmation 是会话 API 的审批票据入口：展示统一 Interaction 外壳并等待答复。
type Confirmation func(context.Context, sse.Interaction) (string, <-chan string, func(), error)

type ChatRequest struct {
	ModelID          string
	Ask              decision.Asker
	ApprovalContext  string
	CoverageSnapshot func() []byte
	Confirmation     Confirmation
	KubeconfigPath   string
	RequestCheck     func(*http.Request) error
	// TargetCheck 在每次 kubectl 子进程执行前重验授权/集群/凭据版本（P12.2）。
	// 为空表示不做运行期重验（CLI 直用/测试）。
	TargetCheck func(context.Context) error
	SessionID   string
	// TurnID 由会话 API 在接收消息时分配（202 响应需要它）；为空时由 Agent 生成。
	TurnID string
	Query  string
	// UserID 是发起本轮的产品内用户，用于 Turn/审计归属。
	UserID string
	// AllowAnySession 仅由具备 chat.manage.all 的管理员请求设置。
	AllowAnySession bool
	// SkipPermissions 非交互自动化显式放行写操作（等效 kubectl-ai --skip-permissions）。
	SkipPermissions bool
	// Skills 用户显式 @ 指定的 Skill 名称（P3.8.4）：非空时不走自动意图检索，
	// 只注入这些已启用的 Skill；任一名字不存在/未启用则整批拒绝。
	Skills []string
	// AccessScope 是当前用户本轮可访问的 Kubernetes 资源范围；nil 仅用于 CLI/测试。
	AccessScope *tools.AccessScope
}

// ChatService 是二期 Agent 应用服务（CLI/Web 复用）。
type ChatService interface {
	// Chat 处理一条用户消息，返回事件流；respond 通道回传审批选择。
	Chat(ctx context.Context, req ChatRequest, respond <-chan string) (<-chan ChatEvent, error)
	// GetOrCreateSession 获取或创建会话。
	GetOrCreateSession(id, namespace, model string) (*session.Session, error)
	// GetSession 只读取已有会话，不自动创建。
	GetSession(id string) (*session.Session, error)
	// GetOrCreateSessionForUser 按登录用户建立会话所有权。
	GetOrCreateSessionForUser(id, userID, namespace, model string) (*session.Session, error)
	ListSessions() ([]session.Session, error)
	// ListSessionsForUser 只返回指定用户的会话。
	ListSessionsForUser(userID string) ([]session.Session, error)
	DeleteSession(id string) error
	ClearSession(id string) error
	RenameSession(id, name string) error
	// PinSession 置顶/取消置顶会话（P7.5）。置顶状态随会话落库，列表置顶优先。
	PinSession(id string, pinned bool) error
}

// SessionTokenizer 是 ChatService 的可选能力（P2.13）：返回会话级 token 用量合计。
// 单独成接口而不是并入 ChatService，是为了让三期实现与其测试桩无需跟着变更；
// 调用方用类型断言探测，未实现时按"未持久化"退化为不展示统计。
type SessionTokenizer interface {
	SessionTokenUsage(ctx context.Context, sessionID string) (session.TokenUsage, error)
}

// SkillPlatformBinder 是 ChatService 的可选能力（P3.8）：把四期 Skill 平台
// （P3.5）的实时 registry 挂到 create_resource 路径。未实现时工具路径回退
// 内置 Skill；Web/CLI 组合根在装配 Store 后调用 SetSkillPlatform 即可让导入与
// 启停即时生效。单独成接口是为了不让既有聊天测试桩背负 Skill 平台依赖。
type SkillPlatformBinder interface {
	SetSkillPlatform(p *SkillPlatform)
}

// RuntimeConfigResolver 解析一轮对话的生效配置（P6.1 / D-29）。实现方按
// "环境变量白名单覆盖 > DB settings > 文件与内置默认"的顺序叠加，并在模型未配置时
// 返回明确错误。
type RuntimeConfigResolver func(ctx context.Context) (*config.Config, error)

// RuntimeConfigBinder 是 ChatService 的可选能力（P6.1）：注入运行时配置解析器后，
// 每轮 Chat 的工具装配、审批策略、LLM 参数与 Agent 限额都按配置中心当前值生效，
// 配置页面改完下一轮立即生效，不需要重启进程。未注入时沿用启动基线（CLI 直用仍走
// config.yaml）。单独成接口是为了不让既有聊天测试桩背负配置中心依赖。
type RuntimeConfigBinder interface {
	SetRuntimeConfigResolver(RuntimeConfigResolver)
}

// ChatResultStoreBinder reuses the platform object gateway for safe tool results.
type ChatResultStoreBinder interface{ SetResultStore(agent.ResultStore) }

type chatService struct {
	diagnosticMemories *DiagnosticMemories
	cfg                *config.Config
	sessions           *session.Manager
	llmFactory         func(llm.Options) llm.LLMClient
	newReader          func() (scanner.Reader, error)
	scanFactory        func() tools.ScanRunner
	newWriteRunner     func() (tools.WriteRunner, error)
	// proposals 跨轮次保存创建提案指纹：每轮 Chat 都会重建工具实例，
	// dry run 与 apply 通常不在同一轮，所以缓存必须挂在服务上。
	proposals *tools.ProposalStore
	// store 是四期 SQLite/MySQL 统一持久化门面：非空时把 Turn/Step 生命周期记录落库（P1.8）。
	store store.Store
	// sessionsInStore 表示会话事实源已是平台库（P8.0 / ADR-046）：会话读写经
	// store.NewDatabaseSessionStore 直接进 sessions 表，fileStore 只留给终端直用
	// （无平台库）模式，因此聊天主流程不再对同一行做第二次写。
	sessionsInStore bool
	// skillsPlat 是四期 Skill 平台应用服务（P3.5），非空时 create_resource 的
	// dry-run / apply 消费其实时 registry（custom/third-party 与启停即时生效）。
	skillsPlat *SkillPlatform
	// sink 非空时把统一 SSE 事件（P1.9 事件路由）实时广播给会话客户端；
	// 三期 ChatEvent 事件流原样保留（D-08：迁移期旧事件继续可消费）。
	// flow 是 Chat 单一输出管道（方案 25）：事件在这里安全处理、微批、原子提交
	// 后才广播；sink 仅作为“提交后广播”钩子存在。
	flow       *chatFlow
	startupErr error
	// resolveCfg 非空时每轮 Chat 用它取生效配置（P6.1，配置中心 + env 覆盖）。
	resolveCfg  RuntimeConfigResolver
	resultStore agent.ResultStore
}

// NewChat 创建二期 Agent 服务。
func NewChat(cfg *config.Config, newReader func() (scanner.Reader, error), llmFactory func(llm.Options) llm.LLMClient, scanFactory func() tools.ScanRunner, newWriteRunner func() (tools.WriteRunner, error)) ChatService {
	return newChat(cfg, newReader, llmFactory, scanFactory, newWriteRunner, nil)
}

func newChat(cfg *config.Config, newReader func() (scanner.Reader, error), llmFactory func(llm.Options) llm.LLMClient, scanFactory func() tools.ScanRunner, newWriteRunner func() (tools.WriteRunner, error), st store.Store) ChatService {
	// 会话事实源分流（P8.0）：装配了平台库就一律读写平台库——容器卷上的会话目录
	// 不作事实源（清卷即丢历史正文；删除只落文件还会在库里留下已删会话的整份正文）。
	var sessStore session.Store
	sessionsInStore := false
	if st != nil {
		sessStore = store.NewDatabaseSessionStore(st.Sessions())
		sessionsInStore = true
		warnLegacySessionFiles(cfg.Agent.SessionDir)
	} else {
		fileStore, err := session.NewFileStore(cfg.Agent.SessionDir)
		if err != nil {
			slog.Warn("chat: 会话目录不可用，回退内存存储", "dir", cfg.Agent.SessionDir, "error", err)
			sessStore = session.NewMemoryStore()
		} else {
			sessStore = fileStore
		}
	}
	if llmFactory == nil {
		llmFactory = func(o llm.Options) llm.LLMClient { return llm.New(o) }
	}
	if scanFactory == nil {
		scanFactory = func() tools.ScanRunner { return New() }
	}
	if newWriteRunner == nil {
		newWriteRunner = func() (tools.WriteRunner, error) { return chatWriteRunner(cfg) }
	}
	svc := &chatService{
		cfg:             cfg,
		sessions:        session.NewManager(sessStore),
		sessionsInStore: sessionsInStore,
		llmFactory:      llmFactory,
		newReader:       newReader,
		scanFactory:     scanFactory,
		newWriteRunner:  newWriteRunner,
		proposals:       tools.NewProposalStore(),
		store:           st,
	}
	if recovery, ok := st.(interface{ RecoverChatRuns(context.Context) error }); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		svc.startupErr = recovery.RecoverChatRuns(ctx)
		cancel()
	}
	svc.ensureFlow()
	return svc
}

// NewChatWithStore 用 Store 持久化创建四期 Agent 服务（P1.8）：非空 store 时把每轮
// Session/Turn 及其 Reason/Tool/Observe/Approval/Response Step 记录入库，与三期文件会话并存。
func NewChatWithStore(cfg *config.Config, newReader func() (scanner.Reader, error), llmFactory func(llm.Options) llm.LLMClient, scanFactory func() tools.ScanRunner, newWriteRunner func() (tools.WriteRunner, error), st store.Store) ChatService {
	return newChat(cfg, newReader, llmFactory, scanFactory, newWriteRunner, st)
}

// warnLegacySessionFiles 平台模式启动时点名会话目录里残留的历史文件（P8.0 / ADR-046）。
// 平台模式下这些文件不再被读取，升级现场如果没人告知，只会看到「历史会话全没了」，
// 容易被当成数据丢失故障；这里明说补救办法（一次性导入）而不是静默忽略。
func warnLegacySessionFiles(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	legacy := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			legacy++
		}
	}
	if legacy > 0 {
		slog.Info("chat: 会话目录里的历史文件不再被读取（平台模式会话事实源是平台库），如需保留历史请执行 aopsctl migrate --platform --sessions-dir",
			"dir", dir, "files", legacy)
	}
}

// NewChatWithSink 创建四期 Agent 服务：保留三期事件流，并通过 sink 广播
// 已提交的 Chat 语义事件（方案 25：提交成功后发布）。
func NewChatWithSink(cfg *config.Config, newReader func() (scanner.Reader, error), llmFactory func(llm.Options) llm.LLMClient, scanFactory func() tools.ScanRunner, newWriteRunner func() (tools.WriteRunner, error), sink func(sse.Event)) ChatService {
	svc := newChat(cfg, newReader, llmFactory, scanFactory, newWriteRunner, nil).(*chatService)
	svc.SetSink(sink)
	return svc
}

// NewChatWithStoreAndSink 创建四期 Agent 服务：同时做 Store 持久化（P1.8）与
// 统一 Chat 事件广播（P1.9，方案 25 单一出口）。
func NewChatWithStoreAndSink(cfg *config.Config, newReader func() (scanner.Reader, error), llmFactory func(llm.Options) llm.LLMClient, scanFactory func() tools.ScanRunner, newWriteRunner func() (tools.WriteRunner, error), st store.Store, sink func(sse.Event)) ChatService {
	svc := newChat(cfg, newReader, llmFactory, scanFactory, newWriteRunner, st).(*chatService)
	svc.SetSink(sink)
	return svc
}

// SetRuntimeConfigResolver 实现 RuntimeConfigBinder（P6.1）。
func (s *chatService) SetRuntimeConfigResolver(fn RuntimeConfigResolver) { s.resolveCfg = fn }

func (s *chatService) SetResultStore(results agent.ResultStore) {
	if gateway, ok := results.(*BlobGateway); ok && s.store != nil {
		s.resultStore = chatResultStore{gateway: gateway, store: s.store}
		return
	}
	s.resultStore = results
}

// effectiveConfig 返回本轮 Chat 的生效配置。装配了解析器时以其结果为准，解析失败
// 直接向上报错——绝不静默回落到启动基线或内置默认模型地址（P6.1 验收口径）。
// 生效范围是一轮 Chat 的装配面（工具/审批/LLM/Agent 限额）；会话默认 context 与
// 三期审计目录属静态启动参数，仍沿用启动基线。
func (s *chatService) effectiveConfig(ctx context.Context) (*config.Config, error) {
	if s.resolveCfg == nil {
		return s.cfg, nil
	}
	cfg, err := s.resolveCfg(ctx)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetSkillPlatform 挂载 Skill 平台底座（P3.8）：导入/启停后 create_resource 的
// dry-run 实时消费平台 registry。组合根在装配 Store 后调用。
func (s *chatService) SetSkillPlatform(p *SkillPlatform) {
	s.skillsPlat = p
}

// liveSkillRegistry 返回 create_resource 消费的实时 Skill registry（P3.8）：
// 组合根装配 Skill 平台后返回其内存 registry（导入/启停即时生效）；未装配时
// 回退内置 registry，避免 CLI 与测试桩受影响。
func (s *chatService) liveSkillRegistry() skills.Registry {
	if s.skillsPlat != nil {
		return s.skillsPlat.Registry()
	}
	if b, err := skills.NewBuiltin(); err == nil {
		return b
	}
	return skills.New()
}

// SetSink 挂载“提交后广播”钩子与连接终止帧钩子（方案 25 §4/§9）。
func (s *chatService) SetSink(sink func(sse.Event)) {
	s.ensureFlow()
	if s.flow == nil {
		return
	}
	s.flow.SetPublisher(sink)
}

// SetFatal 挂载非持久 stream.fatal 帧钩子（会话 API 用它中断当前连接）。
func (s *chatService) SetFatal(fn func(sessionID string, data sse.FatalData)) {
	s.ensureFlow()
	if s.flow == nil {
		return
	}
	s.flow.SetFatal(fn)
}

// ensureFlow 在 Store 支持 Chat 事实源时装配单一输出管道。
func (s *chatService) ensureFlow() {
	if s.flow != nil || s.store == nil {
		return
	}
	if cs, ok := s.store.(store.ChatStore); ok {
		s.flow = newChatFlow(cs)
	}
}

// chatReader 从配置构建只读 Kubernetes 门面（chat 用）。
func chatReader(cfg *config.Config) (scanner.Reader, error) {
	return kubernetes.NewClient(kubernetes.Options{
		RequestCheck: cfg.Kubernetes.RequestCheck, Kubeconfig: cfg.Kubernetes.Kubeconfig,
		Context: cfg.Kubernetes.Context,
		Timeout: cfg.Kubernetes.Timeout,
		QPS:     cfg.Kubernetes.QPS,
		Burst:   cfg.Kubernetes.Burst,
	})
}

// NewChatWithConfig 用完整配置创建 Agent 服务（main/CLI 组合根入口）。
func NewChatWithConfig(cfg *config.Config) ChatService {
	return NewChat(cfg, func() (scanner.Reader, error) { return chatReader(cfg) }, nil, nil, nil)
}

// NewChatWithStoreAndConfig 用完整配置 + 四期 Store 创建 Agent 服务（Web/Server 组合根入口）：
// 既保留三期事件流，又把 Session/Turn/Step 与 token 用量落库（P1.8/P2.13）。
func NewChatWithStoreAndConfig(cfg *config.Config, st store.Store) ChatService {
	return NewChatWithStore(cfg, func() (scanner.Reader, error) { return chatReader(cfg) }, nil, nil, nil, st)
}

// chatWriteRunner 从配置构建写执行器（kubectl argv + client-go，仅 executor 包内）。
func chatWriteRunner(cfg *config.Config) (tools.WriteRunner, error) {
	cs, err := kubernetes.NewClientset(kubernetes.Options{
		RequestCheck: cfg.Kubernetes.RequestCheck, Kubeconfig: cfg.Kubernetes.Kubeconfig,
		Context: cfg.Kubernetes.Context,
		Timeout: cfg.Kubernetes.Timeout,
		QPS:     cfg.Kubernetes.QPS,
		Burst:   cfg.Kubernetes.Burst,
	})
	if err != nil {
		return nil, err
	}
	bin := cfg.Kubernetes.KubectlPath
	if bin == "" {
		bin = "kubectl"
	}
	// 三期创建通道（R3）需要 dynamic + discovery：RESTConfig 与只读门面同源，
	// 缺它时 executor 会报「写通道未装配」而不是静默降级。
	restConfig, err := kubernetes.NewRESTConfig(kubernetes.Options{
		RequestCheck: cfg.Kubernetes.RequestCheck, Kubeconfig: cfg.Kubernetes.Kubeconfig,
		Context: cfg.Kubernetes.Context,
		Timeout: cfg.Kubernetes.Timeout,
		QPS:     cfg.Kubernetes.QPS,
		Burst:   cfg.Kubernetes.Burst,
	})
	if err != nil {
		return nil, err
	}
	return executor.NewRunner(executor.Options{
		ExecPod:    &executor.ExecPodOptions{Enabled: cfg.Agent.ExecPod.Enabled, TimeoutSeconds: cfg.Agent.ExecPod.TimeoutSeconds, MaxTimeoutSeconds: cfg.Agent.ExecPod.MaxTimeoutSeconds, MaxOutputBytes: cfg.Agent.ExecPod.MaxOutputBytes, MaxConcurrentPerInstance: cfg.Agent.ExecPod.MaxConcurrentPerInstance, MaxConcurrentPerUser: cfg.Agent.ExecPod.MaxConcurrentPerUser},
		KubectlBin: bin,
		Kubeconfig: cfg.Kubernetes.Kubeconfig,
		MaxBytes:   cfg.Agent.CmdMaxBytes,
		DenyOps:    cfg.Agent.DenyOps,
		// shell 通道开关来自静态配置（platform.exec.shell_enabled，默认开启）：
		// Options 的零值是关闭，装配方必须显式传，别让默认镜像丢掉管道排障能力。
		ShellEnabled: cfg.Platform.Exec.ShellEnabled,
		TargetBound:  cfg.Kubernetes.TargetBound,
		TargetID:     cfg.Kubernetes.TargetID,
		TargetCheck:  cfg.Kubernetes.TargetCheck,
		RequestCheck: cfg.Kubernetes.RequestCheck,
		Clientset:    cs,
		Namespace:    cfg.Kubernetes.Namespace,
		RESTConfig:   restConfig,
	}), nil
}

// chatScanOptions 把配置映射为一期 ScanOptions（诊断 Tool 的 base opts）。
func chatScanOptions(cfg *config.Config) model.ScanOptions {
	return model.ScanOptions{
		RequestCheck: cfg.Kubernetes.RequestCheck, Kubeconfig: cfg.Kubernetes.Kubeconfig,
		Context:             cfg.Kubernetes.Context,
		Namespace:           cfg.Kubernetes.Namespace,
		Timeout:             cfg.Scan.Timeout,
		QPS:                 cfg.Kubernetes.QPS,
		Burst:               cfg.Kubernetes.Burst,
		Concurrency:         cfg.Scan.Concurrency,
		Phase2Concurrency:   cfg.Scan.Phase2Concurrency,
		CollectLogs:         cfg.Scan.CollectLogs,
		CollectPreviousLogs: cfg.Scan.CollectPreviousLogs,
		CollectEvents:       cfg.Scan.CollectEvents,
		MaxLogLines:         cfg.Scan.MaxLogLines,
		MaxLogBytes:         cfg.Scan.MaxLogBytes,
		MaxLogLineBytes:     cfg.Scan.MaxLogLineBytes,
		PodLogsTimeout:      cfg.Scan.PodLogsTimeout,
		ReportDirectory:     cfg.Report.Directory,
		ReportFormat:        cfg.Report.Format,
		ReportMode:          "none",
		LLM: model.LLMOptions{
			Enabled:        cfg.LLM.Enabled,
			Endpoint:       cfg.LLM.Endpoint,
			APIKey:         cfg.LLM.APIKey,
			Model:          cfg.LLM.Model,
			Temperature:    cfg.LLM.Temperature,
			MaxTokens:      cfg.LLM.MaxTokens,
			MaxInputTokens: cfg.LLM.MaxInputTokens,
			MaxTotalTokens: cfg.LLM.MaxTotalTokens,
			MaxFindings:    cfg.LLM.MaxFindings,
			Timeout:        cfg.LLM.Timeout,
			Thinking:       cfg.Scan.Thinking,
		},
	}
}

// createToolOptions 把 agent.create.* / agent.secret.* 映射为创建工具的门禁与上限（R3.2）。
func createToolOptions(cfg *config.Config) tools.CreateOptions {
	limits := manifestx.Limits{
		MaxObjects: cfg.Agent.Create.MaxObjectsPerCall,
		MaxBytes:   cfg.Agent.Create.MaxManifestBytes,
	}
	return tools.CreateOptions{
		Policy: manifestx.Policy{
			DenyKinds:    cfg.Agent.Create.DenyKinds,
			AllowKinds:   cfg.Agent.Create.AllowKinds,
			ClusterKinds: cfg.Agent.Create.ClusterKinds,
		},
		Limits:           limits,
		DefaultNamespace: cfg.Kubernetes.Namespace,
		SecretAccess:     cfg.Agent.Secret.Access,
		VerifyTimeout:    executor.DefaultVerifyTimeout,
	}
}

// Chat 编排一轮对话：装配 Agent → 转发事件 → 持久化会话。
var ErrChatModelUnavailable = errors.New("所选模型不可用，请重新选择")

func (s *chatService) ChatModels(ctx context.Context) ([]config.ChatModel, error) {
	cfg, err := s.effectiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	return configuredChatModels(cfg), nil
}

func configuredChatModels(cfg *config.Config) []config.ChatModel {
	models := make([]config.ChatModel, 0, len(cfg.LLM.ModelProfiles))
	for _, profile := range cfg.LLM.ModelProfiles {
		models = append(models, config.ChatModel{ID: profile.ID, Name: profile.Name, ContextWindow: profile.ContextWindow})
	}
	for i, model := range models {
		if model.ID == cfg.LLM.DefaultChatModel && i > 0 {
			copy(models[1:i+1], models[:i])
			models[0] = model
			break
		}
	}
	return models
}

func ResolveConfiguredModel(cfg *config.Config, selected string) (*config.Config, string, error) {
	if selected == "" {
		selected = cfg.LLM.DefaultChatModel
	}
	if selected == "" {
		models := configuredChatModels(cfg)
		if len(models) == 0 {
			return nil, "", ErrChatModelUnavailable
		}
		selected = models[0].ID
	}
	for _, profile := range cfg.LLM.ModelProfiles {
		if profile.ID == selected {
			resolved := *cfg
			resolved.LLM.Model = profile.Model
			resolved.LLM.Endpoint = profile.Endpoint
			resolved.LLM.APIKey = profile.APIKey
			resolved.LLM.APIFormat = profile.APIFormat
			resolved.LLM.MaxTokens = profile.MaxTokens
			if profile.Temperature != nil {
				resolved.LLM.Temperature = *profile.Temperature
			}
			if profile.TimeoutSeconds > 0 {
				resolved.LLM.Timeout = time.Duration(profile.TimeoutSeconds) * time.Second
			}
			if profile.ThinkingMode != "" {
				thinking := map[string]string{"type": profile.ThinkingMode}
				resolved.LLM.Thinking = thinking
				resolved.Scan.Thinking = thinking
			}
			resolved.Agent.ContextWindow = profile.ContextWindow
			return &resolved, selected, nil
		}
	}
	return nil, "", ErrChatModelUnavailable
}

func (s *chatService) Chat(ctx context.Context, req ChatRequest, respond <-chan string) (<-chan ChatEvent, error) {
	if s.startupErr != nil {
		return nil, fmt.Errorf("recover interrupted chat runs: %w", s.startupErr)
	}
	sess, err := s.sessions.Get(req.SessionID)
	if err != nil {
		return nil, err
	}
	if req.UserID != "" && !req.AllowAnySession && sess.UserID != req.UserID {
		return nil, fmt.Errorf("%w: session %s", session.ErrForbidden, req.SessionID)
	}
	if sess.ToolContractVersion != tools.CatalogVersion || req.UserID != "" && req.ApprovalContext != sess.ApprovalContext {
		sess.ApprovalGrants = nil
		sess.ApprovedWrites = nil
		sess.ApprovalContext = req.ApprovalContext
		sess.ToolContractVersion = tools.CatalogVersion
	}
	// P6.1：本轮装配统一用生效配置，配置中心改模型后下一轮立即生效。
	cfg, err := s.effectiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	cfg, selected, err := ResolveConfiguredModel(cfg, req.ModelID)
	if err != nil {
		return nil, err
	}
	makeReader, makeWriter := s.newReader, s.newWriteRunner
	if req.KubeconfigPath != "" {
		c := *cfg
		cfg = &c
		cfg.Report.Directory = "" // platform runs cannot consult a CLI report from another scope
		cfg.Kubernetes.Kubeconfig = req.KubeconfigPath
		cfg.Kubernetes.Context = ""
		cfg.Kubernetes.RequestCheck = req.RequestCheck
		if req.AccessScope != nil && req.AccessScope.Policy != nil {
			cfg.Kubernetes.TargetID = req.AccessScope.Policy.ClusterID
		}
		// P12.2：平台会话的目标由服务端绑定到临时 kubeconfig；执行器据此
		// 拒绝无法证明目标边界的 shell 与目标覆盖参数，不依赖人工审批。
		cfg.Kubernetes.TargetBound = true
		cfg.Kubernetes.TargetCheck = req.TargetCheck
		makeReader = func() (scanner.Reader, error) { return chatReader(cfg) }
		makeWriter = func() (tools.WriteRunner, error) { return chatWriteRunner(cfg) }
	}
	reader, err := makeReader()
	if err != nil {
		return nil, err
	}
	// 本轮运行的 turn/step 作用域要在工具装配前创建：Skill 观测回调要用它补关联字段。
	scope := &runScope{sessID: sess.ID}
	reg := tools.NewRegistry()
	disabled := map[string]bool{}
	for _, name := range cfg.Agent.DisabledTools {
		disabled[strings.TrimSpace(name)] = true
	}
	if err := k8stools.RegisterAllWithOptions(reg, reader, k8stools.RegisterOptions{
		SecretAccess: cfg.Agent.Secret.Access, Cluster: sess.Cluster, Disabled: disabled,
	}); err != nil {
		return nil, err
	}
	// 诊断 Tool 复用一期 ScanService（P3）。P12.2：诊断子调用同样绑定会话
	// ClusterID，工具结果与后续证据可追溯到真实目标。
	scanOpts := chatScanOptions(cfg)
	scanOpts.Attribution.ClusterID = sess.Cluster
	scanOpts.Attribution.Actor = req.UserID
	for _, dt := range tools.NewDiagnosisTools(s.scanFactory(), scanOpts, disabled) {
		if err := reg.Register(dt); err != nil {
			return nil, err
		}
	}
	// 写 Tool（P4）：kubectl + 结构化写，全部走审批。
	writeRunner, err := makeWriter()
	if err != nil {
		return nil, err
	}
	for _, wt := range tools.NewWriteTools(writeRunner) {
		if err := reg.Register(wt); err != nil {
			return nil, err
		}
	}
	if cfg.Agent.ExecPod.Enabled {
		if runner, ok := writeRunner.(tools.PodExecRunner); ok {
			if err := reg.Register(tools.NewPodExecTool(runner)); err != nil {
				return nil, err
			}
		}
	}
	// 三期创建通道（R3）：结构化 create_resource，注册在 disabled_tools 之前，
	// 这样 agent.disabled_tools 里写 create_resource 就能摘掉它。
	// liveReg 复用于 create_resource 的 Skill registry 与 procedural 意图检索，
	// 保证导入/启停即时生效两端一致。
	liveReg := s.liveSkillRegistry()
	if cfg.Agent.Create.Enabled {
		if runner, ok := writeRunner.(tools.CreateRunner); ok {
			opts := createToolOptions(cfg)
			opts.Skills = liveReg
			opts.OnSkillRun = s.skillRunAuditor(ctx, sess, scope)
			if err := reg.Register(tools.NewCreateTool(opts, runner, s.proposals, sess.ID)); err != nil {
				return nil, err
			}
		}
	}
	// P3.8.4：Skill 只读工具（list_skills/get_skill）装配，注册在 disabled_tools
	// 之前，配置里同样可摘掉。正文永不执行，仅作文本参考。
	for _, st := range tools.NewSkillsTools(liveReg) {
		if err := reg.Register(st); err != nil {
			return nil, err
		}
	}
	for _, name := range cfg.Agent.DisabledTools {
		reg.Remove(strings.TrimSpace(name))
		if strings.TrimSpace(name) == "exec_pod" {
			if disabler, ok := writeRunner.(interface{ DisablePodExec() }); ok {
				disabler.DisablePodExec()
			}
		}
	}
	events := make(chan ChatEvent, 64)
	pol := approval.Policy{
		Mode:            approval.Mode(cfg.Agent.ApprovalMode),
		RequireApproval: req.UserID != "" || cfg.Agent.RequireApproval,
		SkipPermissions: req.UserID == "" && (req.SkipPermissions || cfg.Agent.SkipPermissions),
		DenyOps:         cfg.Agent.DenyOps,
	}
	// P1.9: 生命周期观察器只负责 Store 持久化；展示事件走单一输出管道。
	tracer := s.chatTracer(ctx, sess, scope)
	// 单一输出管道（方案 25）：本轮运行的所有展示事件经同一有序写者提交后广播。
	s.ensureFlow()
	var writer *chatWriter
	if s.flow != nil {
		writer = s.flow.Open(sess.ID)
	}
	approver := &chatApprover{
		events:  events,
		respond: respond, confirmation: req.Confirmation, cluster: sess.Cluster,
		flow: writer,
	}
	client := s.llmFactory(llm.Options{
		Endpoint:        cfg.LLM.Endpoint,
		APIKey:          cfg.LLM.APIKey,
		Model:           cfg.LLM.Model,
		Temperature:     cfg.LLM.Temperature,
		MaxTokens:       cfg.LLM.MaxTokens,
		Timeout:         cfg.LLM.Timeout,
		Thinking:        cfg.LLM.Thinking,
		ReasoningFields: cfg.LLM.ReasoningFields,
		DisableStream:   !cfg.LLM.Stream, // llm.stream=false：ChatStream 退化为非流式
	})
	recorder := s.newAuditRecorder(sess, scope)
	// 运行上下文独立于 HTTP 请求：SSE 断开不影响执行；输出不可靠时由写者取消。
	runCtx, cancelRun := context.WithCancel(ctx)
	handedOff := false
	defer func() {
		if !handedOff {
			cancelRun()
		}
	}()
	if writer != nil {
		writer.SetRunCancel(cancelRun)
	}
	// P3.8.4：procedural 自定义 Skill 装配（显式 @ 指定或自动意图检索），命中正文
	// 拼接为不可信方法论附录注入 Agent system prompt，并记录命中统计与 SKILL_RUN
	// retrieve 审计。显式指定时任一名字无效整批拒绝，不做静默降级。
	skillCtx, skillHits, err := s.buildSkillContext(ctx, sess, scope, liveReg, req.Query, req.Skills)
	if err != nil {
		return nil, err
	}
	for _, h := range skillHits {
		events <- ChatEvent{Type: ChatEventProgress, Text: fmt.Sprintf("已注入 Skill：%s（%s）", h.name, h.mode)}
	}
	if req.AccessScope != nil {
		bound := *req.AccessScope
		req.AccessScope = &bound
		if channel, ok := writeRunner.(interface{ ProxiedKubectl() bool }); ok {
			req.AccessScope.RequestEnforced = channel.ProxiedKubectl()
		}
		if resolver, ok := writeRunner.(interface {
			ResolveCommandResource(context.Context, string) (string, string, bool, error)
		}); ok {
			bound := *req.AccessScope
			bound.ResolveResource = resolver.ResolveCommandResource
			req.AccessScope = &bound
		}
	}
	var recallMemory func(context.Context) (string, []string)
	var suggestMemory bool
	if s.diagnosticMemories != nil && req.UserID != "" && req.UserID == sess.UserID && req.AccessScope != nil {
		clusterID, namespace := sess.Cluster, sess.Namespace
		suggestMemory, _ = s.diagnosticMemories.enabled(ctx)
		recallMemory = func(ctx context.Context) (string, []string) {
			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()
			access := req.AccessScope
			if access.Refresh != nil {
				fresh, err := access.Refresh(ctx)
				if err != nil {
					return "", nil
				}
				access = fresh
			}
			if access == nil || access.Policy == nil {
				return "", nil
			}
			return s.diagnosticMemories.Recall(ctx, req.UserID, clusterID, namespace, req.Query, access.Policy.Check)
		}
	}
	var remoteClosers []func()
	if cfg.MCP.Enabled && req.AccessScope != nil && req.AccessScope.ExternalMCP {
		discovery, cancel := context.WithTimeout(runCtx, 5*time.Second)
		for _, spec := range cfg.MCP.Servers {
			if spec.Disabled {
				continue
			}
			if discovery.Err() != nil {
				break
			}
			closeRemote, err := mcp.RegisterRemote(discovery, reg, spec, func(callCtx context.Context) error {
				current, err := s.effectiveConfig(callCtx)
				if err == nil && current != nil && current.MCP.Enabled {
					for _, server := range current.MCP.Servers {
						if reflect.DeepEqual(server, spec) {
							return nil
						}
					}
				}
				return fmt.Errorf("MCP 已关闭或服务器配置已变更，请重新发起问诊")
			})
			if err != nil {
				slog.Warn("external MCP discovery failed", "error", err)
				continue
			}
			remoteClosers = append(remoteClosers, closeRemote)
		}
		cancel()
	}
	ag := agent.New(agent.Options{
		RecallMemory:   recallMemory,
		SuggestMemory:  suggestMemory,
		LLM:            client,
		Tools:          reg,
		Session:        sess,
		Approver:       approver,
		Ask:            req.Ask,
		Flow:           agentFlow(writer),
		ApprovalPolicy: &pol,
		Recorder:       recorder,
		Tracer:         tracer,
		SkillContext:   skillCtx,
		MaxIterations:  cfg.Agent.MaxIterations,
		MaxToolCalls:   cfg.Agent.MaxToolCalls,
		Timeout:        cfg.Agent.Timeout,
		ToolCalling:    cfg.Agent.ToolCalling,
		ResultMaxBytes: cfg.Agent.ResultMaxBytes,
		TurnMaxBytes:   cfg.Agent.TurnMaxBytes,
		HistoryTurns:   cfg.Agent.HistoryTurns,
		ContextWindow:  cfg.Agent.ContextWindow,
		OutputTokens:   cfg.LLM.MaxTokens,
		ModelID:        selected,
		ResultStore:    s.resultStore,
		DisabledTools:  cfg.Agent.DisabledTools,
		TurnUserID:     req.UserID,
		TurnID:         req.TurnID,
		AccessScope:    req.AccessScope,
	})
	handedOff = true
	go func() {
		defer func() {
			for _, closeRemote := range remoteClosers {
				closeRemote()
			}
		}()
		s.runChat(runCtx, ag, sess, req, events, recorder, writer, cancelRun, scope)
	}()
	return events, nil
}

// agentFlow 把单会话写者适配成 agent.Flow；未装配管道时返回 nil（CLI 直用）。
func agentFlow(w *chatWriter) agent.Flow {
	if w == nil {
		return nil
	}
	return w
}

// chatTracer 装配生命周期观察器：统一事件广播器（P1.9）+ Store 持久化（P1.8），
// 两者共用调用方创建的 runScope（当前 turn/step 作用域）。
// 数据库不可用时不阻塞聊天，仅回退为不持久化。
func (s *chatService) chatTracer(ctx context.Context, sess *session.Session, scope *runScope) agent.Tracer {
	var persist agent.Tracer
	if s.store != nil {
		if s.sessionsInStore {
			// 会话行已由 session.Store 适配器在建会话时写入（P8.0），这里不再补写一次。
			persist = &persistTracer{store: s.store}
		} else if err := s.store.Sessions().Create(ctx, sess); err != nil && !errors.Is(err, store.ErrExists) {
			slog.Warn("chat: 四期会话初始化失败，本轮跳过 Turn/Step 持久化", "session", sess.ID, "error", err)
		} else {
			persist = &persistTracer{store: s.store}
		}
	}
	if s.flow == nil && persist == nil {
		return nil
	}
	// Tracer 只观察与持久化 Turn/Step，不再产生第二份展示事件（方案 25 §4）。
	return persist
}

// persistTracer 把 Agent 生命周期回调写入四期 Store（P1.8）。落库前由 Store 侧统一脱敏，
// 回调为观察性质，所有写失败只记日志、不中断 Agent 主循环。
type persistTracer struct {
	store store.Store
}

func (t *persistTracer) TurnStarted(ctx context.Context, turn *session.Turn) {
	if err := t.store.Turns().Create(ctx, turn); err != nil {
		slog.Warn("chat: Turn 写入失败", "turn", turn.ID, "error", err)
	}
}

func (t *persistTracer) TurnUpdated(ctx context.Context, turn *session.Turn) {
	if err := t.store.Turns().Update(ctx, turn); err != nil {
		slog.Warn("chat: Turn 更新失败", "turn", turn.ID, "error", err)
	}
}

func (t *persistTracer) StepStarted(ctx context.Context, step *session.Step) {
	if err := t.store.Steps().Create(ctx, step); err != nil {
		slog.Warn("chat: Step 写入失败", "turn", step.TurnID, "step", step.ID, "error", err)
	}
}

func (t *persistTracer) StepUpdated(ctx context.Context, step *session.Step) {
	if err := t.store.Steps().Update(ctx, step); err != nil {
		slog.Warn("chat: Step 更新失败", "turn", step.TurnID, "step", step.ID, "error", err)
	}
}

// newAuditRecorder 为会话创建审计记录器（目录不存在自动创建；失败回退日志记录器）。
// 装配了四期 Store 时同一事件同时落 AuditStore（P1.11），关联字段取自会话与本轮 turn/step。
func (s *chatService) newAuditRecorder(sess *session.Session, scope *runScope) audit.Recorder {
	var inner audit.Recorder
	if s.auditFileCopyEnabled() {
		path := filepath.Join(s.cfg.Agent.AuditDir, sess.ID+".jsonl")
		fileRecorder, err := audit.NewFileRecorder(path)
		if err != nil {
			slog.Warn("chat: 审计文件不可用，回退日志记录器", "path", path, "error", err)
			inner = &audit.LogRecorder{}
		} else {
			inner = fileRecorder
		}
	} else {
		// 副本已关闭（agent.audit_file_copy=false）：审计事实源是平台库 audit_events，
		// 会话目录里不再产生第二份可含事件负载的落盘文件（P8.0）。
		inner = audit.NopRecorder{}
	}
	if s.store == nil {
		return inner
	}
	return newStoreAuditRecorder(inner, s.store, scope, AuditScope{
		SessionID: sess.ID,
		UserID:    sess.UserID,
		ClusterID: sess.Cluster,
	})
}

// auditFileCopyEnabled 判断本轮是否还写会话审计 jsonl 副本（P8.0）：
// 终端直用（无平台库）恒为 true——文件就是审计唯一出口；平台模式下由
// agent.audit_file_copy 决定，默认 true 保持既有部署行为不变。
func (s *chatService) auditFileCopyEnabled() bool {
	if s.store == nil {
		return true
	}
	return s.cfg == nil || s.cfg.Agent.AuditFileCopy
}

func (s *chatService) runChat(ctx context.Context, ag *agent.Agent, sess *session.Session, req ChatRequest, events chan<- ChatEvent, recorder audit.Recorder, writer *chatWriter, cancelRun context.CancelFunc, scope *runScope) {
	defer close(events)
	defer func() { _ = recorder.Close() }()
	defer cancelRun()
	done := make(chan struct{})
	go func() {
		defer close(done)
		var pendingAnswer strings.Builder
		hadToolCall := false
		for m := range ag.Output {
			if ev, ok := translateEvent(m); ok {
				switch ev.Type {
				case ChatEventProgress:
					events <- ev
				case ChatEventAnswerDelta, ChatEventDelta:
					// 先缓存回答增量。若后续发现本轮实际包含工具调用，
					// 这段内容只能是工具前的中间说明，不能提前展示为报告。
					pendingAnswer.WriteString(ev.Text)
				case ChatEventToolCall:
					if progress := summarizeToolPrelude(pendingAnswer.String()); progress != "" {
						events <- ChatEvent{Type: ChatEventProgress, Text: progress}
					}
					hadToolCall = true
					pendingAnswer.Reset()
					events <- ev
				case ChatEventToolResponse:
					events <- ev
					// 工具结果之后进入新的 LLM 回答轮次，允许最终报告增量通过。
					hadToolCall = false
				default:
					events <- ev
				}
			}
		}
		if !hadToolCall && pendingAnswer.Len() > 0 {
			events <- ChatEvent{Type: ChatEventAnswerDelta, Text: pendingAnswer.String()}
		}
	}()
	answer, usage, chatErr := ag.Chat(ctx, req.Query)
	sess.State = string(ag.State)
	// 先关通道并等转发协程收尾，再提交终态：展示事实必须在宣告结束之前落库。
	close(ag.Output)
	<-done
	if req.CoverageSnapshot != nil {
		sess.Coverage = req.CoverageSnapshot()
	}
	// 终态与模型历史同一事务提交，提交成功才发布 turn.status(done/failed)。
	var capacityWarn, capacityFail string
	if writer != nil {
		finishCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := writer.Finish(finishCtx, ag.TurnSnapshot(), sess, ag.FinalMessageID())
		cancel()
		if err != nil {
			slog.Warn("chat: 终态提交失败", "session", sess.ID, "error", err)
			writer.failOutput(err)
			// Agent has exited. Reconcile only after its writer has stopped.
			if recovery, ok := s.store.(interface {
				InterruptChatRun(context.Context, string) error
			}); ok {
				<-writer.done
				recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
				if recoveryErr := recovery.InterruptChatRun(recoveryCtx, sess.ID); recoveryErr != nil {
					slog.Warn("chat: 中断状态保存失败", "session", sess.ID, "error", recoveryErr)
				}
				recoveryCancel()
			}
			if errors.Is(err, store.ErrSessionTooLarge) {
				// 容量超限不是“输出链路故障”：回复本身已完成，只是这次没存进会话。
				capacityFail = fmt.Sprintf(
					"本轮回复没有存进会话：%v。回复内容本身已完成，请新建会话继续，或由管理员调大 agent.session_max_bytes。", err)
			} else {
				capacityFail = "本轮输出未能可靠保存，请重新同步会话"
			}
		}
	} else {
		// 无平台 Store 的终端直用模式：沿用会话文件/内存事实源。
		capacityWarn, capacityFail = s.persistTurnSession(ctx, sess)
	}
	events <- ChatEvent{Type: ChatEventAnswer, Text: answer, Usage: &usage}
	if chatErr != nil && !errors.Is(chatErr, context.Canceled) {
		events <- ChatEvent{Type: ChatEventError, Text: chatErr.Error()}
	}
	// 容量提示放在答案之后：它不影响本轮回复内容，抢在答案前面会把用户注意力从结果上挪走。
	if capacityWarn != "" {
		events <- ChatEvent{Type: ChatEventProgress, Text: capacityWarn}
	}
	if capacityFail != "" {
		events <- ChatEvent{Type: ChatEventError, Text: capacityFail}
		if writer != nil {
			s.flow.Notice(sess.ID, "", sse.NoticeError, "SESSION_SAVE_FAILED", capacityFail)
		}
	}
}

// persistTurnSession 在一轮对话结束时保存会话，并返回两句可读提示：接近上限的预警、
// 以及超限导致本轮未保存的说明（空串表示没有该问题）。
//
// 容量判定本身在持久化层（internal/store 的写入漏斗），这里只负责把它翻译成用户看得懂的
// 话。session.Store 用可选能力 session.SizedStore 上报容量，文件/内存后端没有闸门，
// 断言失败就退回普通 Update，行为与改动前一致。
func (s *chatService) persistTurnSession(ctx context.Context, sess *session.Session) (warning, failure string) {
	warning, failure = s.saveSession(sess)
	// 会话事实源已是平台库时，上一步就是落库，不再重复写同一行（正文可达 MiB 级）。
	if s.store == nil || s.sessionsInStore {
		return warning, failure
	}
	mirrorWarn, mirrorFail := s.saveSessionMirror(ctx, sess)
	return joinCapacityNotice(warning, mirrorWarn), joinCapacityNotice(failure, mirrorFail)
}

// saveSession 写会话事实源（session.Store），有容量回报时一并换算成提示。
func (s *chatService) saveSession(sess *session.Session) (warning, failure string) {
	rep, err := s.sessions.UpdateSized(sess)
	return sessionCapacityNotices(rep, err, sess.ID)
}

// saveSessionMirror 写平台库镜像（会话事实源仍在文件目录的历史形态）。
func (s *chatService) saveSessionMirror(ctx context.Context, sess *session.Session) (warning, failure string) {
	if sized, ok := s.store.Sessions().(store.SessionSizeStore); ok {
		rep, err := sized.UpdateSized(ctx, sess)
		return sessionCapacityNotices(storeSizeReport(rep), err, sess.ID)
	}
	if err := s.store.Sessions().Update(ctx, sess); err != nil {
		_, failure = sessionCapacityNotices(session.SizeReport{}, err, sess.ID)
		return "", failure
	}
	return "", ""
}

// joinCapacityNotice 合并事实源与镜像两侧的提示，避免一侧的告警被另一侧覆盖。
func joinCapacityNotice(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " " + b
	}
}

// storeSizeReport 把 store 侧的容量回报换成 session 侧同名字段（两包各自定义，避免
// session 反向依赖 store）。
func storeSizeReport(rep store.SessionSizeReport) session.SizeReport {
	return session.SizeReport{Size: rep.Size, Limit: rep.Limit, Warning: rep.Warning}
}

// sessionCapacityNotices 把一次容量回报换成两句用户可见文案。非容量错误仍只进日志：
// 数据库抖动不该伪装成「会话太大」，那会把用户引向完全错误的处置方向。
func sessionCapacityNotices(rep session.SizeReport, err error, id string) (warning, failure string) {
	if err != nil {
		slog.Warn("chat: 保存会话失败", "session", id, "error", err)
		if !errors.Is(err, store.ErrSessionTooLarge) {
			return "", ""
		}
		return "", fmt.Sprintf(
			"本轮回复没有存进会话：会话正文 %s 已超过上限 %s（配置键 agent.session_max_bytes）。"+
				"回复内容本身已完成，请新建会话继续，或由管理员调大该上限。",
			store.FormatBytes(rep.Size), store.FormatBytes(rep.Limit))
	}
	if !rep.Warning {
		return "", ""
	}
	return fmt.Sprintf(
		"会话正文已用到 %s，接近上限 %s：再积累几轮就可能无法继续保存，建议新开会话。",
		store.FormatBytes(rep.Size), store.FormatBytes(rep.Limit)), ""
}

// emitMessageDelta 通过 sink 广播统一 message.delta 事件（携带当前 turn/step 作用域）。
// 增量与终态走同一套脱敏（sse 包约定：写入事件流的内容在封装前必须脱敏），
// 避免同一段正文在流式过程中与 message.completed 里呈现不同内容。
// summarizeToolPrelude 保留工具调用前的短进度说明，避免把真正的诊断文本直接丢掉。
// 关键点：只过滤明显的状态噪音（空文本、超长文本、Markdown/代码块），不要因为
// “修复/建议/结论”等词语出现在思考过程中而误伤实际诊断内容。
func summarizeToolPrelude(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > 160 {
		return ""
	}
	if strings.Contains(text, "```") || strings.Contains(text, "##") {
		return ""
	}
	return text
}

// translateEvent 把 Agent 消息总线事件转成服务层事件。
func translateEvent(m agent.Message) (ChatEvent, bool) {
	switch m.Type {
	case agent.MessageTypeText:
		// SourceModel = LLM 流式增量；SourceUser = 用户输入回显（CLI 交互模式）。
		if m.Source == agent.SourceModel {
			return ChatEvent{Type: ChatEventAnswerDelta, Text: toString(m.Payload)}, true
		}
		return ChatEvent{Type: ChatEventText, Text: toString(m.Payload)}, true
	case agent.MessageTypeReasoningDelta:
		return ChatEvent{Type: ChatEventReasoningDelta, Text: toString(m.Payload)}, true
	case agent.MessageTypeError:
		return ChatEvent{Type: ChatEventError, Text: toString(m.Payload)}, true
	case agent.MessageTypeAgentState:
		return ChatEvent{Type: ChatEventState, State: toString(m.Payload)}, true
	case agent.MessageTypeToolCallRequest:
		if ev, ok := m.Payload.(agent.ToolCallEvent); ok {
			return ChatEvent{Type: ChatEventToolCall, Tool: &ChatToolCallView{Name: ev.Name, Arguments: ev.Arguments, Risk: ev.Risk}}, true
		}
	case agent.MessageTypeToolCallResponse:
		if mm, ok := m.Payload.(map[string]any); ok {
			return ChatEvent{
				Type: ChatEventToolResponse,
				Tool: &ChatToolCallView{
					Name:         toString(mm["name"]),
					ResultStatus: toString(mm["resultStatus"]),
					Result:       toString(mm["text"]),
					Summary:      toString(mm["summary"]),
					Ok:           boolPtr(mm["ok"]),
					Truncated:    toBool(mm["truncated"]),
					Data:         mm["data"],
					Risk:         toString(mm["risk"]),
					DurationMs:   toInt64(mm["durationMs"]),
					DisplayType:  toString(mm["displayType"]),
					Command:      toString(mm["command"]),
					Stdout:       toString(mm["stdout"]),
					Stderr:       toString(mm["stderr"]),
					ExitCode:     int(toInt64(mm["exitCode"])),
					ErrorCode:    toString(mm["errorCode"]),
				},
			}, true
		}
	}
	return ChatEvent{}, false
}

// chatApprover 把审批请求转成统一 Interaction 并等待 respond 回传选择。
type chatApprover struct {
	confirmation Confirmation
	cluster      string
	events       chan<- ChatEvent
	respond      <-chan string
	skip         bool
	autoApprove  bool
	// flow 非空时把审批请求作为 interaction.updated 提交后展示（单一出口）。
	flow *chatWriter
}

func (a *chatApprover) Confirm(ctx context.Context, prompt string, options []agent.UserChoiceOption) (string, error) {
	return a.confirm(ctx, prompt, options, nil)
}

// ConfirmStructured 在保留 CLI/旧事件 prompt 的同时，提交结构化审批事实。
func (a *chatApprover) ConfirmStructured(ctx context.Context, req agent.ApprovalRequest) (string, error) {
	data := interactionFromPlan(req)
	if req.InteractionID != "" {
		data.InteractionID = req.InteractionID
	}
	return a.confirm(ctx, req.Prompt, req.Options, data)
}

// interactionFromPlan 把审批计划映射成统一 Interaction 公开载荷（已脱敏）。
func interactionFromPlan(req agent.ApprovalRequest) *sse.Interaction {
	plan := req.Plan
	in := &sse.Interaction{
		InteractionID: "int_" + flowID(), TurnID: "", ToolCallID: req.ToolCallID,
		Kind: sse.InteractionApproval, Status: sse.InteractionPending, Revision: 0,
		Title: plan.Title, Tool: plan.Tool, Operation: plan.Operation, Risk: plan.Risk,
		Summary: plan.Summary, Command: plan.Command, Prompt: req.Prompt,
		Verification: append([]string(nil), plan.Verification...),
	}
	for _, o := range req.Options {
		in.Options = append(in.Options, sse.ChoiceOption{Label: o.Label, Value: o.Value})
	}
	if plan.Target != (tools.ApprovalTarget{}) {
		in.Target = &sse.Target{
			Cluster: plan.Target.Cluster, Namespace: plan.Target.Namespace,
			Group: plan.Target.Group, Version: plan.Target.Version,
			Kind: plan.Target.Kind, Name: plan.Target.Name,
		}
	}
	for _, change := range plan.Changes {
		in.Changes = append(in.Changes, sse.Change{
			Path: change.Path, Operation: change.Operation, Before: change.Before,
			After: change.After, Description: change.Description,
		})
	}
	for _, check := range plan.Checks {
		in.Checks = append(in.Checks, sse.Check{Name: check.Name, Status: check.Status, Message: check.Message})
	}
	return in
}

func (a *chatApprover) confirm(ctx context.Context, prompt string, options []agent.UserChoiceOption, interaction *sse.Interaction) (string, error) {
	if a.skip || a.autoApprove {
		return agent.ChoiceYes, nil
	}
	reply := a.respond
	if interaction == nil {
		interaction = &sse.Interaction{
			InteractionID: "int_" + flowID(), Kind: sse.InteractionApproval,
			Status: sse.InteractionPending, Prompt: prompt,
		}
		for _, o := range options {
			interaction.Options = append(interaction.Options, sse.ChoiceOption{Label: o.Label, Value: o.Value})
		}
	}
	if a.confirmation != nil {
		id, ch, done, err := a.confirmation(ctx, *interaction)
		if err != nil {
			return "", err
		}
		defer done()
		reply = ch
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		interaction.InteractionID = id
		if interaction.Target == nil {
			interaction.Target = &sse.Target{}
		}
		interaction.Target.Cluster = a.cluster
	}
	opts := make([]ChatChoiceOption, 0, len(options))
	for _, o := range options {
		opts = append(opts, ChatChoiceOption{Label: o.Label, Value: o.Value})
	}
	a.events <- ChatEvent{Type: ChatEventChoice, Choice: &ChatChoice{Prompt: prompt, Options: opts, Approval: interaction}}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case c := <-reply:
		return c, nil
	}
}

// RequestSecretValue 走同一条 Interaction 通道索取 Secret 值（secret=true）。
// 返回值只存在于内存与本次工具调用，不进事件流、会话文件与审计日志。
func (a *chatApprover) RequestSecretValue(ctx context.Context, req agent.SecretValueRequest) (string, error) {
	if a.skip || a.autoApprove {
		return "", errors.New("非交互模式下无法向用户索取 Secret 值，请改用 $GENERATE 由 AegisOps 随机生成")
	}
	interaction := sse.Interaction{
		InteractionID: req.InteractionID, Kind: sse.InteractionSecretRead,
		Status: sse.InteractionPending, Prompt: req.Prompt, Secret: true, ToolCallID: req.ToolCallID,
	}
	reply := a.respond
	if a.confirmation != nil {
		key, ch, done, err := a.confirmation(ctx, interaction)
		if err != nil {
			return "", err
		}
		defer done()
		interaction.InteractionID = key
		reply = ch
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
	}
	a.events <- ChatEvent{Type: ChatEventChoice, Choice: &ChatChoice{Prompt: req.Prompt, Secret: true, Approval: &interaction}}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case c := <-reply:
		value := strings.TrimSpace(c)
		if value == "" || strings.EqualFold(value, "no") {
			return "", nil
		}
		return c, nil
	}
}

// GetOrCreateSession 获取已有会话，不存在则创建。
func (s *chatService) GetOrCreateSession(id, namespace, model string) (*session.Session, error) {
	if id != "" {
		if sess, err := s.sessions.Get(id); err == nil {
			return sess, nil
		}
	}
	return s.sessions.NewSession(id, s.cfg.Kubernetes.Context, namespace, model)
}

// GetSession 只读取已有会话，不存在时返回 session.ErrNotFound。
func (s *chatService) GetSession(id string) (*session.Session, error) {
	return s.sessions.Get(id)
}

// GetOrCreateSessionForUser 将 Web 会话绑定到当前登录用户。
func (s *chatService) NewClusterSession(id, userID, cluster, model string, coverage []byte) (*session.Session, error) {
	return s.sessions.NewBoundSession(id, userID, cluster, "", model, coverage)
}

func (s *chatService) GetOrCreateSessionForUser(id, userID, namespace, model string) (*session.Session, error) {
	return s.sessions.GetOrCreateSessionForUser(id, userID, namespace, model)
}

func (s *chatService) ListSessions() ([]session.Session, error) {
	list, err := s.sessions.List()
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(list))
	for _, sess := range list {
		out = append(out, *sess)
	}
	return out, nil
}

func (s *chatService) ListSessionsForUser(userID string) ([]session.Session, error) {
	list, err := s.sessions.ListForUser(userID)
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(list))
	for _, sess := range list {
		out = append(out, *sess)
	}
	return out, nil
}

func (s *chatService) DeleteSession(id string) error {
	return s.sessions.Delete(id)
}

func (s *chatService) ClearSession(id string) error {
	sess, err := s.sessions.Get(id)
	if err != nil {
		return err
	}
	sess.ClearMessages()
	return s.sessions.Update(sess)
}

func (s *chatService) RenameSession(id, name string) error {
	sess, err := s.sessions.Get(id)
	if err != nil {
		return err
	}
	sess.Name = name
	return s.sessions.Update(sess)
}

// PinSession 置顶/取消置顶会话（P7.5）。
func (s *chatService) PinSession(id string, pinned bool) error {
	sess, err := s.sessions.Get(id)
	if err != nil {
		return err
	}
	sess.Pinned = pinned
	return s.sessions.Update(sess)
}

// SessionTokenUsage 汇总会话内各轮 token 用量（P2.13）。
// 未装配四期 Store（三期运行模式）时返回零值：Web 右下角统计自动隐藏，
// 而不是把"未持久化"误报成 0 tokens。
func (s *chatService) SessionTokenUsage(ctx context.Context, sessionID string) (session.TokenUsage, error) {
	if s.store == nil || sessionID == "" {
		return session.TokenUsage{}, nil
	}
	return s.store.Turns().SumTokensBySession(ctx, sessionID)
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toBool(v any) bool {
	b, _ := v.(bool)
	return b
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

// boolPtr 返回 *bool：字段缺失（旧客户端/旧会话无 ok 键）时为 nil，前端据此回退正则兜底。
func boolPtr(v any) *bool {
	b, ok := v.(bool)
	if !ok {
		return nil
	}
	return &b
}
