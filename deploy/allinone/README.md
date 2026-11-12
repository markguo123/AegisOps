> 历史平台清单参考：本目录不再作为新平台安装入口，也不进入安装包。应用模板唯一来源是 `deploy/helm/aegisops`；新安装与迁移请读 [部署手册](../../docs/03-运维与部署/01-部署与安装.md)。镜像构建文件仍用于标准 Web 镜像构建。

# allinone/ 形态 A：前后端一体化

后端镜像用 `go:embed` 内嵌 `web/dist`，同一个进程、同一个端口同时提供 SPA 与 `/api/v1`，
因此本目录只有两份文件：`deployment.yaml`（内嵌静态托管保持默认开启）与
`ingress.yaml`（单 path `/` 直接打到 `aegisops-server:8080`）。

与 `../separated/` 互斥：两者的后端 Deployment 同名 `aegisops-server`，只能选一个目录 apply。

apply 顺序：`../base/` 前置与迁移 Job → `deployment.yaml` → `../base/service.yaml` →
`rollout status` → `ingress.yaml`（先替换 host 与 ingressClassName）。
