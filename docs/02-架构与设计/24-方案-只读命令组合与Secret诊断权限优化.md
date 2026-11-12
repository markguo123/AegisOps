# 只读命令组合与 Secret 诊断权限优化

日期：2026-11-12  
状态：已实施（2026-11-12），实施记录见 `.planning/2026-11-12-shell-secret-review/implementation.md`，待代码审查  
范围：本次只改命令策略与 Secret 诊断体验；独立 Pod/Job 沙箱后置。

## 1. 目标与决策

1. `kubectl get po -A | grep mysql` 等可证明只读的命令组合默认自动执行。
2. 根据完整命令的语义、参数和目标判断权限，不根据是否出现 shell 符号判断风险。
3. Secret 元数据、键名、引用关系可按普通读取权限查询；明文读取能力和审批策略分离。
4. 明文继续使用专用模型数据通道，不开放普通 kubectl/shell Secret 内容输出。
5. 不迁移所有作业，不建设沙箱，不引入任意脚本自动执行，不扩大用户的 Kubernetes RBAC 或平台授权。

## 2. 已确认的现状

- `internal/approval/decision.go` 把所有 Shell 标记直接送入真人审批。
- `internal/approval/risk.go` 把含 shell 语法的命令排除在只读之外。
- `internal/tools/platform_policy.go` 对 shell 额外要求 ActionShell。
- `internal/tools/platform_shell.go` 已有 mvdan shell AST，但过滤阶段仅支持窄形式 head/tail。
- `internal/tools/scope.go` 的旧范围检查路径直接拒绝 shell，必须同时覆盖。
- `internal/executor/kubectl.go` 在 Backend 内启动 bash/kubectl，继承整个进程环境。
- `agent.secret.access` 默认 read_on_demand；工具注册、平台 secret.read 权限、逐次真人审批相互独立。
- `get_secret_value` 限定具体 Secret 与 key，单次最多 5 个 key，并有内容体积限制。
- 明文使用 ModelOnly，仅进入紧接工具结果的当前一次模型请求，其他出口脱敏。

这是源码结论，不代表已经复现用户某次失败；实施时记录实际被拒层及错误码。

## 3. 命令策略

### 3.1 统一分析结果

复用 mvdan.cc/sh/v3/syntax，建立一份无业务入口依赖的命令分析结果。按现有包依赖选择位置，不制造 approval/tools/executor 循环依赖。

结果至少区分：是否需要 shell 解释器、是否可证明只读、是否有未知行为、是否必须真人审批、硬拒绝原因、最高风险，以及每条 Kubernetes 调用的 argv 和目标。

审批、平台与旧范围校验、执行器均复用这个分析结果或同一分析器。不能各自用字符串切分生成互相矛盾的判断；执行前须对实际执行内容重新校验，批准后命令不可替换。

### 3.2 自动执行集合

- 顶层允许单命令、普通管道 `|`、顺序 `;`/换行、条件串联 `&&`/`||`，每条分支都必须通过检查。
- 每个查询分支以明确的 kubectl 只读命令开始；所有 Kubernetes 调用分别检查目标集群、namespace、资源与所需动作。
- 沿用现有已支持的只读动词及 rollout history/status；分类、范围解析、执行支持必须一致。不能仅为放行管道新增不受范围约束的命令。
- 不因 `--dry-run` 字符串出现就自动认定安全；本次组合自动执行不包含写动词 dry-run，现有独立 dry-run 路径另保留测试。
- 首批过滤器：grep、head、tail、sort、uniq、wc、cut、tr、jq。仅处理当前管道 stdin；不得读取额外文件、写文件或启动子命令。
- 明确定义各过滤器支持的参数表及数值上限；不认识的选项、缩写和参数形式不得自动放行。grep 的模式参数与文件参数必须正确区分，支持 `grep mysql`、`grep -i mysql`、`grep -E 'mysql|redis'` 等正常用法。
- jq 支持单个字面量过滤表达式及经校验的常用输出选项（如 -r/-c）；拒绝文件参数、-f、--argfile、--slurpfile、--rawfile、模块导入、input/inputs 等额外输入能力。表达式须可靠校验，不以不完整关键词黑名单冒充安全解析；必要时收敛为明确支持的表达式子集并给出替代查询提示。
- 首批不自动放行 awk、sed、xargs、find -exec、解释器、任意本地文件读取、curl/wget 等外连命令。
- AST 中有动态命令名、变量展开、命令/进程替换、通配符展开、重定向、后台任务、循环、函数、环境赋值或 shell 包装器时，不进入自动执行集合。
- 引号内普通字符不能误判：grep 模式和 jq 表达式内的 `|` 不是新增 shell 命令。

执行建议作为本次要求：对上述可证明只读的语法树编译为受控 argv 管道与顺序/条件执行，由 executor 使用固定二进制路径运行，不把已校验文本重新交给任意 bash 解释。临时中间结果仅在内存/受控管道传递，保留退出码语义；管道失败不能掩盖 Kubernetes 查询错误，grep 无匹配应返回清晰的无匹配结果。

因此安全只读组合不依赖 bash，也不受 platform.exec.shell_enabled=false 阻挡；该开关继续只控制真实 bash 执行通道。工具说明和部署文档必须同步解释这个区别。

### 3.3 权限与审批

| 分析结果 | 权限 | 审批 |
| --- | --- | --- |
| 可证明只读的单命令/组合 | 每条查询所需 read/logs 等权限 | 自动，无须额外 ActionShell |
| 含明确写操作的组合 | 各查询/变更目标权限；真实 shell 仍要求 ActionShell | 保留真人审批，不因前段只读而放行 |
| 行为未知但目标可被现有策略可靠约束 | 保留既有权限门禁 | 真人审批 |
| 目标范围无法证明 | 拒绝 | 审批不能覆盖越权或范围未知 |
| 命中现有硬禁止项 | 拒绝 | 不可通过审批放开 |

--skip-permissions 不把未知 shell、非交互 exec 或硬拒绝变成自动执行；保留普通非 shell 写操作的现有自动化契约。本次不扩展 shell 写操作授权缓存。

完整命令若含写或未知行为，不能先自动执行前面的查询再等待批准；先完成整条检查和必要审批。现有受审批的 heredoc/apply 等能力保持兼容，不能被只读子集的语法限制误删。

继续禁止 --raw、直接 kubectl Secret 访问、现有交互式命令、exec -i/-t 等。非交互 exec 每次真人审批。禁止目标/身份/凭据覆盖参数及插件、配置注入绕过（如 --kubeconfig/--server/--token/--as/--context）；使用任务绑定的目标与凭据。

## 4. Secret 查询与授权

### 4.1 配置契约

```yaml
agent:
  secret:
    access: read_on_demand # 原有 off | create_only | read_on_demand
    read_approval: always # 新增 always | auto，缺省 always
```

- access 继续控制明文读取/创建能力；不新增宽泛 read_write。
- always：每次 get_secret_value 都需要真人审批，沿用旧行为。
- auto：管理员已显式授权在现有用户、任务与集群权限范围内按需读取；不逐次弹窗。可用于交互及非交互运行，但每次必须做 secret.read、目标绑定、配置与 RBAC 校验。
- 缺省/旧配置保持 always；非法值启动校验失败。off/create_only + auto 不得启用明文读取。
- --skip-permissions、require_approval=false 和普通授权缓存均不能代替 read_approval=auto；always 模式不能被这些入口绕过。
- auto 仍标记为敏感读取，不把 IsReadOnly=false 简单改为 true 后依赖普通写操作免审逻辑；Agent 需要明确的敏感读取审批决策。
- Helm、安装配置、配置生成器及实际已有配置入口同步透传；不得顺手新增无关配置管理页面。
- 配置说明明确：auto 表示允许外部模型接收按需读取的明文；此授权不改变平台角色和 Kubernetes RBAC。

### 4.2 元数据与发现路径

提供专用结构化 Secret 信息查询能力（可复用既有 reader），支持按 namespace 列出名称/类型/immutable，以及单个 Secret 的键名、已有诊断所需的引用关系。没有必要为查询名称批量读取所有内容。

元数据查询使用 read 权限，不要求 secret.read、不弹敏感读取审批，也不依赖 access=read_on_demand。off 关闭的是 Secret 创建/明文能力，非资源存在性信息；同步更新旧注释，避免歧义。

只输出字段白名单。不得直接返回完整 ObjectMeta/annotations、data/stringData、last-applied 配置或任意可能携带明文的字段。若 client-go API 返回 Secret 对象，立刻裁剪，值不得进入工具结果、Evidence 或日志；键名发现不输出值。

明文仍只允许 get_secret_value，必须明确 namespace/name/keys/reason，沿用 key 数量与字节上限；不支持读取整个 namespace、通配 key 或自动 dump 全部 Secret。模型先发现相关 Secret/key，再按需取值，不要求预先证明确定因果关系；认证失败、Secret 引用缺失、配置格式核查等合理诊断线索即可。

### 4.3 明文生命周期

- always/auto 只改变审批决策，复用同一执行与 ModelOnly 通道。
- 明文只进入当前一次模型请求，不进入后续轮次或摘要请求。
- UI/SSE、会话文件、数据库、审计、最终回答、普通工具结果和大结果缓存均禁止明文；沿用精确值及已有编码形态脱敏。
- 审计记录敏感读取、审批来源（human/config_auto）、操作者、任务/集群、Secret 定位、key、长度与指纹；不记录值。
- 后续确需再次读取可按策略重新调用；本次不新增跨轮明文缓存或任务级 Secret 授权状态。
- 普通 kubectl 请求 Secret 时返回可指导的结构化错误，指向元数据工具或 get_secret_value；不把 kubectl Secret 输出偷偷转接普通结果通道。
- UI 动态提示正确表达 always/auto 和当前可用工具，不能配置开启后仍笼统告知“Secret 一律禁止”。

## 5. 执行器最低限度加固

本次不建设沙箱，但必须消除为自动命令继承 Backend 全量环境的必要性：使用最小环境白名单，仅带必要 PATH/语言环境、已确认需要的网络/证书配置及任务绑定凭据；不继承数据库密码、模型密钥和其他集群凭据。代理/证书兼容项显式定义并测试。

可证明只读路径禁止继承 KUBECTL_EXTERNAL_DIFF、BASH_ENV、ENV 等可改变命令行为的变量。真实 bash 通道的兼容处理也须明确。

整条组合共用任务超时与输出上限，取消时清理全部子进程与管道；grep/jq 等过滤过程不能无限运行或无限缓冲。处理 partial output、stderr、无匹配和各段退出码。不同任务不得共用可写工作目录或缓存凭据。

## 6. 实施拆分

1. 统一 AST 分析器与受控只读执行路径，完善参数表、权限目标提取和测试。
2. 接入审批、平台 policy 和旧 scope 两条权限路径；保留受审批的写 shell。
3. 增加 Secret 信息查询、read_approval 配置与明确的敏感读取决策，复用 ModelOnly。
4. 同步提示词、工具说明、错误码、配置示例、部署透传与架构文档。
5. 更新 AGENTS.md 中“所有 shell 必须审批”和“所有 Secret 明文必须逐次真人审批”的旧规则：分别改为可证明只读组合可自动执行、auto 显式配置授权；保留其他安全边界。同步查找对应 ADR/既有方案，只修订相关条款并指向本方案。
6. 完成自动测试与本地验收，提交实施记录和遗留环境验收项供 Codex 审查。

仅修改本方案相关文件，保留已有部署/存储改动。不得把工作区已有变更混入本次实现。没有可用集群时明确标注未验证，不伪造真实环境结论。

## 7. 必须覆盖的验收

### 7.1 Shell

- `kubectl get po -A | grep mysql`：有全 namespace 读取权限时自动执行；无权限时拒绝。
- `kubectl get pods -n team-a | grep -E 'mysql|redis' | head -n 20`：自动执行；引号中的管道不能误判。
- `kubectl get pods -n team-a -o json | jq -r '.items[].metadata.name'`：自动执行。
- 两个各自获权的只读查询用 &&、||、; 或换行连接：自动执行且语义正确。
- rollout status/history 等现有只读能力覆盖平台与旧 scope 路径，不发生分类放行但范围层拒绝。
- 只读组合在 shell_enabled=false 时正常运行，真实 bash 命令仍按配置拒绝。
- 不带 shell 权限但有 read 权限的用户可运行只读过滤；不能借此获得写、exec、Secret 明文权限。
- 管道中的任意越权 namespace、跨集群覆盖参数、第二条写命令均被整体发现。
- grep 文件参数、sort -o、jq 文件读取/模块/额外输入、awk system、sed 写文件、xargs 执行、重定向、命令替换和嵌套 bash 均不得自动执行。
- 非交互 exec 保持逐次审批；硬禁止项无法被审批、auto 或 skip 绕过。
- 原有 approved apply/heredoc 等可用能力不退化，旧单条 kubectl 不回归。
- 超时/取消后无残余子进程；超量输出受限；无匹配不伪报集群故障；stderr 和失败查询不被后置过滤隐藏。

### 7.2 Secret

- 旧配置缺失 read_approval：always；default/非法值/配置生成及部署透传均有测试。
- off/create_only 下元数据可按 read 权限查询，但 get_secret_value 不暴露且不能调用明文。
- 只读元数据用户不能获得明文；无 read 权限者不能枚举 Secret。
- read_on_demand + always：每次真人审批，skip/免审配置无效；无人审批入口明确报错。
- read_on_demand + auto：有 secret.read 权限时不弹审批，能完成真实工具调用到当前模型请求的链路；无权限仍拒绝。
- always 和 auto 均验证 UI/SSE/会话/DB/审计/最终回答/大结果缓存/下一次模型请求/摘要无明文。
- 元数据包含恶意 annotation/last-applied/Secret 值时，白名单输出不泄漏；资源字段指令不影响策略。
- 空 key、超量 key、超限内容、跨 namespace、集群/凭据变更、失败与取消路径保持约束和清理。
- 非交互 auto 可执行但仍全程受限；always 不得在无人审批时静默放行。

### 7.3 检查与交付

运行相关单元/集成/架构测试、`go test ./...`、`go vet ./...`；如修改前端，执行仓库要求的类型检查与构建，并检查构建产物未污染已有工作区。

提交变更清单、配置迁移说明、自动测试结果、实际命令验收记录；未做真实集群验收必须列出。测试不能只 mock 分类函数，至少覆盖 Agent 决策→范围检查→executor 的只读组合链路，以及 Secret auto→ModelOnly→各出口脱敏链路。

## 8. 借鉴边界与后续

本地 HolmesGPT 的 bash validation/default_lists/shell_parser 提供“完整解析、逐段检查、参数校验”借鉴；不直接照搬其宽泛前缀允许策略。kagent 默认 K8s 工具与 Skills 沙箱是不同执行路径，本次不引入其 runtime 或沙箱依赖。若复用实际代码，记录来源并保留许可证。

后续另立沙箱方案，优先隔离 shell/kubectl 执行面，再讨论任务级临时 Pod、后台 Job、短期凭据与访问代理；不作为本方案前置条件。
