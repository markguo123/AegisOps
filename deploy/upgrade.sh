#!/usr/bin/env bash
# Thin app-release adapter. Helm owns history/rollback; middleware is never touched here.
set -Eeuo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELM_BIN="${HELM_BIN:-helm}"
RELEASE=aegisops; NAMESPACE="${NAMESPACE:-aegisops}"
ROLLBACK=""; STATUS=0; SRC=""; REF=""; NO_FETCH=0
ARGS=()
usage() {
cat <<'EOF'
AegisOps 应用更新（原生 Helm Release）
  bash deploy/upgrade.sh --env-file=install.env --yes   # 常规：只改 install.env 里的 TAG
  bash deploy/upgrade.sh --rollback=<Helm revision> --namespace=aegisops
  bash deploy/upgrade.sh --status --namespace=aegisops
  --release=<name> --namespace=<ns> 默认 aegisops
  --src=<dir> --ref=<branch/tag/commit> --no-fetch  可选取码；不自动拼接镜像 TAG
  --tag=<tag>                 等效 install.sh --TAG；原样使用
  --no-build --no-push --allow-dirty --follow-env-tag（兼容空操作）
  其余参数转交 install.sh；--values 仅用于覆盖 install.env 未暴露的高级字段。
  源码构建显式 --BUILD=true --PUSH=true。
回滚只恢复应用资源，不能回退数据库；中间件须单独管理。升级前备份并确认 schema 兼容。
EOF
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0;;
    --rollback=*) ROLLBACK="${1#*=}";; --rollback) ROLLBACK=previous;;
    --status) STATUS=1;;
    --release=*) RELEASE="${1#*=}"; ARGS+=("$1");;
    --namespace=*) NAMESPACE="${1#*=}"; ARGS+=("$1");;
    --src=*) SRC="${1#*=}";; --ref=*) REF="${1#*=}";; --no-fetch) NO_FETCH=1;;
    --tag=*) ARGS+=("--TAG=${1#*=}");;
    --follow-env-tag) :;;
    --with-mysql|--with-minio|--mysql-values=*|--minio-values=*) echo '中间件由部署方提供；本项目只管理应用 Release' >&2; exit 1;;
    --) shift; ARGS+=("$@"); break;;
    *) ARGS+=("$1");;
  esac
  shift
done
if [[ -n "$ROLLBACK" || $STATUS -eq 1 ]]; then
  die() { printf '%s\n' "$*" >&2; exit 1; }
  . "$SCRIPT_DIR/lib.sh"
  aegisops_helm_client cached
  if [[ $STATUS -eq 1 ]]; then "$HELM_BIN" status "$RELEASE" -n "$NAMESPACE"; "$HELM_BIN" history "$RELEASE" -n "$NAMESPACE"; exit 0; fi
  if [[ "$ROLLBACK" == previous ]]; then ROLLBACK=0; fi
  [[ "$ROLLBACK" =~ ^[0-9]+$ ]] || { echo 'rollback 必须是 Helm revision 数字' >&2; exit 1; }
  echo '仅回滚应用 Release；数据库和对象内容保持现状。'
  "$HELM_BIN" rollback "$RELEASE" "$ROLLBACK" -n "$NAMESPACE" --wait --wait-for-jobs --timeout 15m
  exit $?
fi
if [[ -n "$SRC" ]]; then SCRIPT_DIR="$(cd "$SRC/deploy" && pwd)"; fi
if [[ -n "$REF" ]]; then
  repo="$(cd "$SCRIPT_DIR/.." && pwd)"
  [[ -z "$(git -c core.filemode=false -C "$repo" status --porcelain --untracked-files=no)" ]] || { echo '切换 ref 前请提交/还原内容改动（chmod 不影响）；--allow-dirty 仅允许当前工作树构建' >&2; exit 1; }
  if [[ $NO_FETCH -eq 0 ]]; then git -C "$repo" fetch origin --tags; fi
  resolved="$REF"
  git -C "$repo" rev-parse --verify --quiet "$REF^{commit}" >/dev/null || resolved="origin/$REF"
  git -c core.filemode=false -C "$repo" checkout --detach "$resolved"
fi
exec bash "$SCRIPT_DIR/install.sh" "${ARGS[@]}"
