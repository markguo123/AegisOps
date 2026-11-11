# Helm 安装

此 Chart 可直接安装，不依赖项目脚本。默认 standard（Web+Backend），后端单副本；minimal 使用内嵌 SPA。

复制 Chart 的 `values.yaml` 到工作目录，填写已发布 `image.tag`、MySQL `database.existingSecret`（键 `dsn`，包含 `parseTime=true`）和入口。Secret 请从受限文件创建，不将口令写入 values 或 CLI 参数。

```bash
helm upgrade --install aegisops ./deploy/helm/aegisops \
  -n aegisops --create-namespace -f values.yaml --wait --timeout 5m
helm uninstall aegisops -n aegisops
```

生产推荐外部 MySQL、`server.publicURL=https://实际域名` 和已有 HTTPS Ingress/可信代理；不安装外部数据库或证书控制器。NodePort 通过 `service.type`/`service.nodePort` 配置。显式 `server.allowQuickstartHTTP=true` 才允许节点 HTTP 登录；生产保持 false。

SQLite 使用 `database.driver=sqlite` 并保留持久卷。`dir` 内容后端也必须持久化。S3 用 `blobs.existingSecret` 引用包含 `AEGISOPS_PLATFORM_BLOBS_*` 变量的 Secret；模型、集群和 OIDC 部署后到页面配置。

迁移在同一 Pod 的 initContainer 执行，失败不启动业务；Recreate 避免 RWO 多 Pod 抢卷。自动生成的凭据 Secret 与 PVC 卸载保留，升级时复用；生产 GitOps/离线渲染推荐 `credentials.existingSecret`（键 `key`：64位十六进制，`password`：初始管理员口令）。在线升级若丢失凭据会拒绝重新生成；先从备份恢复。

升级前备份数据库、内容数据与加密 Secret。`helm rollback` 不回退数据库 schema，不兼容迁移应按备份恢复。脚本管理的同名实例禁止自动接管，切换安装方式须显式迁移。Chart 包会随版本发布，当前源码不代表已有公开镜像。
