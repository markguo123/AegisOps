package arch_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func dsBash(t *testing.T) string {
	t.Helper()
	candidates := []string{os.Getenv("AEGISOPS_TEST_BASH")}
	if p, e := exec.LookPath("bash"); e == nil {
		candidates = append(candidates, p)
	}
	if runtime.GOOS == "windows" {
		candidates = append(candidates, `D:\Program Files\Git\bin\bash.exe`, `C:\Program Files\Git\bin\bash.exe`)
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		out, e := exec.CommandContext(ctx, p, "-c", "printf aegisops-bash").Output()
		cancel()
		if e == nil && string(out) == "aegisops-bash" {
			return p
		}
	}
	t.Skip("requires Bash")
	return ""
}
func dsHelm(t *testing.T) string {
	t.Helper()
	p := os.Getenv("AEGISOPS_TEST_HELM")
	if p == "" {
		p, _ = exec.LookPath("helm")
	}
	if p == "" {
		t.Skip("requires Helm")
	}
	return filepath.ToSlash(p)
}
func dsRoot(t *testing.T) string {
	t.Helper()
	p, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	return p
}

// Only the transport is mocked. Helm template and all installer decisions run as shipped.
const dsTransport = `
helm() {
 printf 'helm %s\n' "$*" >> "$TEST_LOG"
 case "$1" in
 template) "$REAL_HELM" "$@";;
 status) [[ "${TEST_HAS_RELEASE:-false}" == true ]];;
 list) printf '[{"status":"%s"}]' "${TEST_RELEASE_STATE:-deployed}";;
 upgrade|rollback|history) return 0;;
 *) return 7;; esac
}
kubectl() {
 printf 'kubectl %s\n' "$*" >> "$TEST_LOG"
 case "$*" in
 *'config current-context'*) printf '%s' "${TEST_CONTEXT:-kind-aegisops}";;
 *'get configmap'*) printf '%s' "${TEST_STATE:-}";;
 *'get deployment'*) if [[ "$*" == *'-o name'* ]]; then printf '%s' "${TEST_DEPLOYMENT:-}"; else printf '%s' "${TEST_ACTUAL:-}"; fi;;
 *'get storageclass'*) printf '%s' "${TEST_CLASSES:-}";;
 *'get secret'*) if [[ "${TEST_EXISTING_SECRET:-false}" == true && "$*" == *'-o name'* ]]; then printf secret/existing; fi;;
 *'create namespace'*) printf 'apiVersion: v1\nkind: Namespace\nmetadata: {name: aegisops}\n';;
 *'create secret generic'*) printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: fixture}\n';;
 *'apply -f -'*|*'create -f -'*) cat >/dev/null;;
 *'get service'*|*'get nodes'*) return 0;;
 *) echo "unexpected kubectl transport" >&2; return 9;; esac
}
kind() { printf 'kind %s\n' "$*" >> "$TEST_LOG"; }
docker() { printf 'container %s\n' "$*" >> "$TEST_LOG"; }
export -f helm kubectl kind docker
export HELM_BIN=helm KUBECTL_BIN=kubectl CONTAINER_TOOL=docker
`

func dsRun(t *testing.T, entry string, env []string, args ...string) (string, string, error) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	launcher := dsTransport + "\nbash \"$@\""
	argv := append([]string{"-c", launcher, "transport", entry}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dsBash(t), argv...)
	cmd.Dir = dsRoot(t)
	cmd.Env = append(os.Environ(), "REAL_HELM="+dsHelm(t), "TEST_LOG="+filepath.ToSlash(log), "TMPDIR=/tmp", "AEGISOPS_SHOW_INITIAL_PASSWORD=false")
	cmd.Env = append(cmd.Env, env...)
	out, e := cmd.CombinedOutput()
	trace, _ := os.ReadFile(log)
	return string(out), string(trace), e
}

func TestDeploymentSingleCommandQuickstart(t *testing.T) {
	for _, tc := range []struct{ name, classes, want string }{{"no-storage-class", "", "persistence.enabled=false"}, {"default-storage-class", "true", "persistence.enabled=true"}} {
		t.Run(tc.name, func(t *testing.T) {
			out, trace, e := dsRun(t, "deploy/install.sh", []string{"TEST_CLASSES=" + tc.classes}, "--allow-dirty")
			if e != nil {
				t.Fatalf("%v: %s", e, out)
			}
			for _, want := range []string{"helm upgrade --install aegisops", tc.want, "image.tag=qs-", "container build", "kind load docker-image aegisops:qs-"} {
				if !strings.Contains(trace, want) {
					t.Fatalf("missing %s: %s", want, trace)
				}
			}
			if strings.Contains(trace, "releases/download") {
				t.Fatalf("quickstart must not download product bundles: %s", trace)
			}
			if strings.Contains(trace, "get secret aegisops-mysql") || strings.Contains(trace, "get secret aegisops-s3") {
				t.Fatal("quickstart must not require middleware")
			}
		})
	}
}

func TestDeploymentQuickstartBuildsFromSource(t *testing.T) {
	// 指定 TAG 时原样使用，仍从当前源码构建并载入 kind，而不是解析远端发布。
	out, trace, e := dsRun(t, "deploy/install.sh", nil, "--allow-dirty", "--TAG=local-check")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	for _, want := range []string{"container build", "kind load docker-image aegisops:local-check", "image.tag=local-check", "helm upgrade --install"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("missing %s: %s", want, trace)
		}
	}
	if strings.Contains(trace, "releases/download") {
		t.Fatal("local build must not depend on product release")
	}
}

func TestDeploymentQuickstartDataProtection(t *testing.T) {
	// 已有临时实例：允许更新，但必须明确警告重建会丢数据。
	out, trace, e := dsRun(t, "deploy/install.sh", []string{"TEST_HAS_RELEASE=true", "TEST_STATE=false|sqlite|dir", "TEST_ACTUAL=1|aegisops:old"}, "--allow-dirty")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if !strings.Contains(out, "重建 Pod") || !strings.Contains(out, "丢失") {
		t.Fatalf("ephemeral rebuild risk not warned: %s", out)
	}
	if !strings.Contains(trace, "helm upgrade") || !strings.Contains(trace, "persistence.enabled=false") {
		t.Fatalf("ephemeral repeat must stay ephemeral: %s", trace)
	}

	// 持久化实例：直接更新，不切存储模式。
	_, trace, e = dsRun(t, "deploy/install.sh", []string{"TEST_HAS_RELEASE=true", "TEST_STATE=true|sqlite|dir"}, "--allow-dirty")
	if e != nil || !strings.Contains(trace, "helm upgrade") || !strings.Contains(trace, "persistence.enabled=true") {
		t.Fatalf("persistent repeat: %v %s", e, trace)
	}

	// 生产实例不属于快速体验，拒绝覆盖数据库或对象后端。
	_, trace, e = dsRun(t, "deploy/install.sh", []string{"TEST_HAS_RELEASE=true", "TEST_STATE=true|mysql|s3"})
	if e == nil || strings.Contains(trace, "helm upgrade") {
		t.Fatalf("production instance overwritten: %v %s", e, trace)
	}

	// 未归属实例拒绝接管。
	out, trace, e = dsRun(t, "deploy/install.sh", []string{"TEST_DEPLOYMENT=deployment/aegisops-server"})
	if e == nil || strings.Contains(trace, "helm upgrade") {
		t.Fatalf("unowned instance accepted: %v %s", e, out)
	}
}

func TestDeploymentRemoteClusterRequiresPush(t *testing.T) {
	// 非 kind/k3d 集群：未开启推送必须在构建后、部署前明确失败。
	out, trace, e := dsRun(t, "deploy/install.sh", []string{"TEST_CONTEXT=prod-cluster"}, "--allow-dirty", "--TAG=remote-check")
	if e == nil || !strings.Contains(out, "节点可达") || strings.Contains(trace, "helm upgrade") {
		t.Fatalf("remote cluster without push must fail before deploy: %v %s", e, out)
	}
	// 显式仓库与推送后允许部署。
	out, trace, e = dsRun(t, "deploy/install.sh", []string{"TEST_CONTEXT=prod-cluster"}, "--allow-dirty", "--TAG=remote-check", "--PUSH=true", "--IMAGE=registry.example.com/aegisops")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if !strings.Contains(trace, "container push registry.example.com/aegisops:remote-check") {
		t.Fatalf("push missing: %s", trace)
	}
	if !strings.Contains(trace, "helm upgrade") {
		t.Fatal("remote push path must still deploy")
	}
}

func TestDeploymentProductionBuildsWhenImageProvided(t *testing.T) {
	values := filepath.Join(t.TempDir(), "production.yaml")
	data := "database:\n  driver: mysql\n  existingSecret: mysql\nblobs:\n  driver: s3\n  existingSecret: s3\n"
	if e := os.WriteFile(values, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	// 生产显式给出 IMAGE 时默认从源码构建并推送，不要求手工先做镜像。
	out, trace, e := dsRun(t, "deploy/install.sh", []string{"TEST_CONTEXT=prod-cluster", "TEST_HAS_RELEASE=true"}, "--profile=production", "--values="+filepath.ToSlash(values), "--IMAGE=registry.example.com/aegisops", "--TAG=prod-check", "--PUSH=true", "--allow-dirty", "--yes")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	for _, want := range []string{"container build", "container push registry.example.com/aegisops:prod-check", "image.tag=prod-check", "helm upgrade --install"} {
		if !strings.Contains(trace, want) {
			t.Fatalf("missing %s: %s", want, trace)
		}
	}
}

// TestDeploymentCacheBuildDegradesOnDockerDriver 锁住 --cache-build 的驱动降级：
// Docker 默认 docker 驱动不支持 type=local 缓存导出（真机报 Cache export is not supported
// for the docker driver），安装器必须自动降级为内联缓存而不是让整个构建失败。
func TestDeploymentCacheBuildDegradesOnDockerDriver(t *testing.T) {
	values := filepath.Join(t.TempDir(), "production.yaml")
	data := "database:\n  driver: mysql\n  existingSecret: mysql\nblobs:\n  driver: s3\n  existingSecret: s3\n"
	if e := os.WriteFile(values, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	// 传输层的 docker 是桩：buildx inspect 无输出 => driver 为空 => 必须走 docker 驱动降级。
	out, trace, e := dsRun(t, "deploy/install.sh", []string{"TEST_CONTEXT=prod-cluster", "TEST_HAS_RELEASE=true"},
		"--profile=production", "--values="+filepath.ToSlash(values),
		"--IMAGE=registry.example.com/aegisops", "--TAG=cache-check", "--PUSH=true", "--allow-dirty", "--cache-build", "--yes")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if strings.Contains(trace, "cache-to=type=local") {
		t.Fatalf("docker 驱动不应拿到本地缓存导出参数: %s", trace)
	}
	if !strings.Contains(trace, "BUILDKIT_INLINE_CACHE=1") {
		t.Fatalf("docker 驱动应降级为内联缓存: %s", trace)
	}
	if !strings.Contains(trace, "container push registry.example.com/aegisops:cache-check") {
		t.Fatalf("降级后仍应推送镜像: %s", trace)
	}
}

func TestDeploymentNamespaceAndSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.env")
	content := "PROFILE=production\nIMAGE=registry.invalid/aegisops\nTAG=release\nMYSQL_DSN='sentinel-secret@tcp(db:3306)/ops?parseTime=true'\nBLOBS_ENDPOINT=s3.invalid:9000\nBLOBS_ACCESS_KEY=fixture\nBLOBS_SECRET_KEY=sentinel-blob-secret\n"
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
	out, trace, e := dsRun(t, "deploy/install.sh", []string{"NAMESPACE=other", "TEST_HAS_RELEASE=true", "TEST_EXISTING_SECRET=true"}, "--namespace=aegisops", "--env-file="+filepath.ToSlash(path), "--no-build", "--yes")
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	if !strings.Contains(trace, "-n aegisops") || strings.Contains(trace, "-n other") {
		t.Fatalf("explicit namespace lost: %s", trace)
	}
	if strings.Contains(trace, "create secret") {
		t.Fatalf("existing credentials overwritten: %s", trace)
	}
	if strings.Contains(out+trace, "sentinel-") {
		t.Fatal("secret escaped into output or argv")
	}
	if !strings.Contains(trace, "image.tag=release") || strings.Contains(trace, "release-dirty") {
		t.Fatal("configured tag changed")
	}
	// --no-build 也必须有前端仓库：否则会回落 Chart 默认的 ghcr.io，现场表现为 ErrImagePull。
	if !strings.Contains(trace, "web.repository=registry.invalid/aegisops-web") {
		t.Fatalf("--no-build 必须从 IMAGE 派生前端仓库: %s", trace)
	}
}

func TestDeploymentNativeHelmManagement(t *testing.T) {
	out, trace, e := dsRun(t, "deploy/upgrade.sh", nil, "--rollback=3", "--namespace=target", "--release=ops")
	if e != nil || !strings.Contains(trace, "helm rollback ops 3 -n target") || strings.Contains(trace, "helm upgrade") {
		t.Fatalf("%v %s %s", e, out, trace)
	}
	_, trace, e = dsRun(t, "deploy/upgrade.sh", nil, "--status")
	// Mock status returns false for absent releases; the adapter must propagate it.
	if e == nil || !strings.Contains(trace, "helm status") {
		t.Fatal("failed status hidden")
	}
	_, _, e = dsRun(t, "deploy/install.sh", nil, "--with-mysql")
	if e == nil {
		t.Fatal("installer must not install middleware")
	}
}

func TestDeploymentNoExecutionOfEnvironmentFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed")
	envFile := filepath.Join(dir, "install.env")
	data := fmt.Sprintf("PROFILE=quickstart\nTAG='$(touch %s)'\n", filepath.ToSlash(marker))
	if e := os.WriteFile(envFile, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	// The literal invalid image tag may render, but must never be evaluated by a shell.
	_, _, _ = dsRun(t, "deploy/install.sh", nil, "--dry-run", "--env-file="+filepath.ToSlash(envFile))
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("environment file executed code")
	}
}

func TestDeploymentScriptsAndPackage(t *testing.T) {
	bash := dsBash(t)
	syntax := exec.Command(bash, "-n", "deploy/install.sh", "deploy/upgrade.sh", "deploy/lib.sh", "deploy/package.sh")
	syntax.Dir = dsRoot(t)
	if out, e := syntax.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	outDir := filepath.ToSlash(t.TempDir())
	pack := exec.Command(bash, "deploy/package.sh", outDir)
	pack.Dir = dsRoot(t)
	pack.Env = append(os.Environ(), "RELEASE_VERSION=v0.9.0-test")
	if out, e := pack.CombinedOutput(); e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	code := `cd "$1" && sha256sum -c aegisops-install.tar.gz.sha256 && tar -tzf aegisops-install.tar.gz`
	out, e := exec.Command(bash, "-c", code, "package", outDir).CombinedOutput()
	if e != nil {
		t.Fatalf("%v: %s", e, out)
	}
	for _, want := range []string{"deploy/lib.sh", "deploy/helm/aegisops/templates/migration.yaml", "deploy/install.sh"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(string(out), "deploy/base/") || strings.Contains(string(out), "aegisops-mysql/") {
		t.Fatal("package includes obsolete templates/middleware")
	}
	meta, err := exec.Command(bash, "-c", `cd "$1" && tar -xOzf aegisops-install.tar.gz deploy/helm/aegisops/Chart.yaml`, "metadata", outDir).CombinedOutput()
	if err != nil || !strings.Contains(string(meta), "version: 0.9.0-test") || !strings.Contains(string(meta), `appVersion: "v0.9.0-test"`) {
		t.Fatalf("release metadata not pinned: %v: %s", err, meta)
	}
}

func TestDeploymentHelmBootstrapIntegrityAndCache(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			code := `set -Eeuo pipefail
die() { echo "$*" >&2; exit 2; }
TEST_WORK="$(cd "$TEST_WORK" && pwd)"
uname() { if [[ "$1" == -s ]]; then echo Linux; else echo x86_64; fi; }
# Force the missing-client branch even on CI with a system Helm installed.
command() { if [[ "$1" == -v && "$2" == helm ]]; then return 1; fi; builtin command "$@"; }
mkdir -p "$TEST_WORK/source/linux-amd64"
printf '#!/usr/bin/env bash\nexit 0\n' > "$TEST_WORK/source/linux-amd64/helm"
chmod 755 "$TEST_WORK/source/linux-amd64/helm"
(cd "$TEST_WORK/source" && tar -czf helm-v3.17.3-linux-amd64.tar.gz linux-amd64 && sha256sum helm-v3.17.3-linux-amd64.tar.gz > helm-v3.17.3-linux-amd64.tar.gz.sha256sum)
curl() {
 local output='' url=''
 while [[ $# -gt 0 ]]; do case "$1" in -o) output="$2"; shift;; https://*) url="$1";; esac; shift; done
 cp "$TEST_WORK/source/${url##*/}" "$output"
 if [[ "$TEST_CORRUPT" == true && "$url" == *.tar.gz ]]; then printf corrupt >> "$output"; fi
}
export XDG_CACHE_HOME="$TEST_WORK/cache"
HELM_BIN=helm
. deploy/lib.sh
aegisops_helm_client download
test -x "$HELM_BIN"
curl() { echo 'cached lookup unexpectedly downloaded' >&2; exit 9; }
HELM_BIN=helm
aegisops_helm_client cached
test -x "$HELM_BIN"
`
			cmd := exec.Command(dsBash(t), "-c", code)
			cmd.Dir = dsRoot(t)
			cmd.Env = append(os.Environ(), "TEST_WORK="+filepath.ToSlash(t.TempDir()), fmt.Sprintf("TEST_CORRUPT=%v", corrupt))
			out, err := cmd.CombinedOutput()
			if corrupt {
				if err == nil || !strings.Contains(string(out), "Helm 校验失败") {
					t.Fatalf("corrupt client accepted: %v: %s", err, out)
				}
			} else if err != nil {
				t.Fatalf("bootstrap/cache failed: %v: %s", err, out)
			}
		})
	}
}
