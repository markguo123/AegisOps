# Agent platform direction

Scope: implement the v1.82 ordered plan; reuse upstream code, compatibility first. User requests continuous implementation followed by concentrated testing, with live-environment acceptance handed to the user to save tokens.

- [x] Inspect local reference architecture and current deployment/runtime
- [x] Decide bounded topology, multi-agent scope, migration and full remaining order
- [x] Update design, ADR, deployment references and phase plan
- [x] Validate links, task counts, estimates and consistency

## Next Step
Complete: report architecture, remaining order, estimates and expected benefits.

## Errors
Initial read requested nonexistent backend-deployment.yaml; actual file is deployment.yaml. Corrected inventory.

## Scope update: 2026-11-10 multi-cluster priority

- [x] Confirm live cluster binding and local reusable source
- [x] Narrow prior scope to core + two-unit deployment; defer experts/runtime split
- [x] Design central A managing B/C and update plan with multi-cluster immediately after core
- [x] Validate recalculated tasks, dependencies and current-vs-target docs

## Current Next Step
User executes T1–T5 in docs/04-项目管理与规范/P10-底座集中测试方案.md; review the returned results before closing P10.7. P12 and later phases remain unimplemented. Concentrated code validation is complete.

## Implementation: 2026-11-11

- [x] P10.1 Pin upstream and record current-code differences and compatibility contract
- [x] P10.2 Add repeatable synthetic-evidence baseline entry point and scripted preflight
- [x] P10.2 Capture real-model baseline and manually review evidence/root causes
- [x] P10.3 Implement context budget and bounded compaction (concentrated Go validation passed)
- [x] P10.4 Implement tool result budget and scoped retrieval (concentrated Go validation passed)
- [x] P10.5 Implement failure classification and feedback (concentrated Go validation passed)
- [ ] P10.7 Same-condition comparison and compatibility gates

Real-model baseline completed from user-supplied model.txt, with findings recorded separately. User permits test resources in test-complex and elects to run live tests personally. No cluster mutation was performed by the agent. Manual-only Pod fixtures and test plan are provided.
