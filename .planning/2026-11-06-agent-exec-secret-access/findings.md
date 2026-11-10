# Findings & Decisions

## Requirements
- Non-interactive `kubectl exec` should pass approval and execute when the write RBAC profile grants `pods/exec create`.
- `attach` and `port-forward` remain hard-rejected.
- Agent can request specific Secret keys after evidence shows a Secret mismatch.
- Each Secret read requires a visible approval card and a reason.
- Secret plaintext is available only to the current model turn.
- SDK/UI/SSE, session files, database persistence, audit, logs, and final answer never contain plaintext.

## Research Findings
- Current SA can get Secrets but cannot create `pods/exec`.
- Parser already allows non-interactive exec and rejects `-i/-t`.
- Preflight incorrectly maps exec to the generic `exec` verb instead of `pods/exec create`.
- Current Secret mode only allows `off` and `create_only`; kubectl Secret access is hard-rejected.
- Existing persistence redaction is heuristic and cannot guarantee arbitrary Secret values are removed.
- Model streaming deltas currently bypass exact-value redaction.

## Technical Decisions
| Decision | Rationale |
|----------|-----------|
| Add `read_on_demand` Secret mode | Distinguishes create-only from per-key diagnostic reads. |
| Use `get_secret_value` instead of kubectl Secret access | Enforces key scope and prevents bulk dumps. |
| Add model-only fields to ToolResult | Model can diagnose while UI/session use safe fields. |
| Exact-value redaction after any Secret read | Heuristic regexes alone do not cover arbitrary passwords. |
| Suppress model streaming while sensitive values are active | Prevents partial plaintext from reaching the UI before final redaction. |

## Issues Encountered
| Issue | Resolution |
|-------|------------|

## Resources
-
