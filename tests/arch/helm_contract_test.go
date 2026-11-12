package arch_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"gopkg.in/yaml.v3"
)

type hcDoc map[string]any

func hcHelm(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AEGISOPS_TEST_HELM"); p != "" {
		return p
	}
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("requires Helm (CI installs Helm before running deployment contracts)")
	}
	return p
}

func hcRender(t *testing.T, extra ...string) ([]hcDoc, []byte, error) {
	t.Helper()
	args := []string{"template", "aegisops", filepath.Join("..", "..", "deploy", "helm", "aegisops"), "--namespace", "aegisops", "--set", "image.tag=test", "--set", "credentials.existingSecret=app-kept", "--set", "database.existingSecret=mysql", "--set", "blobs.existingSecret=s3"}
	args = append(args, extra...)
	out, err := exec.Command(hcHelm(t), args...).CombinedOutput()
	if err != nil {
		return nil, out, err
	}
	var docs []hcDoc
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var doc hcDoc
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("invalid rendered YAML: %v", err)
		}
		if len(doc) != 0 {
			docs = append(docs, doc)
		}
	}
	return docs, out, nil
}

func hcMustRender(t *testing.T, extra ...string) []hcDoc {
	t.Helper()
	docs, out, err := hcRender(t, extra...)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	return docs
}

func hcAt(v any, path ...string) any {
	for _, p := range path {
		switch m := v.(type) {
		case hcDoc:
			v = m[p]
		case map[string]any:
			v = m[p]
		default:
			return nil
		}
	}
	return v
}

func hcFind(t *testing.T, docs []hcDoc, kind, name string) hcDoc {
	t.Helper()
	for _, doc := range docs {
		if doc["kind"] == kind && hcAt(doc, "metadata", "name") == name {
			return doc
		}
	}
	t.Fatalf("missing %s/%s", kind, name)
	return nil
}

func hcPod(doc hcDoc) map[string]any {
	return hcMap(hcAt(doc, "spec", "template", "spec"))
}

func hcMap(v any) map[string]any {
	switch m := v.(type) {
	case hcDoc:
		return map[string]any(m)
	case map[string]any:
		return m
	default:
		return nil
	}
}

func hcFirst(pod map[string]any, group string) map[string]any {
	items, _ := pod[group].([]any)
	if len(items) == 0 {
		return nil
	}
	return hcMap(items[0])
}

func hcEnv(c map[string]any, name string) any {
	items, _ := c["env"].([]any)
	for _, item := range items {
		m := hcMap(item)
		if m["name"] == name {
			return m
		}
	}
	return nil
}

func hcArgs(c map[string]any) string { return fmt.Sprint(c["args"]) }

func TestHelmProductionMigrationContract(t *testing.T) {
	docs := hcMustRender(t, "--set", "database.migrationSecret=mysql-ddl")
	server := hcFind(t, docs, "Deployment", "aegisops-server")
	pod := hcPod(server)
	init := hcFirst(pod, "initContainers")
	main := hcFirst(pod, "containers")
	job := hcFind(t, docs, "Job", "aegisops-migrate-1")
	migration := hcFirst(hcPod(job), "containers")
	if !strings.Contains(hcArgs(init), "db check --wait") || strings.Contains(hcArgs(init), "db migrate") {
		t.Fatalf("production init must only wait for schema: %v", init["args"])
	}
	if !strings.Contains(hcArgs(main), "--skip-migrations") || !strings.Contains(hcArgs(migration), "db migrate") {
		t.Fatalf("migration ownership wrong: main=%v job=%v", main["args"], migration["args"])
	}
	for name, c := range map[string]map[string]any{"init": init, "job": migration} {
		if hcAt(hcEnv(c, "AEGISOPS_PLATFORM_BLOBS_DRIVER"), "value") != "off" {
			t.Errorf("%s must disable blob validation without accessing S3 credentials", name)
		}
		if c["envFrom"] != nil {
			t.Errorf("DB-only %s must not receive blob Secret", name)
		}
	}
	for name, tc := range map[string]struct {
		c    map[string]any
		want string
	}{"init": {init, "mysql"}, "runtime": {main, "mysql"}, "job": {migration, "mysql-ddl"}} {
		ref := hcAt(hcEnv(tc.c, "AEGISOPS_PLATFORM_STORE_DSN"), "valueFrom", "secretKeyRef")
		if hcAt(ref, "name") != tc.want || hcAt(ref, "key") != "dsn" {
			t.Errorf("%s DSN reference: %v", name, ref)
		}
	}
	if hcAt(server, "spec", "strategy", "type") != "Recreate" || hcAt(server, "spec", "replicas") != 1 {
		t.Fatal("single-runtime backend requires single-replica Recreate")
	}
	if hcAt(job, "metadata", "annotations", "helm.sh/hook") != nil {
		t.Fatal("migration Job cannot assume normal resources exist from a pre-install hook")
	}
}

func TestHelmQuickstartAndExternalMigrationContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		initWords string
	}{
		{"quickstart", []string{"-f", filepath.Join("..", "..", "deploy", "helm", "aegisops", "values-quickstart.yaml"), "--set", "persistence.enabled=false"}, "db migrate"},
		{"external", []string{"--set", "migration.mode=external"}, "db check --wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := hcMustRender(t, tc.args...)
			for _, doc := range docs {
				if doc["kind"] == "Job" {
					t.Fatal("this mode must not create MySQL migration Job")
				}
			}
			pod := hcPod(hcFind(t, docs, "Deployment", "aegisops-server"))
			if !strings.Contains(hcArgs(hcFirst(pod, "initContainers")), tc.initWords) {
				t.Fatalf("wrong init args: %v", hcFirst(pod, "initContainers"))
			}
			if tc.name == "quickstart" {
				for _, v := range pod["volumes"].([]any) {
					volume := hcMap(v)
					if volume["name"] == "data" || volume["name"] == "reports" {
						if volume["emptyDir"] == nil || volume["persistentVolumeClaim"] != nil {
							t.Errorf("ephemeral experience volume: %v", volume)
						}
					}
				}
			}
		})
	}
}

func TestHelmStorageCapacityAndClaimsContract(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		data, reports string
	}{
		{"legacy", []string{"--set", "persistence.size=20Gi"}, "20Gi", "20Gi"},
		{"independent", []string{"--set", "persistence.size=20Gi", "--set", "persistence.dataSize=30Gi", "--set", "persistence.reportsSize=10Gi"}, "30Gi", "10Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docs := hcMustRender(t, tc.args...)
			for _, pair := range [][2]string{{"aegisops-data", tc.data}, {"aegisops-reports", tc.reports}} {
				pvc := hcFind(t, docs, "PersistentVolumeClaim", pair[0])
				if hcAt(pvc, "spec", "resources", "requests", "storage") != pair[1] {
					t.Errorf("%s capacity: %v", pair[0], pvc)
				}
				if hcAt(pvc, "metadata", "annotations", "helm.sh/resource-policy") != "keep" {
					t.Errorf("%s must survive app uninstall", pair[0])
				}
			}
		})
	}
	docs := hcMustRender(t, "--set", "persistence.existingReportsClaim=reports-kept")
	for _, doc := range docs {
		if doc["kind"] == "PersistentVolumeClaim" && hcAt(doc, "metadata", "name") == "aegisops-reports" {
			t.Fatal("bring-your-own reports claim must not be recreated")
		}
	}
	pod := hcPod(hcFind(t, docs, "Deployment", "aegisops-server"))
	found := false
	for _, item := range pod["volumes"].([]any) {
		v := hcMap(item)
		if v["name"] == "reports" {
			found = hcAt(v, "persistentVolumeClaim", "claimName") == "reports-kept"
		}
	}
	if !found {
		t.Fatal("reports volume must reference supplied claim")
	}
}

func TestHelmPrivateImagesIngressAndOIDCContract(t *testing.T) {
	for _, mode := range []string{"standard", "minimal"} {
		t.Run(mode, func(t *testing.T) {
			docs := hcMustRender(t, "--set", "mode="+mode, "--set", "imagePullSecrets[0].name=registry-key", "--set", "ingress.enabled=true", "--set", "ingress.host=ops.invalid", "--set", "server.publicURL=https://ops.invalid", "--set", "auth.oidcCASecret=idp-ca", "--set", "auth.oidcDisablePKCE=true", "--set", "auth.oidcAllowMissingNonce=true")
			for _, doc := range docs {
				if doc["kind"] != "Deployment" && doc["kind"] != "Job" {
					continue
				}
				secrets := hcPod(doc)["imagePullSecrets"].([]any)
				if len(secrets) != 1 || hcAt(secrets[0], "name") != "registry-key" {
					t.Errorf("missing pull Secret: %v", doc["metadata"])
				}
			}
			ingress := hcFind(t, docs, "Ingress", "aegisops")
			rules := hcAt(ingress, "spec", "rules").([]any)
			paths := hcAt(rules[0], "http", "paths").([]any)
			want := "aegisops-web"
			if mode == "minimal" {
				want = "aegisops-server"
			}
			if hcAt(paths[0], "backend", "service", "name") != want {
				t.Fatalf("wrong ingress backend: %v", paths)
			}
			pod := hcPod(hcFind(t, docs, "Deployment", "aegisops-server"))
			mounted := false
			for _, item := range hcFirst(pod, "containers")["volumeMounts"].([]any) {
				if hcAt(item, "name") == "oidc-ca" && hcAt(item, "mountPath") == "/etc/aegisops/oidc" && hcAt(item, "readOnly") == true {
					mounted = true
				}
			}
			if !mounted {
				t.Fatal("OIDC CA Secret must be mounted read-only")
			}
			body := hcAt(hcFind(t, docs, "ConfigMap", "aegisops-config"), "data", "config.yaml").(string)
			var cfg map[string]any
			if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
				t.Fatal(err)
			}
			if hcAt(cfg, "platform", "auth", "oidc_disable_pkce") != true || hcAt(cfg, "platform", "auth", "oidc_allow_missing_nonce") != true || hcAt(cfg, "platform", "auth", "oidc_ca_file") != "/etc/aegisops/oidc/ca.crt" {
				t.Fatalf("OIDC configuration mismatch: %v", hcAt(cfg, "platform", "auth"))
			}
		})
	}
}

func TestHelmRejectsUnsafeOrIncompleteProductionValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"mysql-secret", []string{"--set", "database.existingSecret="}},
		{"s3-secret", []string{"--set", "blobs.existingSecret="}},
		{"sqlite-ephemeral", []string{"--set", "database.driver=sqlite", "--set", "persistence.enabled=false"}},
		{"dir-ephemeral", []string{"--set", "blobs.driver=dir", "--set", "persistence.enabled=false"}},
		{"http-production", []string{"--set", "server.publicURL=http://ops.invalid"}},
		{"migration-mode", []string{"--set", "migration.mode=unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := hcRender(t, tc.args...)
			if err == nil {
				t.Fatalf("unsafe/incomplete values accepted: %s", out)
			}
		})
	}
}

func TestHelmDBOnlyConfigLoadsWithoutS3Credentials(t *testing.T) {
	docs := hcMustRender(t)
	body := hcAt(hcFind(t, docs, "ConfigMap", "aegisops-config"), "data", "config.yaml").(string)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AEGISOPS_PLATFORM_STORE_DSN", "app:test@tcp(db.invalid:3306)/aegisops?parseTime=true")
	t.Setenv("AEGISOPS_PLATFORM_BLOBS_DRIVER", "off")
	for _, k := range []string{"ENDPOINT", "ACCESS_KEY", "SECRET_KEY"} {
		t.Setenv("AEGISOPS_PLATFORM_BLOBS_"+k, "")
	}
	cfg, err := config.Load(config.LoadOptions{ConfigFile: path})
	if err != nil {
		t.Fatalf("DB-only commands must load chart config without S3 credentials: %v", err)
	}
	if cfg.Platform.Blobs.EffectiveDriver() != "off" || !cfg.Platform.Store.IsMySQL() {
		t.Fatalf("wrong DB-only configuration: %v", cfg.Platform.Blobs)
	}
	warnings, err := config.Diagnostics(path, config.ValidateModeServer)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("rendered DB-only config diagnostics: %v, %v", warnings, err)
	}
}

func TestHelmDefaultRBACAndContainerBoundaries(t *testing.T) {
	docs := hcMustRender(t)
	roleCount := 0
	for _, doc := range docs {
		if doc["kind"] == "ClusterRole" {
			roleCount++
			for _, item := range doc["rules"].([]any) {
				for _, verb := range hcAt(item, "verbs").([]any) {
					if verb != "get" && verb != "list" && verb != "watch" {
						t.Errorf("default role unexpectedly grants %v", verb)
					}
				}
			}
		}
		if doc["kind"] != "Deployment" && doc["kind"] != "Job" {
			continue
		}
		pod := hcPod(doc)
		if hcAt(pod, "securityContext", "runAsNonRoot") != true {
			t.Errorf("Pod must run nonroot: %v", doc["metadata"])
		}
		for _, group := range []string{"initContainers", "containers"} {
			containers, _ := pod[group].([]any)
			for _, item := range containers {
				if hcAt(item, "securityContext", "allowPrivilegeEscalation") != false || hcAt(item, "securityContext", "readOnlyRootFilesystem") != true {
					t.Errorf("container boundary missing: %v/%v", doc["metadata"], hcAt(item, "name"))
				}
			}
		}
	}
	if roleCount != 1 {
		t.Fatalf("default must grant exactly one read-only role, got %d", roleCount)
	}
}
