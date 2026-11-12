#!/usr/bin/env bash
# Shared pinned Helm client; never installs middleware or changes global PATH.
aegisops_helm_client() {
if ! command -v "$HELM_BIN" >/dev/null; then
  [[ "$HELM_BIN" == helm ]] || die "HELM_BIN 指定的文件不存在"
  [[ "$(uname -s)" == Linux ]] || die "自动准备 Helm 仅支持 Linux；其它系统请安装 Helm"
  arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) die "不支持的 Helm 架构 $arch";; esac
  helm_dir="${XDG_CACHE_HOME:-$HOME/.cache}/aegisops/helm-v3.17.3-$arch"
  mkdir -p "$helm_dir"; chmod 700 "$helm_dir"
  if [[ ! -x "$helm_dir/linux-$arch/helm" ]]; then
    [[ "${1:-cached}" == download ]] || die "找不到 Helm；请安装或先执行快速体验入口"
    command -v curl >/dev/null || die "自动准备 Helm 需要 curl"
    archive="helm-v3.17.3-linux-$arch.tar.gz"
    curl --connect-timeout 15 --max-time 120 -fsSL "https://get.helm.sh/$archive" -o "$helm_dir/$archive"
    curl --connect-timeout 15 --max-time 60 -fsSL "https://get.helm.sh/$archive.sha256sum" -o "$helm_dir/$archive.sha256sum"
    (cd "$helm_dir" && sha256sum -c "$archive.sha256sum" >/dev/null) || die "Helm 校验失败"
    tar -xzf "$helm_dir/$archive" -C "$helm_dir"
  fi
  HELM_BIN="$helm_dir/linux-$arch/helm"
fi
}
