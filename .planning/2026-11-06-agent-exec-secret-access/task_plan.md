# Task Plan: Agent Exec and On-Demand Secret Diagnostics

## Goal
Allow approved non-interactive kubectl exec and audited per-key Secret diagnostics without persisting or echoing Secret values, while keeping attach/port-forward blocked.

## Next Step
No implementation work remains; deploy the new backend image when ready so the Secret diagnostic tool is active in the cluster.

## Current Phase
Phase 3

## Phases

### Phase 1: Requirements & Discovery
- [x] Understand user intent
- [x] Identify constraints
- [x] Document in findings.md
- **Status:** complete

### Phase 2: Planning & Structure
- [x] Define approach
- [x] Create project structure
- **Status:** complete

### Phase 3: Implementation
- [x] Non-interactive exec hardening and RBAC
- [x] Secret diagnostic config and Kubernetes read facade
- [x] Structured get_secret_value tool and approval plan
- [x] Separate model-only Secret data from safe UI/session data
- [x] Exact output redaction and streaming suppression
- [x] Prompt, docs, and deployment examples
- **Status:** complete

### Phase 4: Testing & Verification
- [x] Verify requirements met
- [x] Document test results
- **Status:** complete

### Phase 5: Delivery
- [x] Review outputs
- [x] Deliver to user
- **Status:** complete

## Decisions Made
| Decision | Rationale |
|----------|-----------|
| Non-interactive exec only | User approved exec; stdin/tty and blocking modes remain dangerous. |
| Keep attach/port-forward denied | Both are interactive/blocking and cannot be made safe via a simple RBAC grant. |
| Per-key Secret approval | Reads authoritative cluster data without asking users to copy secrets manually. |
| Secret values are model-turn only | Values may reach the current model request but never persistence, audit, SSE, or final answer. |

## Errors Encountered
| Error | Resolution |
|-------|------------|
