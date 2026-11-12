#!/usr/bin/env bash
# Canonical templates: deploy/helm only. This adapter never edits YAML or owns release history.
set -Eeuo pipefail
umask 077
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HELM_BIN="${HELM_BIN:-helm}"
KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
PARAM_KEYS="PROFILE ALLOW_QUICKSTART_HTTP IMAGE TAG IMAGE_WEB BUILD PUSH NAMESPACE FORM STORE_DRIVER MYSQL_DSN SQLITE_PATH STORAGE_CLASS REPORT_STORAGE_SIZE DATA_STORAGE_SIZE PERSIST_REPORTS EXPOSE NODE_PORT INGRESS_HOST INGRESS_CLASS PUBLIC_URL ENABLE_WRITE ADMIN_USERNAME ADMIN_PASSWORD BLOBS_BACKEND BLOBS_ENDPOINT BLOBS_REGION BLOBS_USE_TLS BLOBS_PATH_STYLE BLOBS_BUCKET BLOBS_ACCESS_KEY BLOBS_SECRET_KEY BLOBS_CREATE_BUCKET BLOBS_PREFIX BLOBS_MAX_OBJECT_SIZE IMAGE_PULL_SECRET IMAGE_PULL_POLICY OIDC_CA_FILE OIDC_DISABLE_PKCE OIDC_ALLOW_MISSING_NONCE GOPROXY KUBECTL_VERSION KUBECTL_DOWNLOAD_BASE"
declare -A cfg=() cli=() original_env=()
for key in $PARAM_KEYS; do if [[ -v "$key" ]]; then original_env[$key]="${!key}"; fi; done
ENV_FILE=""; VALUES=(); PROFILE=""; RELEASE=aegisops; NAMESPACE=""
DRY_RUN=0; DIFF=0; YES=0; ALLOW_DIRTY=0
RENDER_DIR=""; WORK_DIR=""
die() { printf '安装失败：%s\n' "$*" >&2; exit 1; }
info() { printf '[安装] %s\n' "$*"; }
quickstart_summary() {
  [[ "$PROFILE" == quickstart ]] || return 0
  "$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get services -o wide || true
  local port node encoded password
  port="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get service "$RELEASE-server" --ignore-not-found -o 'jsonpath={.spec.ports[0].nodePort}' 2>/dev/null || true)"
  node="$("$KUBECTL_BIN" --request-timeout=15s get nodes -o 'jsonpath={.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)"
  if [[ -n "$port" && -n "$node" ]]; then info "访问：http://$node:$port （节点 IP 必须可从浏览器访问）"; fi
  info "初始用户：admin；模型与集群在页面配置。"
  if [[ -t 1 && "${AEGISOPS_SHOW_INITIAL_PASSWORD:-true}" == true ]]; then
    encoded="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get secret "$RELEASE-credentials" -o 'jsonpath={.data.password}' 2>/dev/null || true)"
    password="$(printf '%s' "$encoded" | base64 --decode 2>/dev/null || true)"
    [[ -z "$password" ]] || printf '首次初始化口令（改密后不再有效）：%s\n' "$password"
  else
    info "初始口令：kubectl -n $NAMESPACE get secret $RELEASE-credentials -o jsonpath='{.data.password}' | base64 --decode"
  fi
}
# 未指定 TAG 时按当前源码生成构建标签；纯 chmod 不改变内容，因而不影响标签。
# 有未提交内容时附带内容摘要，避免同标签掩盖代码变化。
default_build_tag() {
  local sha="" dirty="" digest=""
  if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    sha="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || true)"
    dirty="$(git -c core.filemode=false -C "$REPO_ROOT" status --porcelain --untracked-files=no 2>/dev/null || true)"
  fi
  [[ -n "$sha" ]] || sha="$(date +%Y%m%d%H%M%S)"
  if [[ -n "$dirty" ]]; then
    if command -v sha256sum >/dev/null 2>&1; then
      digest="$(git -C "$REPO_ROOT" diff --binary HEAD 2>/dev/null | sha256sum | cut -c1-8)" || digest=""
    fi
    printf 'qs-%s-%s-dirty' "$sha" "${digest:-content}"
  else
    printf 'qs-%s' "$sha"
  fi
}
# 构建前先确认镜像能被集群拿到，避免构建完成后才发现分发条件缺失。
ensure_distribution_ready() {
  local context
  context="$("$KUBECTL_BIN" config current-context 2>/dev/null || true)"
  case "$context" in
    kind-*|k3d-*) return 0;;
  esac
  if [[ "${cfg[PUSH]:-false}" != true ]]; then
    die "当前集群 ${context:-未知} 不是 kind/k3d，且未开启 --PUSH。远程/多节点集群必须把镜像推送到节点可达仓库：设置 IMAGE=<仓库地址>/aegisops 并加 --PUSH=true。部署端 Docker 缓存默认对集群不可见。"
  fi
  [[ "${cfg[IMAGE]}" == */* ]] || die "远程集群推送需要显式仓库路径：IMAGE=<仓库地址>/aegisops（当前 IMAGE=${cfg[IMAGE]}）"
}
# 镜像分发闭环：kind/k3d 载入，其它集群必须推进节点可达的仓库，不依赖部署端缓存。
distribute_image() {
  local ref="$1" context cluster archive
  command -v "$KUBECTL_BIN" >/dev/null || die "分发镜像需要已连接目标集群的 kubectl"
  context="$("$KUBECTL_BIN" config current-context 2>/dev/null || true)"
  case "$context" in
    kind-*)
      cluster="${context#kind-}"
      command -v kind >/dev/null || die "检测到 kind 集群 $context，但缺少 kind 命令；无法把本地镜像载入集群"
      if [[ "$BUILD_TOOL" == docker ]]; then
        kind load docker-image "$ref" --name "$cluster" || die "kind 载入镜像失败：$ref"
      else
        archive="$(mktemp "${TMPDIR:-/tmp}/aegisops-image.XXXXXX.tar")"
        "$BUILD_TOOL" save "$ref" -o "$archive" || die "导出镜像失败：$ref"
        kind load image-archive "$archive" --name "$cluster" || die "kind 载入镜像失败：$ref"
        rm -f -- "$archive"
      fi
      info "镜像已载入 kind 集群 $cluster：$ref";;
    k3d-*)
      command -v k3d >/dev/null || die "检测到 k3d 集群 $context，但缺少 k3d 命令；无法导入本地镜像"
      k3d image import "$ref" || die "k3d 导入镜像失败：$ref"
      info "镜像已导入 k3d 集群：$ref";;
    *)
      [[ "${cfg[PUSH]:-false}" == true ]] || die "未检测到 kind/k3d 本地集群，且未开启 --PUSH。远程/多节点集群必须把镜像推送到节点可达的仓库：设置 IMAGE=<仓库地址>/aegisops 并加 --PUSH=true，或确认镜像已由其他方式分发。部署端 Docker 缓存默认对集群不可见。"
      "$BUILD_TOOL" push "$ref" || die "镜像推送失败：$ref（检查仓库地址、登录状态与网络）"
      info "镜像已推送：$ref";;
  esac
}
diagnose_failure() {
  printf '\n[诊断] 安装/更新失败，收集现场：\n' >&2
  "$KUBECTL_BIN" -n "$NAMESPACE" get pods,svc,pvc,jobs -o wide 2>/dev/null || true
  "$KUBECTL_BIN" -n "$NAMESPACE" get events --sort-by=.lastTimestamp 2>/dev/null | tail -n 15 || true
  "$KUBECTL_BIN" -n "$NAMESPACE" logs "deployment/$RELEASE-server" --all-containers --tail=50 2>/dev/null || true
  printf '[诊断] Pod 处于 ImagePullBackOff/ErrImagePull 时：确认镜像已载入集群或已推送到节点可达仓库。\n' >&2
}
usage() {
cat <<'EOF'
AegisOps 安装 / 更新（唯一模板源 deploy/helm）
  默认：当前源码本地构建，快速体验（minimal + SQLite + 本地文件 + NodePort HTTP）
  --profile=quickstart|production   无配置文件默认 quickstart，否则 production
  --values=<file>                  应用 Helm values，可重复；推荐生产入口
  --env-file=<file>                兼容 install.env；不执行文件中的命令
  --release=<name> --namespace=<ns> 默认 aegisops
  --dry-run --render-dir=<dir>     只渲染到受限目录，不访问集群/不构建；产物可能含 Secret
  --diff                          Helm 服务端预演（不写集群；输出可能含 Secret）
  --yes                           确认安装/更新；生产升级前自行完成备份
  --allow-dirty                   允许源码内容改动构建；不改写 TAG，chmod 不算内容改动
  --no-build                      不构建镜像：复用它处（CI）已构建并推送到 values 的镜像
  --PUSH=true                     远程/多节点集群：构建后推送镜像到 IMAGE 指定仓库（生产构建必填）
  --KEY=VALUE                     兼容非敏感 install.env 参数；敏感值仅从文件/环境读取
  -h, --help
生产镜像来源：同一脚本加 --IMAGE=<仓库>/aegisops --PUSH=true 现场构建并推送；已有 CI 镜像时用 --no-build。
镜像分发：kind-* 自动 kind load，k3d-* 自动 k3d image import；其它集群必须推送到节点可达仓库。
纯 Helm、升级、原生回滚、迁移、备份与旧部署迁移见 docs/03-运维与部署/01-部署与安装.md。
EOF
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help) usage; exit 0;;
    --values=*) VALUES+=("${1#*=}");; --values|-f) VALUES+=("${2:?missing values file}"); shift;;
    --env-file=*) ENV_FILE="${1#*=}";; --env-file) ENV_FILE="${2:?missing env file}"; shift;;
    --profile=*) PROFILE="${1#*=}";; --release=*) RELEASE="${1#*=}";; --namespace=*) NAMESPACE="${1#*=}";;
    --dry-run) DRY_RUN=1;; --diff) DIFF=1;; --render-dir=*) RENDER_DIR="${1#*=}";;
    -y|--yes) YES=1;; --allow-dirty) ALLOW_DIRTY=1;;
    --no-build) cli[BUILD]=false;; --no-push) cli[PUSH]=false;;
    --*=*) key="${1%%=*}"; key="${key#--}"; val="${1#*=}"
      [[ " $PARAM_KEYS " == *" $key "* ]] || die "不支持参数 $key；请使用 values 文件"
      case "$key" in MYSQL_DSN|ADMIN_PASSWORD|BLOBS_ACCESS_KEY|BLOBS_SECRET_KEY) die "$key 不允许通过命令行传入；请用受限文件或环境变量";; esac
      cli[$key]="$val";;
    *) die "无法识别参数 $1；查看 --help";;
  esac
  shift
done
if [[ -n "$ENV_FILE" ]]; then
  [[ -f "$ENV_FILE" ]] || die "参数文件不存在：$ENV_FILE"
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || die "install.env 只允许 KEY=VALUE"
    key="${line%%=*}"; key="${key%"${key##*[![:space:]]}"}"
    [[ " $PARAM_KEYS " == *" $key "* ]] || die "旧参数 $key 已退役；请按部署文档迁移，不会静默忽略"
    val="${line#*=}"; val="${val#"${val%%[![:space:]]*}"}"; val="${val%"${val##*[![:space:]]}"}"
    if [[ ${#val} -ge 2 && ( ( "${val:0:1}" == '"' && "${val: -1}" == '"' ) || ( "${val:0:1}" == "'" && "${val: -1}" == "'" ) ) ]]; then val="${val:1:${#val}-2}"; fi
    cfg[$key]="$val"
  done < "$ENV_FILE"
fi
for key in $PARAM_KEYS; do
  if [[ -v "original_env[$key]" ]]; then cfg[$key]="${original_env[$key]}"; fi
  if [[ -v "cli[$key]" ]]; then cfg[$key]="${cli[$key]}"; fi
done
# Explicit lower-case flags outrank legacy environment/file equivalents.
PROFILE="${PROFILE:-${cfg[PROFILE]:-}}"
[[ -n "$PROFILE" ]] || { if [[ -n "$ENV_FILE" || ${#VALUES[@]} -gt 0 ]]; then PROFILE=production; else PROFILE=quickstart; fi; }
[[ "$PROFILE" == quickstart || "$PROFILE" == production ]] || die "profile 必须为 quickstart 或 production"
NAMESPACE="${NAMESPACE:-${cfg[NAMESPACE]:-aegisops}}"
[[ "$RELEASE" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#RELEASE} -le 40 ]] || die "release 必须是最长40字符的小写 DNS 名"
[[ "$NAMESPACE" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ && ${#NAMESPACE} -le 63 ]] || die "namespace 不合法"
for key in BUILD PUSH PERSIST_REPORTS ENABLE_WRITE ALLOW_QUICKSTART_HTTP OIDC_DISABLE_PKCE OIDC_ALLOW_MISSING_NONCE BLOBS_USE_TLS BLOBS_PATH_STYLE BLOBS_CREATE_BUCKET; do
  [[ ! -v "cfg[$key]" || "${cfg[$key]}" == true || "${cfg[$key]}" == false ]] || die "$key 必须为 true 或 false"
done
# 唯一模板源必须在当前检出中；不再下载任何发布安装包。
[[ -d "$SCRIPT_DIR/helm/aegisops" ]] || die "未找到 deploy/helm/aegisops；请在完整源码检出的项目根目录执行"
# 快速体验：默认从当前源码构建并分发，不依赖 GitHub Release/公开镜像。
if [[ "$PROFILE" == quickstart ]]; then
  [[ -n "${cfg[IMAGE]:-}" ]] || cfg[IMAGE]=aegisops
  [[ -n "${cfg[FORM]:-}" ]] || cfg[FORM]=minimal
  if [[ -z "${cfg[TAG]:-}" ]]; then
    if [[ $DRY_RUN -eq 1 ]]; then cfg[TAG]=preview; else cfg[TAG]="$(default_build_tag)"; fi
  fi
  if [[ -z "${cfg[BUILD]:-}" ]]; then cfg[BUILD]=true; fi
fi
# 生产：显式给出镜像仓库即默认现场构建；已有 CI 镜像时用 --no-build 复用。
if [[ "$PROFILE" == production && ! -v "cfg[BUILD]" && -n "${cfg[IMAGE]:-}" ]]; then cfg[BUILD]=true; fi
if [[ "${cfg[BUILD]:-false}" == true && -z "${cfg[TAG]:-}" && $DRY_RUN -eq 0 ]]; then cfg[TAG]="$(default_build_tag)"; fi
. "$SCRIPT_DIR/lib.sh"
if [[ "$PROFILE" == quickstart ]]; then aegisops_helm_client download; else aegisops_helm_client cached; fi
for file in "${VALUES[@]}"; do [[ -f "$file" ]] || die "values 文件不存在：$file"; done
WORK_DIR="$(mktemp -d)"; trap 'rm -rf -- "$WORK_DIR"' EXIT
ARGS=(-f "$SCRIPT_DIR/helm/aegisops/values-$PROFILE.yaml")
for file in "${VALUES[@]}"; do ARGS+=(-f "$file"); done
set_value() { local v="${2//,/\\,}"; ARGS+=(--set-string "$1=$v"); }
set_bool() { ARGS+=(--set "$1=$2"); }
if [[ "$PROFILE" == quickstart ]]; then
  if [[ $DRY_RUN -eq 1 ]]; then
    # Offline preview cannot infer storage and therefore represents ephemeral mode.
    set_bool persistence.enabled false
  else
    command -v "$KUBECTL_BIN" >/dev/null || die "需要已连接目标集群的 kubectl"
    state="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get configmap "$RELEASE-installation" --ignore-not-found -o 'jsonpath={.data.persistence}{"|"}{.data.driver}{"|"}{.data.blobs}')" || die "无法读取安装记录"
    case "$state" in
      'true|sqlite|dir') set_bool persistence.enabled true;;
      'false|sqlite|dir')
        # 固定为临时存储，重跑不自动切卷；重建 Pod 会丢数据，必须明确告知。
        set_bool persistence.enabled false
        actual="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get deployment "$RELEASE-server" --ignore-not-found -o 'jsonpath={.status.readyReplicas}{"|"}{.spec.template.spec.containers[0].image}')" || die "无法检查体验实例"
        wanted="${cfg[IMAGE]}:${cfg[TAG]}"
        if [[ "$actual" == "1|$wanted" ]]; then
          info "临时存储体验实例已在运行；镜像未变化时本次 Helm 更新为幂等操作。"
        else
          info "警告：镜像或配置变化会重建 Pod，临时存储中的体验数据将丢失。"
        fi
        info "提示：临时数据在任何 Pod 重建时都会丢失；需要保留请备份并迁移到持久卷。";;
      '||'|'')
        existing="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get deployment "$RELEASE-server" --ignore-not-found -o name)" || die "无法检查已有实例"
        [[ -z "$existing" ]] || die "已有无安装记录的实例，快速体验拒绝覆盖"
        classes="$("$KUBECTL_BIN" --request-timeout=15s get storageclass -o 'jsonpath={range .items[*]}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{" "}{.metadata.annotations.storageclass\.beta\.kubernetes\.io/is-default-class}{"\n"}{end}')" || die "无法检查存储类"
        if [[ "$classes" == *true* ]]; then set_bool persistence.enabled true; else set_bool persistence.enabled false; info "没有默认存储类：使用临时存储，Pod 重建或升级会丢失体验数据"; fi;;
      *) die "已有实例不是本模式的快速体验，拒绝覆盖数据库或对象后端";;
    esac
    YES=1
  fi
fi
# Public compatibility keys only; all rendering belongs to Helm.
for pair in IMAGE:image.repository TAG:image.tag IMAGE_WEB:web.repository STORAGE_CLASS:persistence.storageClass REPORT_STORAGE_SIZE:persistence.reportsSize DATA_STORAGE_SIZE:persistence.dataSize INGRESS_HOST:ingress.host INGRESS_CLASS:ingress.className PUBLIC_URL:server.publicURL IMAGE_PULL_POLICY:image.pullPolicy BLOBS_BACKEND:blobs.driver STORE_DRIVER:database.driver SQLITE_PATH:database.sqlitePath ADMIN_USERNAME:auth.adminUsername; do
  key="${pair%%:*}"; path="${pair#*:}"
  if [[ -v "cfg[$key]" && -n "${cfg[$key]}" ]]; then
    [[ "$path" != unused ]] || die "$key 不再支持自定义路径/初始用户名；请使用标准路径与页面账号管理"
    set_value "$path" "${cfg[$key]}"
  fi
done
if [[ -n "${cfg[NODE_PORT]:-}" ]]; then
  [[ "${cfg[NODE_PORT]}" =~ ^[0-9]+$ ]] || die "NODE_PORT 必须为端口数字"
  set_bool service.nodePort "${cfg[NODE_PORT]}"
fi
for pair in PERSIST_REPORTS:persistence.enabled ENABLE_WRITE:writeAccess ALLOW_QUICKSTART_HTTP:server.allowQuickstartHTTP OIDC_DISABLE_PKCE:auth.oidcDisablePKCE OIDC_ALLOW_MISSING_NONCE:auth.oidcAllowMissingNonce; do
  key="${pair%%:*}"; if [[ -v "cfg[$key]" ]]; then set_bool "${pair#*:}" "${cfg[$key]}"; fi
done
case "${cfg[FORM]:-}" in allinone|minimal) set_value mode minimal;; separated|standard) set_value mode standard;; '') :;; *) die "FORM 不合法";; esac
case "${cfg[EXPOSE]:-}" in nodeport) set_value service.type NodePort;; ingress|none) set_value service.type ClusterIP;; '') :;; *) die "EXPOSE 不合法";; esac
if [[ -n "${cfg[INGRESS_HOST]:-}" ]]; then set_bool ingress.enabled true; fi
if [[ -n "${cfg[IMAGE_PULL_SECRET]:-}" ]]; then set_value 'imagePullSecrets[0].name' "${cfg[IMAGE_PULL_SECRET]}"; fi
# External credentials are created from protected files only, never passed as argv values.
secrets=()
if [[ -n "${cfg[MYSQL_DSN]:-}" ]]; then
  printf '%s' "${cfg[MYSQL_DSN]}" > "$WORK_DIR/dsn"
  set_value database.existingSecret "$RELEASE-mysql"
  secrets+=(mysql)
fi
blob_file="$WORK_DIR/s3.env"; : > "$blob_file"
for pair in BLOBS_BACKEND:DRIVER BLOBS_ENDPOINT:ENDPOINT BLOBS_REGION:REGION BLOBS_USE_TLS:USE_TLS BLOBS_PATH_STYLE:PATH_STYLE BLOBS_BUCKET:BUCKET BLOBS_ACCESS_KEY:ACCESS_KEY BLOBS_SECRET_KEY:SECRET_KEY BLOBS_CREATE_BUCKET:CREATE_BUCKET BLOBS_PREFIX:PREFIX BLOBS_MAX_OBJECT_SIZE:MAX_OBJECT_SIZE; do
  key="${pair%%:*}"
  if [[ -v "cfg[$key]" ]]; then
    [[ "${cfg[$key]}" != *$'\n'* ]] || die "$key 不允许换行"
    printf 'AEGISOPS_PLATFORM_BLOBS_%s=%s\n' "${pair#*:}" "${cfg[$key]}" >> "$blob_file"
  fi
done
if [[ -n "${cfg[BLOBS_ENDPOINT]:-}" ]]; then set_value blobs.existingSecret "$RELEASE-s3"; secrets+=(s3); fi
if [[ -n "${cfg[OIDC_CA_FILE]:-}" ]]; then
  [[ -f "${cfg[OIDC_CA_FILE]}" ]] || die "OIDC CA 文件不存在"
  set_value auth.oidcCASecret "$RELEASE-oidc-ca"; secrets+=(oidc)
fi
[[ -z "${cfg[ADMIN_PASSWORD]:-}" ]] || die "初始口令请通过 credentials.existingSecret 管理；不会替换或重建加密密钥"
if [[ $DRY_RUN -eq 1 ]]; then
  [[ $DIFF -eq 0 ]] || die "dry-run 与 diff 不能同时使用"
  if [[ -z "$RENDER_DIR" ]]; then RENDER_DIR="$(mktemp -d "${TMPDIR:-/tmp}/aegisops-render.XXXXXX")"; else
    [[ ! -e "$RENDER_DIR" ]] || die "render-dir 必须是新目录，防止覆盖已有文件"
    mkdir -p "$RENDER_DIR"
  fi
  chmod 700 "$RENDER_DIR"
  "$HELM_BIN" template "$RELEASE" "$SCRIPT_DIR/helm/aegisops" -n "$NAMESPACE" "${ARGS[@]}" > "$RENDER_DIR/app.yaml"
  info "预演通过；受限渲染目录：$RENDER_DIR（可能含生成的 Secret，不要提交）"
  exit 0
fi
if [[ $DIFF -eq 1 ]]; then
  [[ ${#secrets[@]} -eq 0 ]] || die "diff 不接收明文凭据配置；先准备 existingSecret，再使用 values"
  "$HELM_BIN" upgrade --install "$RELEASE" "$SCRIPT_DIR/helm/aegisops" -n "$NAMESPACE" "${ARGS[@]}" --dry-run=server --hide-secret
  exit $?
fi
command -v "$KUBECTL_BIN" >/dev/null || die "需要 kubectl"
# Validate values and required references before creating even a namespace.
"$HELM_BIN" template "$RELEASE" "$SCRIPT_DIR/helm/aegisops" -n "$NAMESPACE" "${ARGS[@]}" > "$WORK_DIR/validation.yaml"
info "目标：$("$KUBECTL_BIN" config current-context) / namespace=$NAMESPACE / release=$RELEASE"
if [[ $YES -eq 0 ]]; then
  [[ -t 0 ]] || die "非交互安装请显式 --yes；升级前请备份数据库、对象存储和加密 Secret"
  read -r -p '确认安装/更新（已备份，确认 schema 兼容）？[y/N] ' answer
  [[ "$answer" == y || "$answer" == Y ]] || die "已取消"
fi
# Never silently adopt a script-managed instance or reset an existing database.
if ! "$HELM_BIN" status "$RELEASE" -n "$NAMESPACE" >/dev/null 2>&1; then
  existing="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get deployment "$RELEASE-server" --ignore-not-found -o name)" || die "无法检查既有部署"
  [[ -z "$existing" ]] || die "发现未归属本 Helm Release 的实例；请先按文档完成备份与显式接管迁移"
fi
BUILD_TOOL="${CONTAINER_TOOL:-docker}"
if [[ "${cfg[BUILD]:-false}" == true ]]; then
  [[ -n "${cfg[IMAGE]:-}" && -n "${cfg[TAG]:-}" ]] || die "源码构建需显式 IMAGE 和 TAG；TAG 不会被改写"
  [[ -f "$REPO_ROOT/Dockerfile" ]] || die "当前目录不是完整源码检出（缺少 Dockerfile）；快速体验必须从项目根目录执行"
  command -v "$BUILD_TOOL" >/dev/null || die "找不到构建工具 $BUILD_TOOL；可用 CONTAINER_TOOL 指定 docker/podman"
  if git -C "$REPO_ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    dirty="$(git -c core.filemode=false -C "$REPO_ROOT" status --porcelain --untracked-files=no)"
    commit="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
  else
    dirty=""; commit="unknown"
  fi
  [[ -z "$dirty" || $ALLOW_DIRTY -eq 1 ]] || die "源码内容有未提交改动；提交/还原或 --allow-dirty（TAG 不变）"
  ensure_distribution_ready
  built_images=()
  "$BUILD_TOOL" build -f "$REPO_ROOT/Dockerfile" \
    --build-arg "VERSION=${cfg[TAG]}" --build-arg "COMMIT=$commit" \
    --build-arg "GOPROXY=${cfg[GOPROXY]:-https://proxy.golang.org,direct}" \
    --build-arg "KUBECTL_VERSION=${cfg[KUBECTL_VERSION]-v1.29.15}" \
    --build-arg "KUBECTL_DOWNLOAD_BASE=${cfg[KUBECTL_DOWNLOAD_BASE]:-https://files.m.daocloud.io/dl.k8s.io/release}" \
    -t "${cfg[IMAGE]}:${cfg[TAG]}" "$REPO_ROOT" || die "镜像构建失败：${cfg[IMAGE]}:${cfg[TAG]}"
  built_images+=("${cfg[IMAGE]}:${cfg[TAG]}")
  if [[ "${cfg[FORM]:-standard}" != allinone && "${cfg[FORM]:-standard}" != minimal ]]; then
    web="${cfg[IMAGE_WEB]:-${cfg[IMAGE]}-web}"
    "$BUILD_TOOL" build -f "$SCRIPT_DIR/separated/web.Dockerfile" --build-arg "VERSION=${cfg[TAG]}" --build-arg "COMMIT=$commit" -t "$web:${cfg[TAG]}" "$REPO_ROOT" || die "Web 镜像构建失败：$web:${cfg[TAG]}"
    built_images+=("$web:${cfg[TAG]}")
    set_value web.repository "$web"
  fi
  for image in "${built_images[@]}"; do distribute_image "$image"; done
fi
"$KUBECTL_BIN" create namespace "$NAMESPACE" --dry-run=client -o yaml | "$KUBECTL_BIN" apply -f - >/dev/null
for secret in "${secrets[@]}"; do
  case "$secret" in mysql) secret_name="$RELEASE-mysql";; s3) secret_name="$RELEASE-s3";; oidc) secret_name="$RELEASE-oidc-ca";; esac
  existing_secret="$("$KUBECTL_BIN" --request-timeout=15s -n "$NAMESPACE" get secret "$secret_name" --ignore-not-found -o name)" || die "无法检查 Secret $secret_name"
  if [[ -n "$existing_secret" ]]; then info "复用 Secret $secret_name；参数文件不覆盖已有凭据，轮换请显式管理 Secret"; continue; fi
  case "$secret" in
    mysql) "$KUBECTL_BIN" -n "$NAMESPACE" create secret generic "$RELEASE-mysql" --from-file="dsn=$WORK_DIR/dsn" --dry-run=client -o yaml;;
    s3) "$KUBECTL_BIN" -n "$NAMESPACE" create secret generic "$RELEASE-s3" --from-env-file="$blob_file" --dry-run=client -o yaml;;
    oidc) "$KUBECTL_BIN" -n "$NAMESPACE" create secret generic "$RELEASE-oidc-ca" --from-file="ca.crt=${cfg[OIDC_CA_FILE]}" --dry-run=client -o yaml;;
  esac | "$KUBECTL_BIN" create -f - >/dev/null
done
# No atomic rollback: a Helm rollback cannot undo database DDL/data migrations.
if ! "$HELM_BIN" upgrade --install "$RELEASE" "$SCRIPT_DIR/helm/aegisops" -n "$NAMESPACE" "${ARGS[@]}" --wait --wait-for-jobs --timeout 15m; then
  diagnose_failure
  die "Helm 安装/更新失败；请按上方诊断先修复镜像分发或集群条件"
fi
info "应用安装完成；MySQL/S3 由部署方独立管理。模型与集群请登录页面配置。"
quickstart_summary
