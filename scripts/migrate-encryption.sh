#!/usr/bin/env bash
# migrate-encryption.sh —— 修复「加密密钥轮换后存量密文不可读」的编排脚本。
#
# 场景：平台凭证加密密钥 AEGISOPS_ENC_KEY 被轮换（如 Secret 换成新密钥），但
# 数据库里已经是旧密钥加密的存量密文，Server 启动后出现：登录 401、集群连不上、
# 聊天/模型配置读不出（settings 敏感值 API Key、credentials.kubeconfig_secret
# 等，全部只能被加密它的那把 key 解开）。
#
# 本脚本在安装机/控制节点上执行（需要 kubectl 与 mysql 客户端或 docker）：
#   1) 备份：两个密钥 Secret + mysqldump 全库（回滚锚点）；
#   2) 导出受影响的敏感记录（credentials、settings 中 sensitive=1）为 .b64 文件；
#   3) 用 cmd/aopsmig 逐条分类：new=新 key 可解 / old=仅旧 key 可解 / both=都不可解；
#   4) 生成 apply.sql（事务内回写重加密结果 + 可选重置 admin 口令哈希并踢下线）；
#   5) --apply 才实际执行；最后做数据库与 API（登录 → /clusters/current）验证。
#
# 安全约定：全程不打印密钥/口令/明文密文原文，只输出条数、长度与前缀；任何
# 新旧 key 都解不开的记录都会整体中止，绝不产生"半迁移"结果。
#
# 用法:
#   scripts/migrate-encryption.sh check [flags]    # 只读预检 + 生成 apply.sql 预览
#   scripts/migrate-encryption.sh apply [flags]    # 预检后执行回写并验证（小心!）
#
# 参数:
#   --namespace <ns>        产品命名空间 (默认 aegisops)
#   --mysql-image <img>     docker 里跑 mysql 客户端的镜像 (默认 mysql:8.0)
#   --old-key-hex <hex>     被轮换掉的旧密钥 (必填, 64位hex)
#   --new-key-hex <hex>     当前/新密钥 (默认取 Secret aegisops-credentials/key)
#   --admin-password <pw>   可选; 提供时重置 admin 口令哈希并 session_version+1
#   --dsn <dsn>             可选; 覆盖从 Secret aegisops-mysql/dsn 读到的连接串
#   --workdir <dir>         工作目录 (默认 /tmp/aopsmig)
#   --aopsmig <bin>         可选; 指向已编译的 cmd/aopsmig 可执行文件
#   --no-backup             跳过备份（仅连接验证类场景，不推荐）
set -euo pipefail

NS=aegisops
MYSQL_IMAGE=mysql:8.0
OLD_KEY_HEX=""
NEW_KEY_HEX=""
ADMIN_PASSWORD=""
DSN_OVERRIDE=""
WORKDIR=/tmp/aopsmig
AOPSMIG_BIN=""
DO_BACKUP=1
MODE=""

parse_args() {
  MODE="$1"; shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --namespace) NS="$2"; shift 2 ;;
      --mysql-image) MYSQL_IMAGE="$2"; shift 2 ;;
      --old-key-hex) OLD_KEY_HEX="$2"; shift 2 ;;
      --new-key-hex) NEW_KEY_HEX="$2"; shift 2 ;;
      --admin-password) ADMIN_PASSWORD="$2"; shift 2 ;;
      --dsn) DSN_OVERRIDE="$2"; shift 2 ;;
      --workdir) WORKDIR="$2"; shift 2 ;;
      --aopsmig) AOPSMIG_BIN="$2"; shift 2 ;;
      --no-backup) DO_BACKUP=0; shift ;;
      --) shift; break ;;
      *) echo "未知参数: $1" >&2; exit 1 ;;
    esac
  done
  case "$MODE" in
    check|apply) ;;
    *) echo "mode 只能是 check 或 apply" >&2; exit 1 ;;
  esac
  [ -n "$OLD_KEY_HEX" ] || { echo "缺少 --old-key-hex（被轮换掉的旧密钥）" >&2; exit 1; }
}

fatal() { echo "FATAL: $*" >&2; exit 1; }

# ---- 从 k8s Secret 读取密钥（优先 AEGISOPS_ENC_KEY 环境变量 / --new-key-hex）----
resolve_keys() {
  if [ -z "$NEW_KEY_HEX" ]; then
    NEW_KEY_HEX="${AEGISOPS_ENC_KEY:-}"
  fi
  if [ -z "$NEW_KEY_HEX" ]; then
    NEW_KEY_HEX="$(kubectl -n "$NS" get secret aegisops-credentials -o jsonpath='{.data.key}' 2>/dev/null | base64 -d || true)"
  fi
  [ -n "${NEW_KEY_HEX:-}" ] || fatal "无法取得当前/新密钥：请设 --new-key-hex 或环境变量 AEGISOPS_ENC_KEY，或提供 aegisops-credentials/key"
  echo "key sources:"
  echo "  new_key_sha16=$(printf '%s' "$NEW_KEY_HEX" | sha256sum | cut -c1-16)"
  echo "  old_key_sha16=$(printf '%s' "$OLD_KEY_HEX" | sha256sum | cut -c1-16)"
  [ "$NEW_KEY_HEX" != "$OLD_KEY_HEX" ] || echo "  警告: 新旧密钥相同，正常的 key 轮换场景不应如此"
  export AEGISOPS_ENC_KEY="$NEW_KEY_HEX"
  export AEGISOPS_KEY_OLD="$OLD_KEY_HEX"
}

# ---- DSN 解析（取最后一个 @ 分隔用户信息；兼容 tcp(host:port) 形式）----
resolve_dsn() {
  DSN="${DSN_OVERRIDE:-$(kubectl -n "$NS" get secret aegisops-mysql -o jsonpath='{.data.dsn}' 2>/dev/null | base64 -d || true)}"
  [ -n "${DSN:-}" ] || fatal "无法读取 aegisops-mysql/dsn，请用 --dsn 显式提供"
  DSN_NORM="$(printf '%s' "$DSN" | sed -E 's#tcp\(([^)]*)\)#\1#g')"
  DSN_NOQ="${DSN_NORM%%\?*}"
  USERINFO="${DSN_NOQ%@*}"
  SERVERDB="${DSN_NOQ##*@}"
  USER="${USERINFO%%:*}"; PASS="${USERINFO#*:}"
  HOSTPORT="${SERVERDB%%/*}"; DB="${SERVERDB##*/}"
  HOST="${HOSTPORT%%:*}"; PORT="${HOSTPORT##*:}"
  [ -n "$HOST" ] && [ -n "$PORT" ] && [ -n "$DB" ] || fatal "DSN 解析失败（期望 user:pass@host:port/db）"
  echo "  db=$DB user=$USER host=$HOST port=$PORT dbpass_len=${#PASS}"
}

# mysql 客户端：优先本机 mysql，否则 docker（master 上常无 mysql 客户端）。
mysql_run() {
  if command -v mysql >/dev/null 2>&1; then
    MYSQL_PWD="$PASS" mysql -h"$HOST" -P"$PORT" -u"$USER" "$DB" "$@"
  else
    docker run --rm -i --network host -e MYSQL_PWD="$PASS" "$MYSQL_IMAGE" mysql -h"$HOST" -P"$PORT" -u"$USER" "$DB" "$@"
  fi
}

backup() {
  [ "$DO_BACKUP" = 1 ] || { echo "--no-backup, 跳过备份"; return 0; }
  TS="$(date +%Y%m%d_%H%M%S)"
  mkdir -p "$WORKDIR/backup_$TS"
  kubectl -n "$NS" get secret aegisops-credentials -o yaml > "$WORKDIR/backup_$TS/secret_credentials.yaml" 2>/dev/null || true
  kubectl -n "$NS" get secret aegisops-enc -o yaml > "$WORKDIR/backup_$TS/secret_enc.yaml" 2>/dev/null || true
  if command -v mysqldump >/dev/null 2>&1; then
    MYSQL_PWD="$PASS" mysqldump -h"$HOST" -P"$PORT" -u"$USER" --single-transaction --routines --triggers "$DB" > "$WORKDIR/backup_$TS/dump.sql"
  else
    docker run --rm --network host -e MYSQL_PWD="$PASS" "$MYSQL_IMAGE" mysqldump -h"$HOST" -P"$PORT" -u"$USER" --single-transaction --routines --triggers "$DB" > "$WORKDIR/backup_$TS/dump.sql"
  fi
  echo "backup -> $WORKDIR/backup_$TS"
  echo "  secret_credentials=$(wc -l < "$WORKDIR/backup_$TS/secret_credentials.yaml") lines"
  echo "  dump_inserts=$(grep -c 'INSERT' "$WORKDIR/backup_$TS/dump.sql")"
}

# 导出受影响敏感记录：credentials.kubeconfig_secret + settings(sensitive=1) 的 value。
# 每个文件一个密文，文件名=sanitize(记录标识)，migrate 会为其生成同名 .new。
export_rows() {
  rm -rf "$WORKDIR/rows"; mkdir -p "$WORKDIR/rows"
  # credentials: 全部非空 kubeconfig_secret
  while IFS=$'\t' read -r cid kc; do
    [ -n "$kc" ] || continue
    fname="$(printf 'cred_%s' "$cid" | tr -c 'A-Za-z0-9_.' '_')"
    printf '%s\n' "$kc" > "$WORKDIR/rows/$fname.b64"
  done < <(mysql_run -N -B -e "SELECT id, kubeconfig_secret FROM credentials WHERE kubeconfig_secret <> '';")
  # settings: sensitive=1 的非空 value（sensitive 是 MySQL 8 保留字，统一反引号）
  while IFS=$'\t' read -r grp key val; do
    [ -n "$val" ] || continue
    fname="$(printf 'set_%s__%s' "$grp" "$key" | tr -c 'A-Za-z0-9_.' '_')"
    printf '%s\n' "$val" > "$WORKDIR/rows/$fname.b64"
  done < <(mysql_run -N -B -e "SELECT group_name, key_name, value FROM settings WHERE \`sensitive\` = 1 AND value <> '';")
  echo "exported rows -> $WORKDIR/rows ($(ls "$WORKDIR/rows"/*.b64 2>/dev/null | wc -l) records)"
}

# sql 单引号安全校验（密文是 base64，理论上不含单引号；双保险）。
sql_value() {
  case "$1" in
    *"'"*) fatal "值含单引号，拒绝生成 SQL（不应发生，密文应为 base64）" ;;
    "") fatal "空值" ;;
  esac
  printf '%s' "$1"
}

build_apply_sql() {
  rm -f "$WORKDIR/apply.sql"
  : > "$WORKDIR/apply.sql"
  echo "START TRANSACTION;" >> "$WORKDIR/apply.sql"
  migrated=0
  for f in "$WORKDIR/rows"/*.new; do
    [ -e "$f" ] || continue
    base="$(basename "$f" .new)"
    new="$(sql_value "$(cat "$f")")"
    case "$base" in
      cred_*) id="${base#cred_}"; echo "UPDATE credentials SET kubeconfig_secret='$new', updated_at=NOW() WHERE id='$id';" >> "$WORKDIR/apply.sql"; migrated=$((migrated+1));;
      set_*) grp="${base#set_}"; grp="${grp%%__*}"; key="${base#set_${grp}__}";
             echo "UPDATE settings SET \`value\`='$new', updated_at=NOW() WHERE group_name='$grp' AND key_name='$key';" >> "$WORKDIR/apply.sql"; migrated=$((migrated+1));;
      *) echo "  (跳过无法识别的记录: $base)" >&2;;
    esac
  done
  # admin 重置（可选）必须在 COMMIT 前。
  if [ -n "$ADMIN_PASSWORD" ] && [ -f "$WORKDIR/rows/admin.hash" ]; then
    AH="$(sql_value "$(cat "$WORKDIR/rows/admin.hash")")"
    echo "UPDATE users SET password_hash='$AH', session_version=session_version+1, updated_at=NOW() WHERE username='admin';" >> "$WORKDIR/apply.sql"
    echo "  + admin 口令重置并踢下线: users.username=admin"
  fi
  if [ "$migrated" -eq 0 ] && [ -z "$ADMIN_PASSWORD" ]; then
    echo "  (无需要回写的记录)" >&2
  fi
  echo "COMMIT;" >> "$WORKDIR/apply.sql"
  echo "apply.sql -> $WORKDIR/apply.sql ($migrated UPDATE)"
}

apply_sql() {
  [ -f "$WORKDIR/apply.sql" ] || fatal "缺少 apply.sql"
  mysql_run < "$WORKDIR/apply.sql"
  echo "applied: $WORKDIR/apply.sql"
}

verify() {
  echo "== DB 验证 =="
  echo "  credentials:"
  mysql_run -N -B -e "SELECT id, CHAR_LENGTH(kubeconfig_secret), LEFT(kubeconfig_secret,3) FROM credentials;" 2>/dev/null | sed 's/^/    /'
  echo "  settings (sensitive=1):"
  mysql_run -N -B -e "SELECT group_name, key_name, CHAR_LENGTH(value), SUBSTR(SHA2(value,256),1,16) FROM settings WHERE \`sensitive\`=1;" 2>/dev/null | sed 's/^/    /'
  echo "  users admin:"
  mysql_run -N -B -e "SELECT username, CHAR_LENGTH(password_hash), LEFT(password_hash,3) FROM users WHERE username='admin';" 2>/dev/null | sed 's/^/    /'

  if [ -n "${ADMIN_PASSWORD:-}" ]; then
    echo "== API 验证 (登录 -> /clusters/current) =="
    cat <<'EOF'
    验证命令示例（把 $H 换成实际 Ingress 域名）:
      H=<你的ingress域名>
      curl -ks -c /tmp/ck -H "Origin: $H" -H "Content-Type: application/json" \
        -d "{\"username\":\"admin\",\"password\":\"<admin密码>\"}" "$H/api/v1/auth/login"
      curl -ks -b /tmp/ck -H "Origin: $H" "$H/api/v1/clusters/current"   # 期望 status:"connected"
EOF
  fi
}

# ---- 用 cmd/aopsmig 做文件级分类/迁移 ----
run_migrator() {
  if [ -z "$AOPSMIG_BIN" ]; then
    AOPSMIG_BIN="$WORKDIR/aopsmig"
    [ -x "$AOPSMIG_BIN" ] || go build -o "$AOPSMIG_BIN" ./cmd/aopsmig
  fi
  export AEGISOPS_ENC_KEY AEGISOPS_KEY_OLD
  if [ "$MODE" = "apply" ]; then
    echo "== aopsmig migrate (在 $WORKDIR/rows 内写 .new 与 admin.hash) =="
    # apply 时中途双不可解 → 中止且不留任何半产物
    "$AOPSMIG_BIN" migrate "$WORKDIR/rows" --admin-password "$ADMIN_PASSWORD"
  else
    echo "== aopsmig check (只读) =="
    set +e
    "$AOPSMIG_BIN" check "$WORKDIR/rows"
    rc="$?"
    set -e
    # 0=全部已由新 key 可解；2=存在需迁移（正常，继续给出 apply 指引）；
    # 3=存在新旧 key 都解不开的密文，必须人工核对密钥来源，不能当成"预检通过"。
    if [ "$rc" = 3 ]; then
      echo "check 结果: 存在双不可解记录，请核对 --old-key-hex 是否为轮换前的真正密钥" >&2
      exit 3
    fi
  fi
}

main() {
  parse_args "$@"
  resolve_dsn
  resolve_keys
  backup
  export_rows
  if [ "$MODE" = "apply" ]; then
    run_migrator
    build_apply_sql
    apply_sql
    verify
  else
    # check：只读分类报告，不写任何 .new / apply.sql。
    run_migrator
    echo
    echo "== dry-run 完成 =="
    echo "以上为只读预检。确认每个记录的状态后，用 apply 回写："
    echo "  scripts/migrate-encryption.sh apply --namespace $NS --old-key-hex <old> --new-key-hex <new>"
    echo "  （可选 --admin-password <pw> 一并重置 admin 口令并踢下线）"
    exit 0
  fi
}

main "$@"
