# 统一 Kubernetes Gateway 本地验证与真机验收

日期：2026-11-17

关联设计：方案 26 v2.1。当前为进程内薄网关，不涉及部署拓扑变化；本地验证不代表真实集群验收完成。

## 1. 验收环境

准备 A、B 两个测试集群及 team、other 两个 namespace。普通用户仅有 A/team 的 read、logs、operate，另准备只读用户与管理员。实际执行 kubeconfig 的 Kubernetes RBAC 应允许足够范围，以便独立验证平台拒绝；随后再使用受限 ServiceAccount 验证 RBAC 拒绝。所有写入使用专用测试资源。

## 2. 普通通道与能力覆盖

| 场景 | 预期 |
|---|---|
| 结构化 get_pods 与 kubectl get pods -n team | 均成功，同一资源授权结果 |
| get pods -n team 的只读管道 | 自动执行，无 bash 依赖 |
| set image deployment/demo demo=指定测试镜像 -n team | 直接显示完整命令并请求确认；批准后通过实际 GET/PATCH，不需改写 patch 才能通过平台授权 |
| label / annotate / scale 同一测试工作负载 | 无需新增专用工具或权限解析分支，仍需确认并受实际请求授权约束 |
| 拒绝或取消批准 | 不向集群发送变更请求 |
| 创建清单、文件参数、URL 参数来源 | 使用 create_resource 校验与 proposal；普通代理不旁路创建流程 |
| 任意 shell、解释器、插件、本地文件、目标覆盖参数 | 明确拒绝，不回退直连，不误报 namespace 权限 |

## 3. 统一授权与多集群隔离

1. 普通用户读取 other、跨 namespace 列表或 nodes：结构化工具和 kubectl 均拒绝，API Server 侧无对应越界资源请求。
2. 在 A 会话要求操作 B，或提供 --context、--server、--kubeconfig、--token、--as：拒绝覆盖。A、B 会话分别核对资源唯一标识与 API Server 审计。
3. 只读用户尝试 set image、patch、delete：不产生实际变更。不能通过修改命令描述为只读而获得写权限。
4. 平台允许而实际 ServiceAccount 无权：API Server 拒绝，不能因为人工批准绕过 RBAC。
5. 批准等待期间撤权、停用账号或轮换凭据：旧执行拒绝；已有不可撤销的远端写入不承诺回滚。
6. 授权设置短期到期，保持会话不退出：到期后新请求拒绝。

## 4. 专用安全通道

- exec_pod 每次真人确认，目标 Pod/容器变化不复用旧批准；kubectl exec 兼容入口进入相同能力。
- exec -it、attach、port-forward、proxy、直接 Secret 和 --raw 继续禁止。
- Agent 具体 Secret/key 每次明确询问；越权不发起审批，拒绝读取。
- 显示明文接口在授权 namespace 内可用，失焦/超时清除；聊天、导出、审计无明文。
- 特权、安全上下文、宿主机网络、卷、ServiceAccount 等通用变更体被保守策略拒绝。

## 5. 生命周期与审计

- 子进程临时 kubeconfig 仅含本地网关地址与通道 token，不含真实上游地址/凭据；运行结束后文件删除、监听关闭。
- 同一租约可承载 kubectl 的 discovery 与多次 API 请求；跨租约 token 不通用，缺少许可、修改命令和重复消费许可均拒绝。
- 请求审计 k8s.access 可见用户、目标集群、方法、路径和允许/拒绝；不含 token、请求体、Secret 或查询参数。它是访问决策，实际执行成功与否以工具结果审计为准。
- 审计不可用时访问失败关闭。上游异常、重定向或代理故障不触发直连回退。
- 导出保留脱敏命令、摘要、错误、输出和时间，不再只出现 untrusted_k8s_data 标签；不序列化任意参数或 ModelOnly。

## 6. 本地检查

Go 全量测试、go vet、前端 typecheck、测试及生产构建；专项包 internal/k8sgateway、internal/executor、internal/tools 覆盖租约、实际请求授权和声明式工具接入。

服务拆分尚未实施。多实例失效广播、服务间身份、远端批准记录和租约管理是拆分前置要求，不属于本轮真机通过后即可省略的事项。

本轮本地结果：go test ./...、go vet ./...、前端 typecheck、原全量 241 项测试及生产构建通过；新增导出回归后该专项 4 项通过。未部署、未执行真实集群写入；上述真机项目仍待验收。
