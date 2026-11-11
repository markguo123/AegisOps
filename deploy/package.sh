#!/usr/bin/env bash
# Produce the exact resource bundle consumed by install.sh's remote entry point.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-${ROOT}/dist-install}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
tar -czf "$OUT/aegisops-install.tar.gz" -C "$ROOT" \
  deploy/install.sh deploy/upgrade.sh deploy/install.env.example \
  deploy/base deploy/allinone deploy/separated deploy/helm deploy/rbac-write.example.yaml
(cd "$OUT" && sha256sum aegisops-install.tar.gz > aegisops-install.tar.gz.sha256)
printf 'Installation bundle: %s\n' "$OUT/aegisops-install.tar.gz"
