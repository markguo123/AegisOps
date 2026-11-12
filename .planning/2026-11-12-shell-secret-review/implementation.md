# 实施记录：只读命令组合与 Secret 诊断权限优化

- 方案：`docs/02-架构与设计/24-方案-只读命令组合与Secret诊断权限优化.md`
- 日期：2026-11-12
- 状态：代码与自动化测试完成；未做真实集群验收（见 §5 遗留环境项）

## 1. 变更清单

### 1.1 统一命令分析器（新增）

| 文件 | 说明 |
| --- | --- |
| `internal/cmdplan/cmdplan.go` | `Analyze`：mvdan shell AST → `Analysis{NeedsShell, ProvablyReadOnly, Calls, Program, Reason}`；只接受单命令/管道/顺序/条件串联；拒绝重定向、展开、通配符、后台、循环、分组、赋值与 shell 包装 |
| `internal/cmdplan/kubectl.go` | kubectl 只读参数表（含取值范围与动词作用域）、资源/名称提取、`-A`/`-n` 互斥；未知参数一律不放行 |
| `internal/cmdplan/filters.go` | grep/head/tail/sort/uniq/wc/cut/tr 的显式参数表与数值上限；文件参数、写文件、递归读取全部拒绝 |
| `internal/cmdplan/jq.go` | jq 词法 + 白名单标识符校验（拒绝 input/inputs/env/$ENV/debug/halt/import/字符串插值/文件参数）；括号配平 |
| `internal/cmdplan/cmdplan_test.go` | 只读组合、连接语义、调用目标、jq 子集与全部拒绝用例 |

分析器不依赖 approval/tools/executor，避免循环依赖。

### 1.2 审批与权限路径接入

- `internal/approval/risk.go`：新增 `CommandAnalysis` / `AnalyzeCommand`；`ClassifyCommand`、
  `IsReadOnlyCommand`、新增 `NeedsShellApproval` 全部基于统一分析结论。保留 `--dry-run`
  写动词只读的既有路径。
- `internal/approval/decision.go`：新增 `Input.SensitiveAuto` 与 `DecisionSensitiveAuto`
  （`auto_sensitive`）；`RequireHuman` 先于一切自动化通道。
- `internal/tools/kubectl_shell.go`：shell 审批闸门改用 `approval.NeedsShellApproval`。
- `internal/tools/platform_policy.go`：`ProvablyReadOnly` 走新增 `readCallPolicy`，逐条调用
  校验 namespace/资源/动作（read/logs），不要求 `ActionShell`；其余保持原 `argvPolicy` /
  `shellPolicy`。
- `internal/tools/scope.go`：旧范围路径同样按调用逐条校验（`checkReadCall`），不再整体拒绝
  管道。
- `internal/agent/loop.go`：敏感读取工具经 `tools.SensitiveReadAware` 给出
  `AUTO_SENSITIVE` 决策，审计 `match_reason=policy_sensitive_auto`。

### 1.3 执行器

| 文件 | 说明 |
| --- | --- |
| `internal/executor/program.go` | `runReadOnlyProgram`：解析固定二进制路径 → 逐段 `exec.CommandContext` → 内存管道连接 → 按顺序回收退出码；最小环境白名单（`childEnv`）；组合超时；SIGPIPE(141) 视为下游提前结束而非失败 |
| `internal/executor/kubectl.go` | `RunKubectl` 在红线检查后先尝试可证明只读路径（再分析一次，批准后命令不可替换）；Secret 拒绝文案指向 `get_secret_info`/`get_secret_value` |
| `internal/executor/types.go` | `Result.NoMatch`：过滤阶段无匹配不是集群故障 |
| `internal/executor/program_test.go` | 无 bash 的顺序/管道执行、最小环境、拒绝路径不起进程、真实 grep 管道与无匹配（过滤器缺失时跳过） |

`platform.exec.shell_enabled=false` 只影响真实 bash 通道；只读组合照常运行（用例覆盖）。

### 1.4 Secret 诊断

- `internal/config/config.go`：`agent.secret.read_approval`（默认 `always`），校验
  `always|auto`，`off/create_only + auto` 启动报错；配置生成器与配置中心目录同步。
- `internal/tools/k8s/secret_info.go`（新增 `get_secret_info`）：按 namespace 查询元数据
  （名称/类型/immutable/键名），普通 read 权限，字段白名单，始终注册。
- `internal/tools/k8s/k8s.go` / `secret_value.go`：`RegisterOptions.SecretReadApproval`；
  `get_secret_value` 在 `auto` 下不再要求逐次审批，但 `IsReadOnly` 仍为 false，并提供
  `SensitiveReadAuto`；Schema 元数据与描述随模式变化。
- `internal/tools/scope.go`：`get_secret_info` 归入 namespace 读工具（read 权限）。
- `internal/tools/shell.go`：新增 `SensitiveReadAware` 接口。
- `internal/service/chat.go`：透传 `read_approval`。

### 1.5 文档与提示词

- `AGENTS.md`：命令策略改为"按语义判断 + 可证明只读组合自动执行"；Secret 改为
  `read_approval` 两档；补充 `get_secret_info`。
- `internal/agent/prompt.go`：四层意图决策树与交叉规则补充只读组合与过滤器白名单；
  Secret 协议改为"先 `get_secret_info` 再按需取值"。
- `docs/02-架构与设计/08-安全设计.md`：威胁模型、二期边界、`Policy.Evaluate` 决策链
  （新增 `AUTO_SENSITIVE`）、scope 门禁同步修订。
- `docs/02-架构与设计/11-方案-审批系统V2.md`：顶部新增 2026-11-12 修订说明并指向方案 24。
- `docs/02-架构与设计/19-方案-Agent执行与Secret按需诊断-四期.md`：访问模式表与
  外部模型边界按 `read_approval` 修订。
- `docs/02-架构与设计/04-架构决策记录-ADR.md`：ADR-044 增加 2026-11-12 口径更新。
- `docs/02-架构与设计/10-方案-Agent总体架构.md`：顶部增加 2026-11-12 修订说明。
- `docs/05-使用与操作手册/01-技术指导手册.md`：工具集、Shell 构造、配置表与 Skill 口径。
- `docs/04-项目管理与规范/03-变更记录-Changelog.md`：新增条目。
- `README.md`：安全边界、工具集、Secret 配置与核对清单。
- `deploy/helm/aegisops/files/config.yaml`：基线配置补充 `agent.secret.read_approval` 注释。

## 2. 配置迁移说明

- 旧配置无需修改：缺失 `agent.secret.read_approval` 时等效 `always`。
- 需要免逐次审批的部署显式设置 `read_on_demand + read_approval: auto`；把 `access` 改回
  `off`/`create_only` 时必须同时把 `read_approval` 改回 `always`，否则启动校验失败。
- `auto` 不改变平台角色与 Kubernetes RBAC；部署方需确认"外部模型可见按需读取的明文"这一
  数据处理边界。

## 3. 测试结果（本地，无真实集群）

- `go test ./...`：全部通过。
- `go vet ./...`：无输出。
- `tests/arch`：执行点唯一性、Options 装配、只读边界与依赖方向红线全部通过。
- 新增覆盖：
  - `internal/cmdplan`：§7.1 自动执行集合与全部拒绝项；jq 子集；`&&`/`||`/`;` 语义与目标提取。
  - `internal/approval`：只读组合为 SAFE 且不要求 shell 审批；未证明只读的 shell 仍需审批。
  - `internal/tools`：平台策略逐调用授权（越权 namespace、`-A`、Secret、写命令、awk 组合）；
    旧 scope 路径正反向用例；rollout status/history 按 read 放行。
  - `internal/executor`：`shell_enabled=false` 下只读顺序/管道执行、最小环境、拒绝路径不起
    进程、grep 无匹配 `NoMatch`。
  - `internal/tools/k8s`：`get_secret_info` 白名单（annotation/last-applied/值不泄漏）、与
    access 模式无关；`always`/`auto` 两种审批决策。
  - `internal/agent`：`auto` 不弹审批且明文只进当前轮模型请求，回答/会话/审计/SSE 无明文；
    `always` + `skip_permissions` 仍必须真人审批。
  - `internal/config`：`read_approval` 默认值、合法组合与非法/矛盾组合。

## 4. 实际命令验收记录（本地无集群）

未连接真实集群，以下按自动化用例覆盖的等价路径记录，未做真实 API Server 验证：

| 场景 | 覆盖 |
| --- | --- |
| `kubectl get po -A | grep mysql` | `internal/cmdplan` + `internal/tools`（`-A` 需全命名空间 read，无权限拒绝） |
| `kubectl get pods -n team-a | grep -E 'mysql|redis' | head -n 20` | 引号内管道不误判；`cmdplan` + scope 正向用例 |
| `kubectl get pods -n team-a -o json | jq -r '.items[].metadata.name'` | jq 白名单表达式 |
| `&&`/`||`/`;` 连接只读查询 | `cmdplan` 连接语义 + executor 顺序执行 |
| `kubectl rollout status/history` | 分类 SAFE + 平台策略 read 放行 |
| 越权 namespace / 跨集群参数 / 第二条写命令 | 平台策略与 scope 拒绝用例 |
| `shell_enabled=false` 下只读组合 | executor 用例真实执行 |
| grep 无匹配 | `NoMatch` 用例 |
| `read_on_demand + always/auto` | 工具与 Agent 用例 |

## 5. 遗留环境验收项（需真实集群）

1. 真实 RBAC 下 `kubectl get po -A | grep mysql`：有全 namespace 读权限时自动执行、无权限时
   `SCOPE_DENIED`；确认平台策略与 Kubernetes RBAC 的双层结论一致。
2. 真实镜像（含 bash/kubectl/grep/jq）上复跑 §7.1 的组合与失败/取消路径，确认无残余子进程
   （`kubectl get pods | head -n 1` 后 `ps` 干净、SIGPIPE 不误报）。
3. 真实集群上验证 `jq`/`grep` 版本兼容性（本机 Windows 无 grep，用例跳过）。
4. 非交互运行（CronJob/API）下 `read_approval=auto` 的审计字段与配置生效验证。
5. Web UI 上确认审批卡不再为只读组合弹窗，且 `auto` 模式下界面不出现"Secret 一律禁止"的
   静态文案（当前前端没有该文案，未改前端）。
