# 部署形态收敛方案（范围收口版）

- 版本：v1.1
- 日期：2026-11-10
- 状态：待实现；本轮仅文档。
- 决策：ADR-051，替代本文件 v1.0 的三服务、跨进程调度与专家首版要求；保留文件路径以兼容已有引用。

## 1. 部署定位

默认 Web + Backend 两个部署单元。Backend 内含 Platform API、现有 Agent Runtime、Tools、Approval、Executor 和应用服务；前端只负责展示与同源转发。沿用现有 Go + Vue 产品，优先统一部署资产，避免重新建设运行平台。

保留既有内嵌 SPA 最小模式，前后端和 Agent 同进程，共用所有业务实现。第一版 Backend 单副本，不承诺 HA；登录/OIDC 事务仍按当前身份方案，重启可重新登录，运行中任务可中断。审批中心阶段补安全审批恢复，不等于任意调查断点续跑。

~~~mermaid
flowchart LR
  U[浏览器] --> W[Web / 同源 HTTPS]
  W --> B[Backend: API + Agent Runtime + Tools + Executor]
  B --> D[(既有业务数据库)]
  B --> O[既有对象存储]
  B --> M[模型服务]
  B --> K[A/B/C 的 Kubernetes API]
~~~

本轮不新增 all/api/runtime 启动角色、数据库任务队列、租约、跨进程 SSE 总线、独立 gateway 或沙箱。现有服务边界保持清楚，未来出现容量或故障隔离需求时再拆 Runtime。

## 2. 多 Agent 与扩展取舍

工具集和 Skill 作为当前扩展方式，不实现 workload/network/storage 专家定义、委派工具或专家时间线。未来 Linux 能力先通过工具、Skill 与明确的执行/授权适配扩展；仅在确有独立上下文、模型或远程执行边界时引入 Agent。

支持未来演进不等于现在预建 Agent 注册中心、A2A、CRD 或每 Agent 一个 Pod。原专家与独立 Runtime 任务在计划中标延后，不作为四期出口。审批采用确定性策略服务，不建设审批子 Agent。

## 3. 部署实现与复用

P11 在多集群闭环之后开展。一套 deploy/helm/aegisops Chart 为目标模板源，standard 默认生成 Web + Backend，minimal 生成既有内嵌 SPA 后端。路径是待实现目标，当前不可当作已有安装命令。

优先复用现有 deploy 的镜像、探针、配置、迁移 Job、数据库和对象存储逻辑；kagent v0.10.6（e36246357df0b8ad248e421ddfcce534be1d6816）的 helm/kagent/templates/controller-deployment.yaml、ui-deployment.yaml、ui-nginx-configmap.yaml 仅作适用模板片段来源，不搬其 CRD/Controller/Google ADK 依赖图。

install/upgrade 脚本转为同 Chart 薄封装，保留 dry-run、diff、升级记录和回滚；旧 FORM=allinone/separated 显式映射 minimal/standard，不复制业务或长期维持第二套应用 YAML 渲染器。

标准模式沿用 MySQL 和既有 db/dir/s3 内容后端；dir 由唯一后端使用并按现有锁/PVC 要求挂载，Web 不访问目录。最小模式支持已有 SQLite/dir，SQLite 数据挂 PVC，不跨 Pod 共享。数据库迁移先于业务启动，失败阻断升级。

## 4. 入口与凭据

默认 ClusterIP，复用已有 Ingress 或可信代理提供同源 HTTPS；NodePort 显式选用并接 HTTPS 浏览器入口。沿用 Secure Cookie、OIDC public_url 和 CA 校验，不自行安装证书控制器或网关。前端与后端镜像版本明确配对；沿用现有离线 kubectl 投递方式。

平台部署 SA 只服务 A 上的安装/运行需要；管理 B/C 使用其各自托管凭据，不因为服务部署于 A 而获得其他集群权限。具体多集群拓扑见 [方案 22](22-方案-多集群集中管理与执行闭环-四期.md)。

## 5. 升级与验收

切换模板前排空或取消运行、备份数据库及内容存储；保留用户、集群、授权、Skill、会话与报告 ID。不能安全恢复的旧运行标 interrupted；非兼容 schema 按备份恢复，不承诺只回滚镜像就可回退数据。

两模式新装、重跑幂等、升级、备份恢复、HTTPS/OIDC、SSE、审批卡、多集群目标与取消通过后，移除重复应用模板；example/kubectl/scan 按用途保留。已有脚本在迁移前仍按当前语义使用。

P11 五项 4.50d 初估，P11.1–4 合计 3.50d 实现部署收敛；P11.5 1.00d 与最终准出一起验收迁移。完整验收见计划 §12。
