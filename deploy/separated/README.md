> 历史平台清单参考：本目录不再作为新平台安装入口，也不进入安装包。应用模板唯一来源是 `deploy/helm/aegisops`；新安装与迁移请读 [部署手册](../../docs/03-运维与部署/01-部署与安装.md)。镜像构建文件仍用于标准 Web 镜像构建。

# separated/ 形态 B：前后端分离

前端由独立 Nginx 镜像托管 `web/dist`，后端只对外提供 API，两侧各自发布与回滚。

| 文件 | 作用 |
| --- | --- |
| `deployment.yaml` | 后端 Deployment，与 `../allinone/deployment.yaml` 正文逐字一致，只多一条 `AEGISOPS_PLATFORM_SERVER_STATIC_ENABLED=false` |
| `web-deployment.yaml` / `web-service.yaml` | 前端 Nginx Deployment 与 Service（容器监听 8080，Service 映射 80） |
| `ingress.yaml` | 同域按路径分流：`/api/v1` → server，其余 → web；SSE 关缓冲只加在 API 那半 |
| `web.Dockerfile` / `nginx.conf` | 前端镜像构建输入，与仓库根 `Dockerfile` 互不依赖 |

与 `../allinone/` 互斥（后端 Deployment 同名 `aegisops-server`），`ingress.yaml` 也不要和
`../allinone/ingress.yaml` 同时留着。构建命令与完整顺序见 `../README.md`。
