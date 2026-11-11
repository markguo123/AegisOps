# Findings

User now authorizes moderate service separation and multi-agent support; prior single-process-only decision must be superseded. Existing frontend-separated manifests and static toggle can be reused. No business code changes this turn.

Live evidence: kagent checkout is v0.10.6 e36246357df0b8ad248e421ddfcce534be1d6816. Go remote A2A tools import Google ADK; full file transplant would pull SDK. Reuse isolated logic, not full SDK. Existing API uses in-memory run/broadcast ownership. Splitting requires durable run/approval/events, not merely manifests. Existing deploy duplicates backend allinone/separated and defaults HTTP NodePort despite Secure Cookie constraints.

Identity v2.0 (2026-11-07) supersedes legacy 3-role/RBAC-derivation plan: admin/user, platform-managed credentials, no automatic RoleBinding. Must not revive old P4.6 behavior. Prior plan tasks and summary counts require arithmetic audit. Target topology: standard Web/API/Runtime, minimum integrated mode reuses same services. No independent gateway/approval/audit/LLM service, no CRD controller, no per-agent pod.

Decision: standard Web + API + Runtime (one replica per backend role first), shared MySQL, no new mandatory middleware, integrated minimal mode kept via shared application services. Default Helm chart, two values profiles, thin install/upgrade wrappers, HTTPS same-origin. Multi-agent uses isolated specialists inside Runtime; borrow kagent Go logic for lineage/context isolation without Google ADK dependency. Historical workload sum audit: P0 4 vs 3.5, P3 13.5 vs 11, P4 8 vs 7.5; prior summary undercounted 3.5d. Actual 124 task rows including deferred P8.7 => 123 mainline.

New priority: core first, multi-cluster immediately next; user explicitly narrows experts/runtime split. Live cluster/target.go ExecutionConfig and service/platform_run.go PlatformAccess.Bind already bind decrypted selected config, current access and credential version; scans bind explicitly and frontend Chat/Reports already send clusterId. Old memory claiming unbound target is stale. Audit nested diagnosis scanFactory for config capture. Central direct API topology, B/C need credentials/RBAC but no product agent.

## Multi-cluster scope refresh
Current source confirms ExecutionConfig minifies selected kubeconfig; BoundRun rechecks grants and credential version. Chat diagnosis inherits target config via chatScanOptions; page scans bind target too. Existing revocation/credential-rotation test exists. Cluster handler interface lacks credential replacement. Keep one global scan slot for now; document explicit capacity. Central A -> B/C apiserver, no remote agents. Earlier memory about unbound cluster is stale and not reused as a current defect.

## 2026-11-11 implementation audit

Clean starting HEAD 67df0ef. Upstream HolmesGPT HEAD matches 27c623ee3ab074734cf0d72964bafd11245b5f80. Current Agent uses turn-based history truncation, byte-limited results and identical-failed-call suppression, with no full request context budget. Upstream result storage suggests cat/pipes without approval; this cannot be copied into AegisOps. Compaction must not receive current-turn approved Secret values. Full mapping is in design document 23. docs/CREDITS.md did not exist; added a truthful research-only entry, no claimed code transplant.

P10.2 baseline exercises the actual Agent.Chat with a real opt-in model and a synthetic read-only tool. Three fixed cases (OOM, image pull, scheduling) carry SHA-256 fixtures and measured requests/tool calls/token usage/elapsed time. Scripted preflight is separate and is not a model-quality score. Real-model setup and manual evidence review remain required before runtime changes.

User supplied local model.txt and kubeconfig (paths/credentials are not copied into product docs). Real baseline completed all three synthetic cases with DeepSeek-V4-Flash: OOM 3 requests/2 tool calls/15504 total tokens/7772ms; image pull 2/1/10196/3594ms; scheduling 3/2/15467/5177ms. Review found invented StatefulSet/container and Deployment repair targets in OOM/scheduling; completion/marker hits are not correctness scores. Added explicit target-owner/container evidence requirement. Logs remain in ignored .aegisops-test/p10. model.txt is locally excluded in .git/info/exclude.

User directs implementing functions continuously then one concentrated validation, avoiding broad intermediate tests; user also directs direct copying wherever compatible. P10 implementation adopts upstream prompt text (with safety adaptations), ports bounded rolling compaction and preview/retrieval to Go, reuses BlobGateway + transactional Acquire/Release to enter existing orphan GC, and preserves current turn references only. No cluster fault injection or mutations performed.
