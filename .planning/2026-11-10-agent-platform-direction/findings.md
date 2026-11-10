# Findings

User now authorizes moderate service separation and multi-agent support; prior single-process-only decision must be superseded. Existing frontend-separated manifests and static toggle can be reused. No business code changes this turn.

Live evidence: kagent checkout is v0.10.6 e36246357df0b8ad248e421ddfcce534be1d6816. Go remote A2A tools import Google ADK; full file transplant would pull SDK. Reuse isolated logic, not full SDK. Existing API uses in-memory run/broadcast ownership. Splitting requires durable run/approval/events, not merely manifests. Existing deploy duplicates backend allinone/separated and defaults HTTP NodePort despite Secure Cookie constraints.

Identity v2.0 (2026-11-07) supersedes legacy 3-role/RBAC-derivation plan: admin/user, platform-managed credentials, no automatic RoleBinding. Must not revive old P4.6 behavior. Prior plan tasks and summary counts require arithmetic audit. Target topology: standard Web/API/Runtime, minimum integrated mode reuses same services. No independent gateway/approval/audit/LLM service, no CRD controller, no per-agent pod.

Decision: standard Web + API + Runtime (one replica per backend role first), shared MySQL, no new mandatory middleware, integrated minimal mode kept via shared application services. Default Helm chart, two values profiles, thin install/upgrade wrappers, HTTPS same-origin. Multi-agent uses isolated specialists inside Runtime; borrow kagent Go logic for lineage/context isolation without Google ADK dependency. Historical workload sum audit: P0 4 vs 3.5, P3 13.5 vs 11, P4 8 vs 7.5; prior summary undercounted 3.5d. Actual 124 task rows including deferred P8.7 => 123 mainline.

New priority: core first, multi-cluster immediately next; user explicitly narrows experts/runtime split. Live cluster/target.go ExecutionConfig and service/platform_run.go PlatformAccess.Bind already bind decrypted selected config, current access and credential version; scans bind explicitly and frontend Chat/Reports already send clusterId. Old memory claiming unbound target is stale. Audit nested diagnosis scanFactory for config capture. Central direct API topology, B/C need credentials/RBAC but no product agent.

## Multi-cluster scope refresh
Current source confirms ExecutionConfig minifies selected kubeconfig; BoundRun rechecks grants and credential version. Chat diagnosis inherits target config via chatScanOptions; page scans bind target too. Existing revocation/credential-rotation test exists. Cluster handler interface lacks credential replacement. Keep one global scan slot for now; document explicit capacity. Central A -> B/C apiserver, no remote agents. Earlier memory about unbound cluster is stale and not reused as a current defect.
