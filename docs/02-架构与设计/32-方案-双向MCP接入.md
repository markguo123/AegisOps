# AegisOps 双向 MCP 接入方案

日期 2026-11-26，状态为 MVP 已实现，待环境联调。

当前实现已接入 Web Agent 的两种出站传输，并提供独立 CLI 服务。本地协议测试覆盖两种出站传输及 CLI 的发现、发起、轮询、取报告。管理员在配置中心保存后可点「检查连接」，每轮问诊从当前配置发现外部工具。工具目录缓存仍待补齐。

Grafana 已升级提供 Streamable HTTP 后，配置项用 `transport: streamable-http`，URL 填部署方提供的实际 MCP 地址。内网 HTTP 地址需要显式设置 `allow_http: true`。

CLI 启动前通过环境变量 `AEGISOPS_MCP_TOKEN` 提供独立随机凭据，客户端以 Bearer 头传递。当前每个 CLI 实例使用一份调用方凭据，需要隔离的系统分别启动实例。CLI 报告包含健康概览、采集质量和可分页发现，分页使用 `offset` / `nextOffset`。结果在进程内保留一小时。跨主机连接在 CLI 回环端口前配置 HTTPS 反向代理。

## 1. 产品边界

| 方向 | 用户入口 | 提供的能力 |
| --- | --- | --- |
| AegisOps 调用外部 MCP | Web 配置中心和 Agent 问诊 | 管理员接入外部工具，Agent 在同一轮结合 Kubernetes 原生证据与外部数据完成诊断 |
| 外部 MCP 客户端调用 AegisOps | 独立启动的 `aopsctl mcp serve` 进程 | 异步发起体检、查询任务进度、读取结构化报告 |

常规生产平台部署只装配出站 MCP 客户端。CLI 服务由接入方手动启动、独立部署和停止。两个入口各自配置，CLI 服务不由平台页面控制。

配置中心沿用 `mcp.enabled` 和加密存储的 `mcp.servers`。[`internal/mcp`](../../internal/mcp) 承担出站协议连接及工具适配，独立 CLI 入口负责装配体检服务。

## 2. Web 端接入外部 MCP

### 2.1 传输与现有 Grafana

出站首版支持两种显式配置的传输：Streamable HTTP 用于新接入，旧版 HTTP+SSE 用于已有服务。两者均通过真实 MCP 初始化和工具列表验证连通性。Streamable HTTP 请求中的 SSE 响应仍属于 Streamable HTTP。

现有生产云 `mcp-grafana` 已通过 NodePort `32539` 提供服务。2026-11-26 从当前工作环境进行未认证探测，`POST /mcp`（MCP `initialize`）返回 404，`GET /sse` 返回 401。该结果确认 `/sse` 可达且未认证请求被拒绝。带凭据的 SSE 握手、工具发现和 Grafana 查询仍待联调。首轮复用现有 `/sse` 服务，不以修改生产 Grafana 部署为前置条件。

### 2.2 配置与调用流程

保留 `mcp.enabled` 和 `mcp.servers`。每个服务器配置唯一名称、传输类型、地址、可选认证头和精确工具白名单。示例中的地址为占位符，认证头与服务器配置一同加密存储。

```json
[
  {
    "name": "grafana",
    "transport": "sse",
    "url": "https://grafana-mcp.example.internal/sse",
    "headers": {"Authorization": "Bearer <MCP 调用凭据>"},
    "allowed_tools": ["search_dashboards", "query_prometheus"]
  }
]
```

1. 管理员保存配置后可主动检查连接。检查完成 `initialize` 和分页 `tools/list`，展示传输、认证、发现和白名单匹配结果。
2. Agent 从生效配置发现并注册工具。工具名带服务器前缀，并满足模型工具命名约束。目录按配置版本短期缓存，修改、禁用或凭据轮换后失效。旧 `command/args/env` 条目保留可读取性，页面显示首版不支持 stdio。
3. 调用前再次检查用户权限、开关和工具白名单。首版仅对平台管理员开放，外部工具沿用真人审批与审计。审批显示目标服务、工具和脱敏参数。Grafana 按现有部署侧的只读身份查询。
4. 连接、查询及返回内容均设上限。单个外部服务超时、不可达或认证失败时，Agent 继续使用内置工具，向用户说明外部证据缺口。

出站访问由部署侧约束目标与实际拨号地址。旧版 SSE 实现还要验证服务器下发的消息提交地址，默认要求与配置端点同源，并禁止自动重定向，以免认证头被转发到其他主机。远端连接优先使用 HTTPS。现有 HTTP NodePort 如需直接带凭据联调，仅在部署方确认链路为受控内网后显式允许该目标。需要跨网络接入时，使用 HTTPS 入口转发到同一现有服务。

远端描述和结果按不可信数据处理。Secret 明文和平台凭据不能进入工具参数。工具结果经过大小限制和脱敏后进入 Agent。审计记录发起人、会话、工具、审批与结果状态，凭据、敏感参数和远端原文不写入审计。

## 3. CLI 对外提供体检

### 3.1 运行方式

在现有 `aopsctl` 增加 `mcp serve` 子命令。使用 Streamable HTTP 暴露 `/mcp`，进程默认绑定本机回环地址。接入方在可访问 Kubernetes API 的节点或运行环境中手动启动。远程访问由部署方显式开放受保护的 HTTPS 入口。

```bash
aopsctl mcp serve --listen 127.0.0.1:8090 --kubeconfig /path/to/config --context target-cluster
```

命令和参数为拟议接口。启动时固定一个目标集群，凭据可由部署方通过 kubeconfig/context 或 Pod 的 ServiceAccount 注入。CLI 直接复用 [ScanService](../../internal/service/service.go) 和报告模型，独立装配任务管理。常规生产部署、平台 Web 路由、配置中心和报告中心无需承担 CLI 服务的启动或依赖。

### 3.2 工具契约

| 工具 | 请求参数 | 响应 |
| --- | --- | --- |
| `start_inspection` | `scope`、按范围需要的 `namespace` / `pod`、可选 `preset` | 校验后异步启动，返回 `scanId` 和初始状态 |
| `get_inspection_status` | `scanId` | 返回当前状态、阶段进度及脱敏错误，完成后提示可读取报告 |
| `get_inspection_report` | `scanId`、可选视图及分页游标 | 返回结构化体检报告摘要和按需分页的发现项，包含相关证据和采集质量 |

支持的范围沿用体检服务的 cluster、namespace 和 Pod。参数采用白名单校验，部署方可进一步限制允许请求的扫描范围。工具参数没有 Kubernetes 连接地址、kubeconfig、凭据或调用方权限信息。任务和报告共用 `scanId` 关联。报告读取等到任务完成，结果有大小上限。客户端通过分页继续读取，不把整个大报告塞进单次调用。

首版使用单进程、有限并发的异步任务和有界报告缓存，并设置完成态保留期。进程重启后旧任务和缓存报告不可继续轮询，CLI 启动配置会明确这一边界。需要跨重启获取报告时再引入独立持久化。调用方遇到启动请求超时时避免盲目重试，以免重复扫描。

### 3.3 权限与责任

| 责任 | 承担方 | 约束 |
| --- | --- | --- |
| 终端用户能否发起体检、查看报告 | 外部调用端 | 调用端使用自己的账号、授权与审计，用户角色和授权关系无需传给 MCP |
| 可信系统能否连接 MCP、能否读取某个任务结果 | CLI 部署方和 MCP 接入边界 | 远程请求逐次认证调用系统，任务及报告绑定调用系统。凭 `scanId` 无法跨身份读取 |
| 扫描进程实际能读哪些集群资源 | 部署方和 Kubernetes | 部署时配置最小只读身份，实际资源访问受 Kubernetes RBAC 约束。权限不足时返回采集缺口 |
| 执行体检、渲染与输出报告 | AegisOps CLI | 复用体检服务，输出脱敏报告。对外工具限于上述三项 |

远程首版使用部署方配置的独立服务凭据，通过 HTTPS 入口传递。服务凭据只识别调用系统，不映射到 AegisOps 平台用户。CLI 日志记录调用系统、请求范围、`scanId` 和结果状态，不记录凭据或报告正文。扫描身份由进程固定，模型和调用者无法通过工具参数切换。

## 4. 借鉴与代码分层

- **kubectl-ai**。参考 `pkg/mcp/http_client.go`、`pkg/mcp/manager.go` 的连接和工具发现，以及 `cmd/mcp.go` 中由 CLI 独立启动服务的装配方式。
- **HolmesGPT**。参考远端 MCP 的服务器级配置、工具白名单，以及连接失败与单次工具调用失败的处理。
- **AegisOps**。MCP 包负责传输和协议映射，Web 装配出站工具，CLI 装配入站服务。两者复用现有业务服务。实施时验证并锁定支持两种出站传输的 SDK 版本。 [官方 MCP Go SDK](https://go.sdk.modelcontextprotocol.io/protocol/)

## 5. 实施顺序与验收

| 阶段 | 验收条件 |
| --- | --- |
| Web 出站 | 一台本地测试服务覆盖两种传输、分页工具发现、错误凭据、关闭开关、非法 SSE 消息端点、超时及降级。使用现有 Grafana SSE 入口跑通认证和白名单工具调用，并用授权范围内的一次 Pod 重启对齐事件与内存指标 |
| CLI 入站 | 在测试集群手动启动 CLI MCP 服务。Codex 作为外部 MCP 客户端，发现三项工具，发起一次指定 namespace 的体检，轮询并读取结构化报告，最后依据报告说明发现和证据缺口 |
| 边界与回归 | 验证未认证请求被拒绝、调用系统之间不能互读任务、超出只读身份的范围不会被采集。关闭 CLI 进程后，平台 Web 的外部 MCP 接入及普通体检可继续独立工作。实施后运行 `go test ./...`、`go vet ./...` |
