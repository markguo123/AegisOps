> 历史平台清单参考：本目录不再作为新平台安装入口，也不进入安装包。应用模板唯一来源是 `deploy/helm/aegisops`；新安装与迁移请读 [部署手册](../../docs/03-运维与部署/01-部署与安装.md)。镜像构建文件仍用于标准 Web 镜像构建。

# base/ 两种形态共用的前置清单

命名空间、RBAC、平台 ConfigMap、Secret 模板、迁移 Job、后端 Service、报告 PVC 都放在这里，
形态 A 与形态 B 都只 apply 本目录一次，不需要按形态各存一份。

唯一与形态有关的东西在后端 Deployment 里，因此本目录不含 Deployment：
形态 A 用 `../allinone/deployment.yaml`，形态 B 用 `../separated/deployment.yaml`。

本目录的 `secret.example.yaml` / `pvc.yaml` 分别需要填占位值后才可用，见 `../README.md`。
