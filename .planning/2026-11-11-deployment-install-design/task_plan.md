# 部署安装设计修订

## Objective
按用户最新决定，直接参考本地 kagent/HolmesGPT，完成 dev 一键脚本、生产变量文件脚本、独立 Helm 的设计；不调用 DSH，不改运行实现。

## Phases
- [x] 核对现有方案、开发计划与用户纠正。
- [x] 参考本地项目安装与发布资产。
- [x] 更新方案 21、ADR、开发计划，检查一致性。

## Next Step
主控已接手并实现，完成最终检查与交付；不再调用 DSH。

## Superseded next step
用户已授权落地：派发 workspace-write DSH 实现，注册完成通知；完成后主控独立审查、处理安全敏感代码并验收。

## 2026-11-11 用户修正与实施授权
- 对外口径：一键安装快速体验环境，不称一键 dev。无额外体验配置文件。
- 授权 DSH 实现部署设计，主控不轮询等待，完成通知后再审查。
- 旧调查 agent c3e47414-8dcf-46fd-b127-313ecb0b7b20 是只读权限；本次须新建写权限实现 agent。

## Active DSH implementation
- agent_id: 53727e33-f60c-43da-8eea-215751a1ef0d (workspace-write), accepted.
- Scope: P11 scripts/Helm/release/docs/tests; preserve existing edits; no live deployment/publish/commit.
- Parent follow-through: inspect result and diff, own Go allow_quickstart_http config/browser safety implementation, run checks and accept.
- Callback setup failed twice with socket hang up (native watch then parent helper); receipts: dsh-callback-7vKPfZ and dsh-callback-Fvc0ZR under local bridge callbacks. No successful watcher, no polling/wait retained as explicitly requested. Retrieval needed on next parent activation.

## 主控实现（2026-11-11）
- 用户取消 DSH，主控直接实现 install.sh/生产模板、独立 Chart、发布打包、HTTP Cookie opt-in、测试和文档。
- Go test ./... / go vet ./... 已通过；新增远程管道/损坏包/存储/Chart/登录安全测试通过，最终变更后再验证。
- 不操作真实集群、不发布外部制品、不提交；原工作区变更保留。

## Final local acceptance
- Complete: production env-file script, zero-parameter experience script, independent Helm, opt-in HTTP cookies, release bundle/workflow, tests and docs.
- Final checks: go test ./..., go vet ./..., Helm strict lint/template/package, Bash syntax, remote stdin bootstrap + checksum-corruption tests, git diff --check passed.
- Script SQLite deliberately reuses existing server startup migration; Helm uses same-Pod init migration. Legacy upgrade.sh retained for compatibility; version-bundle upgrades use install.sh.
- No live cluster deployment, GHCR/GitHub publication or commits performed. P11.5 and public/live acceptance remain pending; previous user changes preserved.
