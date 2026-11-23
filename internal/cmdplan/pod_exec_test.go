package cmdplan

import "testing"

func TestPodExecDiagnosticRules(t *testing.T) {
	good := [][]string{{"ls", "-l", "/app"}, {"stat", "/app"}, {"df", "-h"}, {"du", "-s", "/app"}, {"ps"}, {"ss", "-lnt"}, {"netstat", "-lnu"}, {"hostname"}}
	for _, a := range good {
		if _, err := NormalizePodExec(PodExecRequest{Namespace: "team", Pod: "api-0", Argv: a}); err != nil {
			t.Errorf("%q: %v", a, err)
		}
	}
	bad := [][]string{{"cat", "/app/config"}, {"env"}, {"/bin/ls", "/app"}, {"busybox", "ls", "/app"}, {"ps", "aux"}, {"ps", "e"}, {"ps", "-e", "-o", "args"}, {"ls", "-R", "/app"}, {"ls", "/app", "/etc"}, {"ls", "/app/../etc"}, {"ls", "/proc"}, {"ls", "/var/run/secrets/kubernetes.io"}, {"stat", "/app/.aws"}, {"ss", "-K"}, {"sh", "-c", "id"}, {"ls", "/app\n"}, {"ls", "$(id)"}, {"df", "-h", "-k"}, {"du", "/app"}}
	for _, a := range bad {
		if _, err := NormalizePodExec(PodExecRequest{Namespace: "team", Pod: "api-0", Argv: a}); err == nil {
			t.Errorf("accepted %q", a)
		}
	}
}
func TestPodExecStrictJSON(t *testing.T) {
	for _, raw := range []string{`{"namespace":"team","pod":"api","argv":["id"],"approved":true}`, `{"namespace":"*","pod":"api","argv":["id"]}`, `{"namespace":"team","pod":"api","argv":["id"],"timeout_seconds":31}`, `{"namespace":"team","pod":"api","argv":["id"]} {}`} {
		if _, err := DecodePodExec([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}
func TestPodExecCompatibility(t *testing.T) {
	for _, command := range []string{"kubectl exec api-0 -n team -c app -- ls -l /app", "kubectl -n team exec pod/api-0 -c app -- ls -l /app"} {
		r, m, err := ParsePodExec(command)
		if !m || err != nil || r.Namespace != "team" || r.Pod != "api-0" || r.Container != "app" {
			t.Errorf("%s: %+v %v %v", command, r, m, err)
		}
	}
	for _, command := range []string{"kubectl exec pods/pod/api -n team -- id", "kubectl exec api -n team --namespace other -- id", "kubectl exec api -n team -it -- id", "kubectl exec api -n team -- sh -c id", "kubectl exec api -n team --kubeconfig other -- id", "kubectl exec deploy/api -n team -- id"} {
		_, m, err := ParsePodExec(command)
		if !m || err == nil {
			t.Errorf("accepted %s: matched=%v err=%v", command, m, err)
		}
	}
	if _, m, _ := ParsePodExec("kubectl get pods exec -n team"); m {
		t.Fatal("Pod named exec was mistaken for a subcommand")
	}
}
func TestScopedAuthCanI(t *testing.T) {
	for _, s := range []string{"kubectl auth can-i get pods -n team", "kubectl auth can-i create deployments --namespace=team"} {
		if !Analyze(s).ProvablyReadOnly {
			t.Errorf("not accepted: %s", s)
		}
	}
	for _, s := range []string{"kubectl auth can-i --list", "kubectl auth can-i get pods --as=admin -n team", "kubectl auth can-i get pods -n team -n other", "kubectl auth can-i get pods"} {
		if Analyze(s).ProvablyReadOnly {
			t.Errorf("accepted: %s", s)
		}
	}
}
