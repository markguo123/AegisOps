# AegisOps 应用 Helm Chart

这是应用部署的唯一模板来源。MySQL 与 S3 兼容对象存储由用户自行部署和维护，本 Chart 不包含中间件。

- 快速体验：仓库根目录执行 `bash deploy/install.sh`，使用当前源码本地构建、自动准备 Helm 并按集群类型分发镜像；页面配置模型及集群。
- 生产：准备 MySQL、对象桶、受限 Secret 文件，复制 `values-production.yaml` 为私有配置并填写固定镜像版本和入口。
- 手动体验：`helm upgrade --install aegisops ./deploy/helm/aegisops -n aegisops --create-namespace -f deploy/helm/aegisops/values-quickstart.yaml --set image.tag=<构建或选定的镜像版本> --wait --wait-for-jobs --timeout 15m`（需自行构建并分发镜像）。
- 模板预览：`helm template` 与脚本 `--dry-run` 使用同一 Chart，包含的 Secret 必须保护；重复生成随机凭据的预览不能直接用于升级。
- 原生管理：`helm history`、`helm upgrade`、`helm rollback`。应用回滚不恢复数据库，迁移兼容性必须先确认。

MySQL 自动迁移使用独立 Job；initContainer 等待 schema，server 使用 `--skip-migrations`。SQLite 自动模式在 initContainer 初始化。
`migration.mode=external` 关闭自动迁移，由部署流水线执行迁移后应用只检查；`database.migrationSecret` 可以使用独立 DDL 账号。
PVC 与自动生成的凭据 Secret 卸载时保留。备份必须包含数据库、对象存储和 `credentials` 中的加密 key。

完整步骤、配置、原脚本实例迁移、恢复与排障见 [部署与安装手册](../../../docs/03-运维与部署/01-部署与安装.md)。
