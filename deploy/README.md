# 部署入口

完整操作指南：[部署与安装](../docs/03-运维与部署/01-部署与安装.md)。先读场景选择，再按首次安装、升级、回滚和恢复章节操作。

**生产推荐 MySQL + S3 兼容对象存储。** 用户或基础设施团队先准备这两项服务，再将连接和应用访问凭据通过 Secret 提供给 AegisOps。项目只交付和维护应用 Chart，不自研、安装或维护 MySQL/MinIO Chart。

| 路径 | 用途 |
| --- | --- |
| `helm/aegisops/` | 应用模板唯一来源，Backend/Web、配置、RBAC、PVC、迁移任务 |
| `helm/aegisops/values-quickstart.yaml` | SQLite + 本地文件的体验预设，HTTP NodePort |
| `helm/aegisops/values-production.yaml` | 外部 MySQL + S3 的生产预设，需配置实际镜像与服务 |
| `install.sh` | 当前源码本地构建、镜像分发、参数与环境检查、调用应用 Helm，不安装中间件 |
| `upgrade.sh` | 升级便利入口，状态、历史和回滚使用原生 Helm |
| `package.sh` | 生成离线审查/交付用的应用 Chart 与脚本包，不是快速体验入口 |
| `scan/` | 可选独立定时巡检通道，不是平台安装入口 |
| `example/` | 独立配置示意；不能递归 apply 整个 deploy 目录 |

快速体验只需 Kubernetes 集群、已配置的 kubectl 和 Docker/Podman。在 Linux 检出的项目目录执行：

```bash
bash deploy/install.sh
```

不用先安装 Helm、填写 TAG、准备 values 或 chmod。安装器在缺少 Helm 时自动下载并校验固定客户端，使用**当前检出源码**构建镜像；kind/k3d 集群自动载入镜像，普通远程/多节点集群须推送到节点可达仓库（`--IMAGE=<仓库>/aegisops --PUSH=true`）。默认 SQLite + 本地文件、minimal HTTP NodePort；有默认 StorageClass 就持久化，没有则警告并使用临时存储。临时数据会在 Pod 重建时丢失。

脚本不解析 GitHub Release，也不下载其它版本的安装包或镜像。源码内容有未提交改动时需显式 `--allow-dirty`；未指定 TAG 时由脚本按当前提交生成构建标签。

生产安装：先按完整指南创建外部服务 Secret，填写生产 values。生产镜像由同一脚本从当前源码构建并推送到你的仓库（推荐），或由你的 CI 构建后用 `--no-build` 复用：

```bash
bash deploy/install.sh --profile=production \
  --values="$HOME/.aegisops/deploy/production.yaml" \
  --IMAGE="<仓库地址>/aegisops" --TAG="<版本>" --PUSH=true --yes

bash deploy/install.sh --profile=production \
  --values="$HOME/.aegisops/deploy/production.yaml" --no-build --yes
```

预演使用 `--dry-run --render-dir=<受限目录>`。产物可能含生成的 Secret，不能公开、提交 Git 或直接当成另一套安装入口。正式安装仍由 Helm 管理。

Backend 当前为单副本，不代表高可用。快速体验默认本地构建；生产默认复用已有镜像，需要源码构建时显式 `--BUILD=true --PUSH=true`。用户指定的 TAG 原样使用；未指定时快速体验按当前提交生成标签。脚本可用 Bash 调用，无需先 chmod。

没有现成依赖时，参考[MySQL Operator 官方安装文档](https://dev.mysql.com/doc/mysql-operator/en/mysql-operator-installation.html)及所选 S3 服务供应商文档。MinIO 社区仓库已归档，不承诺旧社区 Chart 或镜像的生产维护；当前供应商产品信息见完整指南第 5 节。

附件和文件产物走对象存储；启用 S3 不等于整份会话正文已经外置。`blobs=db` 保留兼容和轻量体验用途，不作为生产默认推荐。

旧脚本实例禁止自动 Helm 接管；已有 `db/dir` 对象数据禁止仅修改 driver 就切到 S3。迁移、备份、恢复与版本兼容要求见完整指南第 8、9 节。
