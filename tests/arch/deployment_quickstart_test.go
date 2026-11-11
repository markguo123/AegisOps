package arch_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestQuickstartOfflineDefaults(t *testing.T) {
	bash := findBash()
	if bash == "" {
		t.Skip("requires Bash")
	}
	dir := renderManifests(t, bash, "--PROFILE=quickstart")
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "base", "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &cm); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Platform struct {
			Auth struct {
				HTTP bool `yaml:"allow_quickstart_http"`
			} `yaml:"auth"`
			Store struct {
				Driver string `yaml:"driver"`
			} `yaml:"store"`
		} `yaml:"platform"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Platform.Auth.HTTP || cfg.Platform.Store.Driver != "sqlite" {
		t.Fatalf("wrong defaults: %+v", cfg)
	}
	var dep struct {
		Spec struct {
			Template struct {
				Spec struct {
					Volumes []struct {
						Name     string         `yaml:"name"`
						EmptyDir map[string]any `yaml:"emptyDir"`
						PVC      any            `yaml:"persistentVolumeClaim"`
					} `yaml:"volumes"`
					Containers []struct {
						Policy string `yaml:"imagePullPolicy"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	b, err = os.ReadFile(filepath.Join(dir, "allinone", "deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, &dep); err != nil {
		t.Fatal(err)
	}
	data := false
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name == "data" {
			data = v.EmptyDir != nil && v.PVC == nil
		}
	}
	if !data || dep.Spec.Template.Spec.Containers[0].Policy != "Always" {
		t.Fatal("preview must show ephemeral SQLite and no stale image cache")
	}
}

func TestQuickstartStorageSelection(t *testing.T) {
	bash := findBash()
	if bash == "" {
		t.Skip("requires Bash")
	}
	script := readRepoFile(t, "deploy", "install.sh")
	start := strings.Index(script, "resolve_quickstart_storage() {")
	end := strings.Index(script[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("missing storage resolver")
	}
	function := script[start : start+end+3]
	for _, tc := range []struct {
		name, state, classes string
		deployed             bool
		want                 string
		fail                 bool
	}{
		{"no-class", "", "", false, "false|", false},
		{"default-class", "", "fast true", false, "true|fast", false},
		{"beta-class", "", "fast  true", false, "true|fast", false},
		{"pinned-volume", "true", "other true", true, "true|", false},
		{"multiple-defaults", "", "fast true\nslow true", false, "", true},
		{"existing-temporary", "false", "fast true", true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := fmt.Sprintf(`set -Eeuo pipefail
NAMESPACE=experience
STORAGE_CLASS=""
die() { echo "$1" >&2; exit 2; }
warn() { :; }
mock() {
 case "$*" in
 *"get configmap"*) printf '%%s' "$TEST_STATE";;
 *"get deployment"*) %s;;
 *"get pvc"*) :;;
 *"get storageclass"*) printf '%%s' "$TEST_CLASSES";;
 *) exit 3;;
 esac
}
KUBECTL_BIN=mock
%s
resolve_quickstart_storage
printf 'result=%%s|%%s\n' "$PERSIST_REPORTS" "$STORAGE_CLASS"
`, map[bool]string{true: "return 0", false: "return 1"}[tc.deployed], function)
			cmd := exec.Command(bash, "-c", code)
			cmd.Env = append(os.Environ(), "TEST_STATE="+tc.state, "TEST_CLASSES="+tc.classes)
			out, err := cmd.CombinedOutput()
			if tc.fail {
				if err == nil {
					t.Fatalf("expected refusal: %s", out)
				}
				return
			}
			if err != nil || !strings.Contains(string(out), "result="+tc.want) {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestHelmDeploymentContracts(t *testing.T) {
	helm := os.Getenv("AEGISOPS_TEST_HELM")
	if helm == "" {
		var err error
		helm, err = exec.LookPath("helm")
		if err != nil {
			t.Skip("requires Helm")
		}
	}
	chart, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "aegisops"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"minimal", "standard"} {
		t.Run(mode, func(t *testing.T) {
			out, err := exec.Command(helm, "template", "aegisops", chart, "--namespace", "experience", "--set", "image.tag=test", "--set", "database.driver=sqlite", "--set", "mode="+mode, "--set", "credentials.existingSecret=kept").CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			decoder := yaml.NewDecoder(strings.NewReader(string(out)))
			deployments := 0
			for {
				var doc map[string]any
				if err := decoder.Decode(&doc); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("invalid rendered YAML: %v", err)
				}
				if doc["kind"] != "Deployment" {
					continue
				}
				deployments++
				meta := doc["metadata"].(map[string]any)
				if !strings.HasSuffix(meta["name"].(string), "-server") {
					continue
				}
				spec := doc["spec"].(map[string]any)
				if spec["replicas"] != 1 || spec["strategy"].(map[string]any)["type"] != "Recreate" {
					t.Fatal("backend must be single-replica Recreate")
				}
				pod := spec["template"].(map[string]any)["spec"].(map[string]any)
				init := pod["initContainers"].([]any)[0].(map[string]any)
				app := pod["containers"].([]any)[0].(map[string]any)
				for _, c := range []map[string]any{init, app} {
					found := false
					for _, v := range c["volumeMounts"].([]any) {
						mount := v.(map[string]any)
						if mount["name"] == "data" && mount["mountPath"] == "/data" {
							found = true
						}
					}
					if !found {
						t.Fatal("migration and app must share /data")
					}
				}
			}
			want := 1
			if mode == "standard" {
				want = 2
			}
			if deployments != want {
				t.Fatalf("got %d deployments, want %d", deployments, want)
			}
		})
	}
	for _, args := range [][]string{
		{"--set", "image.tag=test"}, // no MySQL Secret
		{"--set", "image.tag=test", "--set", "database.driver=sqlite", "--set", "persistence.enabled=false"},
		{"--set", "image.tag=test", "--set", "database.driver=sqlite", "--set", "server.publicURL=http://example.test"},
	} {
		cmd := exec.Command(helm, append([]string{"template", "aegisops", chart}, args...)...)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Fatalf("unsafe configuration accepted: %v\n%s", args, out)
		}
	}
}

func TestRemotePipeBootstrapUsesVerifiedRelease(t *testing.T) {
	bash := findBash()
	if bash == "" {
		t.Skip("requires Bash")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	bundle := t.TempDir()
	pack := exec.Command(bash, "deploy/package.sh", filepath.ToSlash(bundle))
	pack.Dir = root
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("pack: %v: %s", err, out)
	}
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt=%v", corrupt), func(t *testing.T) {
			// Execute the real entry point through stdin, from outside the repository.
			// Mock only transport, keeping tar extraction and checksum verification real.
			code := `set -Eeuo pipefail
curl() {
 local output="" url=""
 while [[ $# -gt 0 ]]; do
   case "$1" in -o) output="$2"; shift;; https://*) url="$1";; esac
   shift
 done
 case "$url" in
 *.sha256) cp "$TEST_BUNDLE/aegisops-install.tar.gz.sha256" "$output";;
 *.tar.gz) cp "$TEST_BUNDLE/aegisops-install.tar.gz" "$output"; if [[ "$TEST_CORRUPT" == true ]]; then printf corrupt >> "$output"; fi;;
 *) return 9;;
 esac
}
export -f curl
cat "$TEST_INSTALL" | bash -s -- --dry-run --PROFILE=quickstart --TAG=vtest
`
			cmd := exec.Command(bash, "-c", code)
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "TEST_BUNDLE="+filepath.ToSlash(bundle), "TEST_INSTALL="+filepath.ToSlash(filepath.Join(root, "deploy", "install.sh")), fmt.Sprintf("TEST_CORRUPT=%v", corrupt), "AEGISOPS_INSTALL_NO_TEE=1", "TMPDIR=/tmp")
			out, err := cmd.CombinedOutput()
			if corrupt {
				if err == nil || !strings.Contains(string(out), "安装包校验失败") {
					t.Fatalf("bad bundle accepted: %v: %s", err, out)
				}
				return
			}
			if err != nil || !strings.Contains(string(out), "预演通过") || !strings.Contains(string(out), ":vtest") {
				t.Fatalf("bootstrap failed: %v: %s", err, out)
			}
		})
	}
}
