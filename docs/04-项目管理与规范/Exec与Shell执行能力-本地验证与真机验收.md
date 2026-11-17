# Exec 与 Shell 执行能力：本地验证与真机验收

日期：2026-11-17

设计依据：[方案 26](../02-架构与设计/26-方案-Agent%20Exec与Shell执行能力.md)。

状态：本地实现与自动化验证完成；以下真实集群项目待用户执行。本文不代表已完成真机隔离验收。

## 1. 本次交付

- `exec_pod` 结构化诊断；kubectl exec 使用相同诊断规则和 client-go 执行路径。
- 逐次真人批准、上下文绑定的单次许可、重放拒绝和目标状态重验。
- namespace/exec 资源授权、pods/exec RBAC、平台 API 请求守卫。
- 目录路径、敏感挂载、特权 Pod、远端程序和参数边界。
- 超时、共享输出上限、会话/用户/实例并发限制和明确错误分类。
- version/api-resources、受限 auth can-i 的授权一致性；未知资源经绑定 discovery 解析后仍须满足平台资源目录。
- 模型能力提示、审批预览、前端错误提示、审计执行元数据和静态配置模板。

普通 Pod exec 不承载文件内容、Secret、解释器或自由脚本；平台真实 Shell 不开放。安全只读组合继续独立于 Bash 开关执行。

## 2. 本地检查

交付检查包括：

```text
go test ./...
go vet ./...
npm run typecheck  (web)
npm test           (web)
npm run build      (web)
git diff --check
```

专项测试覆盖：无 Approver 零执行、批量与会话批准不能替代本次确认、直接调用无许可、许可重放、批准后参数变化、Pod UID/容器重启/凭据变化、敏感挂载及符号链接、路径解析失败、递归访问敏感子目录、并发限制、输出限制、取消、两入口等价、禁用开关、deny_ops、HTTP 权限守卫、SAR 子资源字段及 discovery 不扩大授权。

上述检查均通过。前端测试 241 项通过；构建存在既有大包体提示，不影响构建成功。未自动提交、推送或部署，未执行真实 Kubernetes 操作。

## 3. 测试准备

使用测试集群，准备两个集群 A/B 和两个 namespace TEAM_A/TEAM_B。选择运行中的普通 Pod，记录真实 Pod 与容器名称；不要从 Pod 名猜测工作负载名。

- 普通测试用户仅获得集群 A、TEAM_A 的 read/operate/exec 授权。
- 另备无 exec 资格的只读用户，以及 Admin。
- Kubernetes 执行身份需要 `get pods` 和 `create pods/exec`；普通读取与日志权限沿用现有角色。
- 准备包含 `ls/readlink/df/id` 的普通容器；准备多容器 Pod、缺少诊断程序的容器以及包含 Secret 挂载的测试 Pod。
- 通过现有部署流程部署本地构建；前后端使用同一版本。

配置默认开启 exec_pod，但所有调用仍需批准。需要关闭时在静态配置设置 `agent.exec_pod.enabled: false` 并重启；kubectl exec 兼容入口也应不可用。

## 4. 功能与两入口一致性

在聊天中请求以下诊断，替换所有大写占位符：

| 编号 | 请求 | 预期 |
|---|---|---|
| F1 | 查看 TEAM_A/POD 的 /app 目录，指定 CONTAINER | 发起 exec_pod；审批卡显示真实容器和 argv；批准后返回目录信息 |
| F2 | 执行 `kubectl exec POD -n TEAM_A -c CONTAINER -- ls -l /app` | 同一规则、同一真人批准与结果契约 |
| F3 | 再次执行 F1 或 F2 | 再次询问；无会话免审选项 |
| F4 | 查看容器 id、hostname、磁盘及业务进程 | 支持的规则可执行；ps 只显示 pid/ppid/comm |
| F5 | 多容器 Pod 不提供 container | 提示补充目标，不执行到默认容器 |
| F6 | 目标镜像缺少 readlink、ss 或参数不兼容 | 明确报告程序或能力限制，不安装工具、不回退解释器 |
| F7 | `kubectl version`、`kubectl api-resources` | 有该集群资源资格时正常返回 |
| F8 | `kubectl auth can-i get pods -n TEAM_A` | 查询当前执行身份，不允许 --as 或 --list |
| F9 | `kubectl get pods -n TEAM_A | grep api` | 自动执行；无匹配视为查询无匹配 |
| F10 | `kubectl get pods -n TEAM_A -o json | jq '.items'` | 按既有受限 jq 规则执行 |
| F11 | rollout history/status 查询明确工作负载 | 只读授权通过，不按 rollout 变更处理 |
| F12 | 查询平台目录未支持的 CRD | 明确资源目录不支持，不伪装为用户缺少 namespace 权限 |

## 5. 权限、目标与审批

| 编号 | 场景 | 预期 |
|---|---|---|
| P1 | 只读用户请求 exec | 不发审批，不建立 exec 流 |
| P2 | 普通用户请求 TEAM_B | 拒绝；切换为 kubectl 入口结果相同 |
| P3 | 平台授权充分，Kubernetes 禁止 pods/exec | RBAC_DENIED；明确拒绝时不发审批 |
| P4 | 审批卡待答时撤销用户授权 | 原调用停止；批准不能恢复权限 |
| P5 | 待答时轮换集群凭据 | 旧运行失效；不能使用原批准继续 |
| P6 | 待答时重建同名 Pod | UID 变化，要求重新取得计划和批准 |
| P7 | 待答时重启容器 | 容器身份变化，原计划停止 |
| P8 | 用户拒绝，或关闭/取消会话 | 不执行，不同义重试索取批准 |
| P9 | `--skip-permissions` / require_approval=false | 普通变更原有行为保持；exec 仍逐次真人批准 |
| P10 | 指定其他 kubeconfig/context/server 或 impersonation | 拒绝；批准不能覆盖 |
| P11 | 集群 A/B 同名 Pod 放置不同可识别标记 | 各会话结果符合目标，并核对 API Server 审计访问记录 |

P11 必须核对实际 API 请求归属；仅凭聊天内容不同，不认定端到端隔离验收通过。

## 6. 安全与运行控制

| 编号 | 场景 | 预期 |
|---|---|---|
| S1 | cat、env、ps aux、ps e、sh -c、Python、curl | 不执行；给出不支持或策略拒绝，不进入通用脚本通道 |
| S2 | Secret 挂载目录、/proc、/sys、/dev、凭据路径 | 拒绝 |
| S3 | 普通路径符号链接指向敏感挂载 | 实际路径检查后拒绝，不输出敏感内容 |
| S4 | du 的目录包含敏感子挂载 | 拒绝递归统计 |
| S5 | privileged/hostNetwork/hostPID/hostIPC/hostPath Pod | 本期诊断入口拒绝，保留结构化 API 查询 |
| S6 | 配置 shell_enabled=false | F9/F10 受控组合仍工作；真实 Shell 不执行 |
| S7 | 配置 exec_pod.enabled=false | 两种 exec 入口均不可执行，普通查询不受影响 |
| S8 | 单用户、单会话并发超过上限 | EXECUTION_BUSY，不无限排队 |
| S9 | 大输出、超时、用户取消 | 输出截断或停止流；超时明确远端终止未确认，不自动重启诊断 |
| S10 | 具体 Secret/key 诊断 | 使用原有 get_secret_value 单独确认；ModelOnly、历史/SSE/审计脱敏及 reveal 规则保持 |

## 7. 记录与完成条件

每项记录：版本、集群、用户角色、目标、时间、批准结果、工具结果、API 审计证据和通过/失败。避免保存真实 Secret 明文。

本地验证完成不等于真机验收完成。功能、权限与目标、敏感数据三组用例均通过后，方可将方案 26 的真机验收状态改为完成。

## 8. 已知执行边界

- client-go 使用非交互 SPDY exec；具体集群与代理的升级链路兼容性须通过本次真机验证。
- 业务镜像的程序与参数可能不同，平台不会自动注入工具、下载程序或启动调试容器。
- 路径解析与随后执行不是远端文件系统原子事务；不承诺对恶意容器的完整隔离。
- 断开远端流不保证目标进程已经退出。超时后应由授权人员核对，不自动追加 kill。
- 绑定模式子进程不再继承平台全局云身份环境；集群凭据应自包含，认证插件所需环境须通过受信集群接入配置提供。
