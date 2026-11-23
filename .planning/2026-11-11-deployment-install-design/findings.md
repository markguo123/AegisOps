# Findings

- 现方案 21 v1.1 与 P11 要求脚本薄封装 Helm，用户明确改为两种独立安装方式。
- dev 一键安装走最新已发布镜像、SQLite、NodePort HTTP 兼容；模型与集群均在页面后配，不纳入安装自动初始化。
- 工作区已有 deploy 脚本/清单和 AGENTS.md 未提交修改，保留。
- kagent install.go 实际执行 Helm；借鉴默认体验，不搬冲突机制。HolmesGPT 用普通 values.yaml、渲染校验与发布版本配对。
- 最终：不新增 quickstart.sh/dev.env/values-dev.yaml；install.sh 零参数 dev，生产显式 --env-file；两安装方式独立。
- dev 首装有默认 SC 用 PVC，无默认 SC 用临时卷并提示 Pod 重建数据丢失，重跑不切卷；生产推荐外部 MySQL/HTTPS。
