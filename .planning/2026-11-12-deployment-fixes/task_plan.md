# Deployment fixes
Scope: fix install defaults, PVC class alignment, Helm capacity compatibility, chmod dirty detection and tag semantics; assess deployment maturity. No cluster operations.
## Phases
1. Inspect: complete.
2. Implement and document: complete.
3. Run deployment regression tests and review diff: complete.
4. Assess deployment architecture and bundled dependencies from local reference projects: complete; discussion only.
5. Implement final deployment convergence: complete. Canonical app Helm chart, user-managed middleware only, thin installation/upgrade/render entrypoints, explicit migrations, rewritten user guide; one-command experience retained.
6. Validate full Go tests/vet, render/package/contracts and independent review: complete locally. Live kind/MySQL CI and production acceptance remain explicitly unexecuted here (no local Docker/target supplied).
## Next Step
Deliver code/docs and validation results. Publish matching release artifacts before expecting remote one-command users to receive this implementation.
