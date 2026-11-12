#!/usr/bin/env bash
# Offline review/delivery bundle (scripts + application Chart). Not used by install.sh.
set -Eeuo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-${ROOT}/dist-install}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
STAGE="$(mktemp -d)"
trap 'rm -rf -- "$STAGE"' EXIT
tar -cf - -C "$ROOT" \
  deploy/install.sh deploy/upgrade.sh deploy/lib.sh deploy/install.env.example deploy/README.md \
  deploy/helm/aegisops | tar -xf - -C "$STAGE"
if [[ -n "${RELEASE_VERSION:-}" ]]; then
  [[ "$RELEASE_VERSION" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+[-+A-Za-z0-9.]*$ ]] || { echo 'invalid RELEASE_VERSION' >&2; exit 1; }
  awk -v V="${RELEASE_VERSION#v}" -v A="$RELEASE_VERSION" '
    /^version:/ {print "version: " V; next}
    /^appVersion:/ {print "appVersion: \"" A "\""; next}
    {print}
  ' "$STAGE/deploy/helm/aegisops/Chart.yaml" > "$STAGE/chart-version.yaml"
  mv "$STAGE/chart-version.yaml" "$STAGE/deploy/helm/aegisops/Chart.yaml"
fi
tar -czf "$OUT/aegisops-install.tar.gz" -C "$STAGE" deploy
(cd "$OUT" && sha256sum aegisops-install.tar.gz > aegisops-install.tar.gz.sha256)
printf 'Installation bundle: %s\n' "$OUT/aegisops-install.tar.gz"
