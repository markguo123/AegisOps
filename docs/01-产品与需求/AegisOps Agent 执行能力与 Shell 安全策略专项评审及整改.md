# AegisOps Agent 执行能力与 Shell 安全策略专项评审及整改

## 一、任务背景

项目：AegisOps

GitHub：https://github.com/markguo123/AegisOps.git

目标分支：`DEV-v4-new`  本地就是最新代码

请作为资深云原生 Agent 架构师、Kubernetes 专家和 Go 工程师，对 AegisOps 当前 Agent 的 `kubectl exec`、Shell、CLI 命令执行、风险分类、权限控制和审批机制进行一次系统性代码评审，并在确认合理方案后完成整改。

**本次任务的核心目标：在不破坏现有多租户权限、安全审批和 Kubernetes 目标隔离机制的前提下，显著增强 Agent 的实际排障能力，降低不必要的命令拦截和人工审批。**

不要机械接受下面的分析。请以 `DEV-v4-new` 分支的最新实际代码为准，重新审查、验证和纠正其中可能不准确的判断。

## 二、产品定位与架构原则

AegisOps 是面向 Kubernetes 及未来更广泛基础设施的智能运维 Agent 平台。

核心产品价值不是严格限制 Agent 可以使用什么命令，而是让 Agent 能够：

1. 自主获取足够完整的故障证据。
2. 使用必要的诊断工具进行深入排查。
3. 基于真实证据分析和定位根因。
4. 在授权范围内执行低风险排查操作。
5. 对有风险的变更提供审批与执行机制。
6. 执行后验证结果，并形成完整的运维闭环。

当前架构已有以下基础能力：

- Agent Loop：LLM 驱动多轮 Reason → Tool → Observe。
- Tool Registry：统一工具注册与调用。
- Client-go：Kubernetes 资源读取、扫描和结构化操作。
- cmdplan：Shell AST 分析及可证明只读的命令组合执行。
- ApprovalPolicy：风险分类、人工审批及会话授权。
- AccessScope / Authz：平台级用户与 Kubernetes 资源授权。
- Executor：kubectl、Shell 和 Kubernetes API 执行。
- TargetBound：会话集群与凭据绑定。
- Code Guard、Secret 按需读取、审计及输出脱敏。

这些机制都是需要优先复用的既有能力。

**不要为了本次优化重写 Agent Runtime，也不要将 Client-go 全面替换成 LLM 驱动的 kubectl。**

Client-go 继续负责确定性资源访问和巡检扫描；LLM 驱动的 kubectl、exec、Shell 负责灵活排障，两者互补。

## 三、目前发现的典型问题

### 3.1 kubectl exec 限制过于严格

实际遇到以下命令：

```
kubectl exec app-backend-b664b9766-45c2z -n test-complex -- ls /usr/share/nginx/html/
```

返回：

```
SCOPE_DENIED: cannot prove the complete target
```

前端显示：

```
当前平台授权范围不允许该操作
```

这是一条正常的业务容器目录排查命令。

前期静态代码评审发现：

`internal/tools/platform_policy.go` 中的 `argvPolicy()`，对 `kubectl exec` 后面的命令设置了非常狭窄的白名单。

主要允许：

- hostname
- uptime
- id
- ps（部分固定参数）
- df（部分固定参数）

不支持常见的 `ls`、`stat` 等诊断命令。

问题在于：

**命令不在自动执行白名单，不代表用户越权，也不代表该命令必须被永久拒绝。**

请确认这是否为截图报错的实际根因，并检查其他类似问题。

不要仅仅添加 `ls` 就结束整改，需要从整体诊断能力和授权流程进行设计。

### 3.2 Shell 权限与平台授权耦合过重

重点检查：

```
internal/authz/access.go
internal/tools/platform_policy.go
internal/tools/platform_shell.go
internal/tools/scope.go
```

此前评审发现，`ActionShell` 可能要求集群级权限，以及全 Namespace 的 read/logs/operate/destroy 权限。

这会导致普通运维用户即使已获得特定 Namespace 的排障权限，仍然无法使用某些 Shell 能力。

需要重新评估：

- Shell 语法是否必须绑定集群级授权？
- 只读 Shell 管道是否应该使用普通资源读取权限？
- Namespace 范围内的容器诊断是否应该只依赖对应的 `exec` 授权？
- 是否存在把命令能力限制误判为平台权限不足的情况？

必须区分：

**资源授权范围、工具使用权限、命令风险与执行环境限制。**

它们不应该混为一谈。

### 3.3 风险分类过于粗糙

重点检查：

```
internal/approval/risk.go
internal/approval/decision.go
internal/approval/grant.go
internal/tools/write.go
```

当前 `kubectl exec` 基础风险统一为 HIGH。

例如：

```
kubectl exec pod-a -n team -- ls /app
```

与：

```
kubectl exec pod-a -n team -- rm -rf /app/data
```

不应该具有完全相同的自动执行资格。

但必须注意：

`pods/exec` 本身是一种强执行能力，Kubernetes RBAC 并不能限制它只能运行 `ls`。

因此，需要将工具的基础敏感性与具体命令的风险分开建模。

建议研究如何实现：

- 明确授权的常见诊断命令可自动执行。
- 不符合自动执行规则，但在合法授权范围内的命令，进入人工审批。
- 已确定越权或违反硬安全边界的命令直接拒绝。
- 未知命令不应直接被当成普通只读命令。
- 人工审批不能替代平台授权或 Kubernetes RBAC。

### 3.4 TargetBound 与 Shell 执行存在冲突

重点检查：

```
internal/executor/kubectl.go
internal/executor/parse.go
internal/executor/program.go
internal/executor/runner.go
internal/cluster/target.go
internal/service/chat.go
```

此前评审发现：

平台模式通过 TargetBound 绑定目标 Kubernetes 集群。

当命令包含不能转换为受控 argv 程序的 Shell 结构时，即使已经经过人工审批，执行器仍可能直接拒绝。

需要明确：

1. 哪些命令可以通过受控 argv 执行？
2. 哪些命令可以在不调用 Bash 的情况下完成？
3. 哪些命令必须使用真实 Shell 解释器？
4. 当前 TargetBound 为什么拒绝真实 Shell？
5. 哪些限制是必须保留的安全边界？
6. 在不破坏目标隔离的前提下，哪些排障能力可以恢复？

**禁止简单关闭 TargetBound、允许任意 kubeconfig/context 切换，或者让任意 Shell 继承平台主进程的全部凭据。**

如果某类自由 Shell 无法在当前架构下安全实现，应明确保留限制，而不是通过降低安全等级强行放行。

### 3.5 排查能力存在不一致

请重点检查以下命令与其对应的解析、授权、风险分类、执行流程：

```
kubectl version
kubectl api-resources
kubectl auth can-i get pods -n test-complex
kubectl get pods -n test-complex | grep backend
kubectl get pods -n test-complex -o json | jq '.items'
kubectl get <CRD资源> -n test-complex
kubectl exec <pod> -n test-complex -- ls /app
```

此前发现：

- cmdplan 与平台 Scope Guard 对某些命令的支持范围不完全一致。
- CRD 动态资源查询可能受内置 Kind 白名单限制。
- 某些只读命令在执行器层允许，但在平台授权层无法通过。
- Agent Prompt 声明的能力与实际执行能力可能不一致。

请系统排查命令分类、平台授权、审批和执行器之间的行为不一致，而不是只检查上述示例。

### 3.6 错误分类与前端反馈不准确

目前前端可能将各种 `SCOPE_DENIED` 统一展示为：

```
当前平台授权范围不允许该操作
```

但真实原因可能是：

- 平台没有授权。
- Kubernetes RBAC 不足。
- 命令不在自动执行白名单。
- 命令无法完成目标解析。
- TargetBound 不允许执行。
- 审批策略拒绝。
- Shell 执行环境不可用。

建议采用结构化错误码，至少区分：

```
SCOPE_DENIED
ACTION_DENIED
RBAC_DENIED
POLICY_DENIED
APPROVAL_REQUIRED
TARGET_ISOLATION_DENIED
COMMAND_UNSUPPORTED
EXECUTION_UNAVAILABLE
```

具体命名以项目现有错误契约为准，避免重复建设。

Agent 收到错误后应知道是需要重新生成命令、请求审批、选择替代工具，还是告知用户缺少授权。

不要让模型反复重试根本不可能成功的命令。

### 3.7 审批链路可能存在 fail-open 风险

重点检查：

```
internal/agent/loop.go
```

此前静态评审发现：

当策略要求人工审批，但 Approver 为 nil 时，部分普通工具调用可能没有显式硬拒绝路径。

这只是静态代码评审发现的潜在风险，尚未经过完整运行测试验证。

请确认真实调用链是否存在该问题。

如果确实存在：

**需要人工审批但审批组件不可用时，一律 fail-closed，不允许继续执行。**

同时验证 skip_permissions、require_approval=false、会话授权缓存等机制不会绕过必须人工审批的敏感操作。

这项属于安全优先整改内容。

## 四、重点调研 HolmesGPT 的实际设计

这是本次整改的主要参考方向。

请直接研究 HolmesGPT 最新开源代码和官方设计，而不是仅根据历史文档判断。

参考：

https://github.com/HolmesGPT/holmesgpt

https://holmesgpt.dev/

重点研究以下两部分。

### 4.1 HolmesGPT Bash Toolset

重点分析：

- core / extended 命令白名单。
- Shell AST 或命令解析机制。
- 管道、重定向、变量展开及命令替换处理。
- 命令参数安全验证。
- 命令不在白名单时如何请求审批。
- 危险命令如何拒绝。
- 是否支持配置扩展命令。
- 不同部署模式下审批能力的差异。

研究其实现，而不是直接照搬命令列表。

### 4.2 Kubernetes Remediation MCP

重点参考：

https://github.com/containers/kubernetes-mcp-server

以及 HolmesGPT 对应的 Kubernetes Remediation MCP 集成文档。

前期调研显示，该方案可能包含以下能力，请以最新实际源码核实：

- `run_preapproved_kubectl_command`
- `run_kubectl_command`
- `read_file_from_container`
- `run_preapproved_diagnostic_image`

重点分析：

1. 如何在目标业务 Pod 内执行命令？
2. 哪些 Pod 内诊断命令可以自动执行？
3. 是否通过命令和参数白名单控制？
4. 如何限制容器内文件读取范围？
5. 如何处理符号链接、Secret 挂载目录、敏感文件？
6. 未预批准的命令如何进入人工审批？
7. 如何校验 Kubernetes RBAC？
8. 如何避免工具命令绕过授权？
9. 是否区分普通诊断执行与高权限 Shell？
10. 哪些能力是默认开启，哪些需要额外部署配置？

请特别分析它的 Pod 内 `exec` 设计。

不要将 HolmesGPT 普通 Bash Toolset 与 Kubernetes Remediation MCP 混为一谈。

也不要假设它们一定共享相同的命令白名单和审批策略。

**目标是提炼成熟设计并融合到 AegisOps 现有架构，而不是直接引入整个 HolmesGPT 或 MCP Server。**

如果某些现成实现确实成熟 可直接复制

## 五、建议的整改方向

以下是架构建议，不是必须照抄的最终实现。

请结合实际代码独立评审。

### 5.1 保留现有 Client-go 体系

不要将 Client-go 结构化工具全面改成 kubectl。

保留：

- 资源读取。
- 集群体检。
- 扫描引擎。
- 规则引擎。
- 证据链与故障关联。
- 结构化资源创建、修改及验证。

灵活排障能力通过 kubectl、受控 exec 和 Shell 补充。

### 5.2 建立更加合理的命令执行策略

建议至少区分三种决策：

**AUTO：**

已授权、可证明符合低风险诊断规则的命令，自动执行。

**REQUIRE_APPROVAL：**

目标和平台授权合法，但命令存在不确定性或潜在副作用，需要人工审批。

**DENY：**

明确越权、违反目标隔离、命中安全硬限制，或当前执行环境无法安全承载的操作。

特别注意：

不能证明只读，不代表一定可以经审批执行。

如果无法保证目标隔离，即使人工批准，也应该继续拒绝。

### 5.3 改善 Pod 内排障能力

重点解决这些常见需求：

- 查看容器文件目录及属性。
- 检查业务进程。
- 查看磁盘与内存状态。
- 诊断 DNS、网络及连接状态。
- 检查应用部署文件。
- 有条件地读取非敏感配置文件。
- 执行符合策略的有限非交互诊断命令。

可以评估新增 `exec_pod` 结构化工具，也可以优化现有 kubectl 工具。

请比较两种方式，选择复用度高、改造小、授权边界清楚的方案。

不要求为了设计美观而增加一套重复工具体系。

### 5.4 扩展只读诊断命令支持

可参考：

```
ls
stat
pwd
id
hostname
uptime
ps
df
du
ss
netstat
```

但不能简单按命令名称自动放行。

必须评估：

- 具体参数是否安全。
- 是否读取敏感数据。
- 是否可以触发子进程。
- 是否可以写入文件。
- 是否可以访问未授权资源。
- 是否会产生长时间阻塞。
- 是否存在参数绕过、路径遍历或符号链接风险。

对于 `cat`、`find`、`curl`、`wget`、`sh -c` 等能力，应根据真实风险单独处理，不得直接纳入普通自动执行白名单。

如果某些命令的副作用无法可靠控制，就默认审批或拒绝。

### 5.5 优化人工审批体验

目标：

- 常见低风险诊断不反复弹窗。
- 高风险操作仍需明确确认。
- 授权内的复杂排障命令可以得到准确的审批或拒绝结论。
- 审批卡显示实际操作目标和风险原因。
- 会话授权不能越过原有权限边界。
- 用户拒绝后 Agent 应寻找安全替代方案。
- 不改变已有的硬拒绝和审计规则。

尤其要避免所有 exec 都显示 HIGH，但用户无法理解具体风险的情况。

### 5.6 保留核心安全边界

必须保留：

- 多租户用户与资源授权。
- Cluster / Namespace 范围控制。
- Kubernetes RBAC。
- TargetBound。
- 执行前权限与目标重验。
- Secret 安全读取机制。
- 高风险人工审批。
- Code Guard。
- 命令超时、输出限制、脱敏。
- 审计与执行结果记录。

不允许通过关闭这些机制来解决正常命令被拒绝的问题。

## 六、本次明确不实施的内容

经过架构讨论，当前暂不引入：

1. 每个 Agent 任务独立 Kubernetes Sandbox。
2. 新的 Sandbox Worker 调度系统。
3. Agent Runtime 全面微服务化。
4. Web 与 Agent Runtime 的大规模拆分。
5. 将所有 Client-go 工具迁移到独立进程。
6. 全面重写 Agent Loop。
7. 将整个 Agent 执行体系改造成纯 kubectl 驱动。

原因：

AegisOps 当前的主要问题是 Agent 执行能力受到过度限制，而不是已经确认存在无法承受的并发瓶颈。

目前优先借鉴 HolmesGPT，优化现有执行能力。

未来如果发现 Agent Runtime 的并发、内存或任务隔离问题，再单独评估 Worker 水平扩容。

如果执行器确实需要少量接口调整以支持未来扩展，可以提出，但不要为尚未确定的 Sandbox 架构提前进行大规模重构。

## 七、工作流程要求

### 第一阶段：代码深度审查

先检查目标分支及工作区状态，避免覆盖已有未提交修改。

完整梳理：

```
Agent Loop
    ↓
Tool Registry
    ↓
命令解析 / 风险分类
    ↓
AccessScope / 平台授权
    ↓
Kubernetes RBAC
    ↓
ApprovalPolicy
    ↓
Executor / TargetBound
    ↓
Kubernetes API / Pod exec
    ↓
结果处理 / 审计
```

对于每一处发现，输出：

- 实际代码文件与函数。
- 当前运行逻辑。
- 可复现的触发命令。
- 预期行为与实际行为。
- 根因。
- 风险及影响。
- 建议修改方式。

明确区分已经验证的事实与推断。

### 第二阶段：竞品方案验证

研究 HolmesGPT 最新实现。

输出：

- 可复用的设计。
- 不适合直接复用的设计。
- 与 AegisOps 架构的差异。
- 推荐采用的方案及原因。

不要仅仅引用 README，需要尽可能核对真实源码与测试。

### 第三阶段：整改设计

形成正式整改方案。

重点说明：

1. 是否新增 `exec_pod`。
2. 是否需要重构 `argvPolicy()`。
3. 如何统一命令解析和风险分类。
4. 如何处理 Shell、容器命令与 kubectl 的差异。
5. 如何让自动执行、人工审批、硬拒绝三者正确分流。
6. 如何保留 TargetBound 与多租户权限。
7. 如何优化错误码及前端反馈。
8. 哪些现有组件可以直接复用。
9. 哪些修改属于必要整改，哪些属于不必要的过度设计。

优先选择改造范围小、实现成熟、长期可维护的方案。

### 第四阶段：实施整改

在完成代码评审和方案设计后，对可以明确确认的缺陷直接实施整改。

要求：

- 只修改与本次任务相关的代码、配置和文档。
- 尽量复用已有接口和组件。
- 保留现有功能行为与安全约束。
- 不新增无必要的独立服务。
- 不引入大规模破坏性重构。
- 不修改无关业务模块。
- 不自动提交、推送代码。
- 不静默改动生产部署权限。

如果发现涉及重大安全取舍、无法兼容的接口变更或必须放宽执行边界的决策，请先列出方案及风险，等待确认，不要自行降低安全标准。

### 第五阶段：测试和验收

必须补充覆盖以下场景的测试：

**正常排障：**

- kubectl get / describe / logs。
- 受控管道 grep / jq / head。
- 常见 Pod 内非交互诊断。
- Pod 内文件与目录查询。
- CRD 资源查询。
- kubectl auth can-i。
- 只读 rollout status/history。

**权限边界：**

- 普通用户只能操作授权 Namespace。
- 缺少 exec 授权时拒绝。
- Kubernetes RBAC 不足时拒绝。
- 禁止越权访问其他 Namespace。
- 禁止覆盖 kubeconfig/context/server。
- 撤销权限后执行前必须重新校验。

**风险与审批：**

- 合法低风险诊断自动执行。
- 不确定命令进入合理的审批或拒绝路径。
- 写操作继续遵守审批规则。
- 无 Approver 时必须 fail-closed。
- skip_permissions 不能绕过必须审批的操作。
- 高风险命令不能通过参数伪装为只读。
- 复杂命令不得绕过 Secret 安全策略。

**执行与稳定性：**

- 命令超时。
- 非零退出码。
- 输出超限。
- 用户取消。
- 执行目标变更。
- 命令参数异常。
- Shell 环境不可用。
- 子进程清理。
- 失败后的 Agent 重试策略。

尽可能执行 Go 单元测试、集成测试与架构约束测试。

如果没有真实 Kubernetes 集群，必须明确说明哪些用例仅通过 Mock 或单元测试验证，不得声称已完成真实集群验收。

## 八、最终交付

请最终输出：

### 1. 架构评审结论

说明 AegisOps 当前 exec/Shell 的核心问题，以及哪些限制合理、哪些过度保守。

### 2. HolmesGPT 对标结论

说明哪些机制值得借鉴，哪些不建议照搬。

### 3. 最终整改方案

清楚说明执行策略、授权策略、审批策略与目标隔离的设计。

### 4. 实际代码变更清单

逐项列出：

- 修改文件。
- 修改目的。
- 关键逻辑。
- 是否改变原有行为。
- 风险和兼容性。

### 5. 测试结果

明确已通过、失败、未执行以及需要真实集群验证的内容。

### 6. 遗留问题

只保留确实无法在本次范围内完成的问题，避免扩大项目范围。

## 九、最终强调

AegisOps 的核心竞争力是 Agent 的真实运维能力。

**不能因为过度保守的命令白名单，使 Agent 连正常查看容器目录、进程和网络状态都做不到。**

但也不能为了追求类似 Codex 的自由执行体验，而忽略生产 Kubernetes 环境的真实权限边界。

本次整改应当学习 HolmesGPT 的成熟设计：

**尽可能自动完成授权内的低风险排障；对存在风险但合法的操作进行审批；对真正越权或无法安全执行的操作坚决拒绝。**

不要把命令不在白名单等同于用户没有权限。

不要将人工审批等同于可以绕过目标隔离。

不要为了补齐执行能力而重新开发一套 Agent Runtime。

请从实际代码出发，独立评审、确定最小合理架构、完成必要整改，并以真实测试结果作为最终验收依据。
