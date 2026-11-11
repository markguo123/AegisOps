# 部署安装与形态收敛方案

- 版本：v1.2
- 日期：2026-11-11
- 状态：P11.1–4 主要代码已落地，本地验证通过；公开发布/真实集群安装与 P11.5 迁移准出待验。
- 决策：沿用 ADR-051 的进程拓扑；ADR-053 覆盖安装入口、脚本与 Helm 关系、快速体验环境默认值和实施顺序。安装实现已落地；远程地址仍需发布制品后验证，不表示已发布。

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

## 3. 安装入口与配置决策

对外统一称“一键安装快速体验环境”，不称“一键 dev”或开发环境。快速体验是安装默认配置，不是模型或业务功能的裁剪版；脚本内部模式建议 PROFILE=quickstart，生产 PROFILE=production。镜像默认取最新已成功发布版本，具体通道与不可变 digest 由发布流程提供。

交付两种独立安装方式：脚本、Helm。脚本不调用或依赖 Helm，Helm 不依赖项目安装脚本。共享业务内核、镜像版本与配置语义；允许维护各自部署模板，通过一致性检查控制漂移，覆盖旧“单 Chart 是唯一模板源”的要求。

不增加 quickstart.sh、dev.env、values-dev.yaml 或新的配置转换框架。**一键安装快速体验环境 是现有 install.sh 的零参数默认路径**；生产脚本继续编辑现有 install.env；Helm 使用原生 values.yaml。环境和部署形态分开：快速体验环境默认 minimal（内嵌 SPA），production 默认 standard（Web+Backend），两条安装路径均支持两形态。Backend 首版单副本。

### 3.1 README 一键安装快速体验环境

目标命令（尚未实现，发布前不得在 README 宣称可用）：

```bash
curl -fsSL https://raw.githubusercontent.com/markguo123/AegisOps/main/deploy/install.sh | bash
```

发布时确认仓库实际公开分支。远程执行不能依赖本地源码或相邻文件：install.sh 自动下载同版本脚本模板包，验证校验和，复用同一安装流程；本地版本包安装直接使用包内资源。支持管道 stdin/非 TTY，不补问必填项。不要求 Git、Go、Node、Docker 或 Helm；不在用户机器构建推送镜像。

脚本使用内置默认值，零参数不会读取当前目录偶然存在的 install.env，生产必须显式指定 --env-file，防止从 README 执行时误读现场配置。默认部署到当前 kubeconfig context，开始展示目标，不创建 Kubernetes 集群，不修改全局 kubeconfig、节点或防火墙。

| 配置项 | 零参数快速体验安装 默认 | 生产建议 |
| --- | --- | --- |
| 部署形态 | minimal，内嵌 SPA，单副本 | standard；可选 minimal；Backend 单副本 |
| 镜像 | 自动获取最新成功发布的镜像 | 固定版本/digest，前后端版本配对 |
| 数据库 | SQLite，/data/aegisops.db | 外部 MySQL，库/账号预建，DSN 含 parseTime=true |
| 内容存储 | db，随 SQLite；不增加外部依赖 | db/dir/s3 沿用现有能力 |
| 数据卷 | 首装有默认 StorageClass 则自动建 2Gi PVC；无默认类则 emptyDir | 按实际目录用途配置持久卷；dir 必须持久化 |
| 访问 | NodePort，集群分配空闲端口，自动查询并输出 | ClusterIP+已有 Ingress/可信代理 HTTPS；可选 NodePort |
| 管理账号 | admin，随机口令；Secret 复用 | 随机生成或引用现有 Secret；不覆盖已有用户 |
| 加密密钥 | 自动生成并复用 Secret | 生成/引用既有 Secret，提供备份说明 |
| 模型/集群/OIDC | 安装后页面配置 | 页面配置或保留既有设置 |
| 写 RBAC | 默认关闭 | 显式开启；应用运行仍要求审批与真实 RBAC |

安装成功指 API 就绪、页面可访问、账号可登录。模型和集群接入不作为安装前置，不自动保存安装者 kubeconfig、不自动登记宿主集群或创建业务 Grant。应用未配置模型时的既有提示不在本轮扩成模型改造任务。

### 3.2 生产脚本

用户获取版本化安装包，复制 install.env.example 为 install.env，正常修改变量后执行：

```bash
./deploy/install.sh --env-file ./install.env
```

模板显式设置 PROFILE=production，脚本生产路径校验必填现场值；不自动回落快速体验模式。保留 CLI > 环境变量 > 指定 env 文件 > 内置默认的优先级，旧 FORM=allinone/separated 映射 minimal/standard。env 文件白名单解析，不作为 shell source 执行。

安装脚本自身完成预检、临时 YAML 渲染、Secret/PVC、迁移、apply、等待及验收。默认不构建镜像；构建推送放发布流程，离线包可覆盖镜像地址/拉取 Secret。保留 dry-run/diff、失败阶段提示、升级记录、密钥复用和显式轮换。已有 DSN Secret 时不要求重新输入明文。敏感值经受限文件或 Secret 引用提供，不出现在命令示例、日志、diff、进程参数中。

### 3.3 原生 Helm

沿用 deploy/helm/aegisops 的目标设计：一个 Chart 支持 standard/minimal，使用普通 values.yaml 覆盖、values.schema.json/模板校验、NOTES 安装摘要和版本化 Chart 包。用户可直接 helm install/upgrade/uninstall，不需要 install.env 或脚本先初始化。

```bash
helm upgrade --install aegisops ./deploy/helm/aegisops \
  --namespace aegisops --create-namespace -f values.yaml --wait --timeout 5m
```

上述为目标命令。Helm 默认生产语义，推荐外部 MySQL；独立生成或引用管理员/加密 Secret，升级复用，不把明文密钥作为 values 推荐入口。迁移必须先于业务启动，明确首次 Secret/PVC 与迁移 Job 的创建时序；pre-install hook 不能引用尚未创建的普通 Secret/PVC。离线渲染采用已有 Secret 引用，在线自动生成路径另行验收。

## 4. 必须落地的边界

### 4.1 存储与迁移

快速体验自动存储只在首装决定，记录最终 PVC/临时卷选择，重跑沿用，不能因默认 StorageClass 改变而自动切卷。多个默认类无法唯一判定时报告错误；PVC Pending 不静默改临时卷。无默认类采用临时卷时，摘要明确提示：**Pod 重建或升级会丢失数据库、报告及页面配置**；不把临时体验宣传为持久安装。

临时 SQLite 必须只在明确快速体验模式 下允许，生产 SQLite 仍要求 PVC。更新当前 SQLite/PVC 校验，不能把 快速体验例外扩为全局放宽。脚本 SQLite 复用 server 启动时已有的幂等迁移，与业务共享同一 /data，不新增独立迁移 Pod；Helm 使用同 Pod initContainer。不能让独立 Job 迁移另一份 emptyDir。PVC 路径可复用迁移 Job，SQLite/dir 采用 Recreate、先停业务再迁移，避免 RWO 冲突和并发写。

已有临时卷实例的升级会影响数据，必须显式确认；零参数新装的非交互行为不能授权重置旧库。卸载默认保留 PVC 和恢复所需密钥，不默认删 namespace。

### 4.2 NodePort 与登录

增加显式 快速体验 HTTP 兼容开关（拟定 platform.auth.allow_quickstart_http，默认 false），由 快速体验安装自动启用；现有仅 loopback 的开关不冒充节点 IP 兼容。HTTP 下 Cookie 使用普通名称且不设置 Secure，不使用要求 Secure 的 __Host- 前缀；保留 session HttpOnly、SameSite、CSRF、Origin 校验、认证限流和授权。HTTPS 仍使用 Secure Cookie；代理协议仅接受受信代理，不信任任意 X-Forwarded-Proto。

生产默认仍要求 HTTPS 登录，NodePort 本身不触发 Cookie 降级；生产需要 HTTP 时必须显式选择开发兼容并显示其边界。OIDC 正式入口仍使用 HTTPS/public_url/CA 校验。不自动安装证书控制器。

输出实际 NodePort 与节点 IP 候选，不能保证客户端网络路由；不可达时给 port-forward 备用命令。登录验收必须覆盖 HTTP Cookie 回传和 CSRF 请求，不能只判断页面 200。

### 4.3 最新镜像与发布

一键安装自动选择最新可用发布版本，无需用户填镜像或版本。发布流程负责公开可拉取的 amd64/arm64 镜像、版本化安装包、校验和及 Chart；脚本与前后端镜像绑定同一 commit/version。实际仓库地址在发布配置中确定，不宣称当前已有公开镜像。

发布可使用滚动 latest 标签；安装时解析并记录实际 digest，整次安装固定同一版本，避免中途更新错配。无法解析 digest 而用可变标签时必须 Always 并显式触发更新，不使用旧缓存。安装包版本应由同一次发布关联，不能把 stable releases/latest 当成 镜像发布。下载、校验、拉取失败即说明原因，不擅自换成旧版本。

不另造用户可编辑的发布配置文件。README 一行命令上线前，必须用无登录、无源码、无 Helm/构建工具环境验证脚本、资源包和镜像全部公开可用。生产显式固定版本，升级使用版本包，不依赖 Git 拉代码；既有 upgrade.sh 保留 Git 发布兼容路径，新版发布包可直接重跑 install.sh。

## 5. 本地借鉴与合理取舍

| 本地来源 | 实际做法 | 本项目采用 |
| --- | --- | --- |
| kagent helm/kagent/values.yaml 的 database.postgres | dev/eval 自带数据库，生产外部数据库；集中配置镜像/存储/Service | 采用环境分层；已有 SQLite，不引入其 PostgreSQL Pod |
| kagent go/core/cli/internal/cli/agent/install.go | 内置参数/profile，实际调用 helm upgrade --install | 借鉴无额外用户配置的体验；不搬 Helm 封装，遵守独立脚本安装 |
| kagent helm/README.md | 独立 Helm 安装/升级/卸载命令 | Helm 保留标准使用方式 |
| HolmesGPT docs/installation/kubernetes-installation.md、helm/holmes/values.yaml | 用户普通 values.yaml 覆盖、Secret 引用 | 不增加环境专用配置体系 |
| HolmesGPT helm/holmes/templates/validate.yaml、Chart.yaml | 渲染早校验、发布时写入版本 | 两条路径早校验、制品配对发布 |

仅核查本地副本，不代表上游在线最新；未找到能同时原样满足本项目“独立脚本+Helm、零配置 dev、页面后配模型”的现成实现。以上按本项目目标取舍，不称全套照搬。未来复制模板/代码须保留许可并登记 docs/CREDITS.md；不搬其 CRD/Controller/权限或固定开发口令。

## 6. 维护、迁移与验收

两套部署资产共享参数语义对照表：数据库、Secret、卷、探针、入口、RBAC、迁移、镜像、资源限制同步变化。CI 分别跑脚本 dry-run 与 Helm lint/template，对等配置比较关键资源语义，避免漂移；无需再建设通用模板编译器。

同一实例只由脚本或 Helm 之一管理，资源加 ownership 标记，冲突默认拒绝；不自动接管另一方式的资源。切换方式须备份并显式迁移。升级前排空/取消运行、备份数据库/内容与加密密钥；迁移失败阻断启动。保留用户、集群、授权、Skill、会话和报告 ID；不能安全恢复的运行标 interrupted。镜像/模板回滚不回退 schema，非兼容 schema 按备份恢复。

验收范围：零参数管道安装、有/无默认存储类、非 TTY、最新镜像解析、HTTP NodePort 登录、Secret 复用、页面后配、minimal/standard 两形态、独立脚本/独立 Helm 新装重跑升级卸载、离线包、生产外部 MySQL、HTTPS/OIDC/SSE、备份恢复及跨方式冲突保护。安装成功不以已配置模型或集群为条件。

## 7. 实施顺序

按用户最新安排，底座之后先 P11 部署收敛，再 P12 多集群联合闭环；部署不等待 P12，不扩大到自动集群接入。P10.7 现场门禁继续保留，不因顺序调整宣称验收完成。

P11.1 契约 → P11.2 独立 Helm / P11.3 脚本规范与零参数安装 → P11.4 HTTP 兼容、发布与两路联验 → P11.5 最终迁移准出。新范围包含独立模板维护、远程安装和发布链路，旧 4.50d 仅历史基线，实施前重估，不作为当前承诺。
