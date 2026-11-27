package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func execFixture(t *testing.T) (*Runner, *fake.Clientset, cmdplan.PodExecRequest, *int) {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "team", UID: types.UID("uid-1")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "container://1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	cs := fake.NewSimpleClientset(pod)
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authorizationv1.SelfSubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}, nil
	})
	r := NewRunner(Options{Clientset: cs, RESTConfig: &rest.Config{Host: "https://unused.invalid"}})
	count := new(int)
	r.podStream = func(_ context.Context, _ cmdplan.PodExecRequest, argv []string, out, _ io.Writer) error {
		*count++
		if argv[0] == "readlink" {
			_, _ = io.WriteString(out, argv[2])
		} else {
			_, _ = io.WriteString(out, "diagnostic-ok")
		}
		return nil
	}
	return r, cs, cmdplan.PodExecRequest{Namespace: "team", Pod: "api", Argv: []string{"id"}}, count
}

func TestPodExecTransportIsNonInteractiveAndRejectsRedirects(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/api/v1/namespaces/team/pods/api/exec" {
			t.Errorf("wrong target: %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("container") != "app" || q.Get("stdin") == "true" || q.Get("tty") == "true" || q.Get("command") != "id" {
			t.Errorf("unsafe exec query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Location", "/other")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	rc := &rest.Config{Host: server.URL}
	cs, e := kubernetes.NewForConfig(rc)
	if e != nil {
		t.Fatal(e)
	}
	r := NewRunner(Options{Clientset: cs, RESTConfig: rc})
	_, _, _, e = r.streamPodExec(context.Background(), cmdplan.PodExecRequest{Namespace: "team", Pod: "api", Container: "app"}, []string{"id"}, 64)
	if e == nil || calls != 1 {
		t.Fatalf("redirect followed or accepted: calls=%d err=%v", calls, e)
	}
}
func approvedFixture(t *testing.T, r *Runner, req cmdplan.PodExecRequest) context.Context {
	t.Helper()
	ctx, e := r.PreparePodExec(context.Background(), req, "u", "s")
	if e != nil {
		t.Fatal(e)
	}
	ctx, e = r.ApprovePodExec(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}
func TestPodExecRequiresCurrentSingleUsePermit(t *testing.T) {
	r, _, req, count := execFixture(t)
	if _, e := r.ExecPod(context.Background(), req); e == nil {
		t.Fatal("direct execute bypassed approval")
	}
	prepared, e := r.PreparePodExec(context.Background(), req, "u", "s")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.ExecPod(prepared, req); e != nil {
		t.Fatalf("prepared low-risk read failed: %v", e)
	}
	ctx := approvedFixture(t, r, req)
	res, e := r.ExecPod(ctx, req)
	if e != nil || res.Stdout != "diagnostic-ok" {
		t.Fatalf("%+v %v", res, e)
	}
	if _, e = r.ExecPod(ctx, req); e == nil {
		t.Fatal("permit replay succeeded")
	}
	if *count != 2 {
		t.Fatalf("remote calls=%d", *count)
	}
}
func TestPodExecRejectsMutationAfterApproval(t *testing.T) {
	for _, kind := range []string{"argv", "pod", "restart", "credential"} {
		t.Run(kind, func(t *testing.T) {
			r, cs, req, count := execFixture(t)
			ctx := approvedFixture(t, r, req)
			switch kind {
			case "argv":
				req.Argv = []string{"hostname"}
			case "pod", "restart":
				p, _ := cs.CoreV1().Pods("team").Get(ctx, "api", metav1.GetOptions{})
				if kind == "pod" {
					p.UID = "new"
				} else {
					p.Status.ContainerStatuses[0].RestartCount++
				}
				_, _ = cs.CoreV1().Pods("team").Update(ctx, p, metav1.UpdateOptions{})
			case "credential":
				r.targetCheck = func(context.Context) error { return fmt.Errorf("SCOPE_DENIED: revoked") }
			}
			if _, e := r.ExecPod(ctx, req); e == nil {
				t.Fatal("changed plan executed")
			}
			if *count != 0 {
				t.Fatal("remote stream started")
			}
		})
	}
}
func TestPodExecPathsAndSensitiveMounts(t *testing.T) {
	for _, kind := range []string{"allowed", "sensitive-literal", "sensitive-symlink", "no-readlink", "privileged", "hostnetwork"} {
		t.Run(kind, func(t *testing.T) {
			r, cs, req, count := execFixture(t)
			req.Argv = []string{"ls", "/app"}
			p, _ := cs.CoreV1().Pods("team").Get(context.Background(), "api", metav1.GetOptions{})
			switch kind {
			case "sensitive-literal":
				p.Spec.Volumes = []corev1.Volume{{Name: "secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "db"}}}}
				p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "secret", MountPath: "/app"}}
			case "privileged":
				yes := true
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes}
			case "hostnetwork":
				p.Spec.HostNetwork = true
			}
			_, _ = cs.CoreV1().Pods("team").Update(context.Background(), p, metav1.UpdateOptions{})
			ctx, e := r.PreparePodExec(context.Background(), req, "u", "s")
			if kind == "sensitive-literal" || kind == "privileged" || kind == "hostnetwork" {
				if e == nil {
					t.Fatal("unsafe Pod admitted")
				}
				if *count != 0 {
					t.Fatal("preapproval remote call")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			ctx, e = r.ApprovePodExec(ctx)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "sensitive-symlink" {
				r.podStream = func(_ context.Context, _ cmdplan.PodExecRequest, _ []string, out, _ io.Writer) error {
					*count++
					_, _ = io.WriteString(out, "/var/run/secrets/token")
					return nil
				}
			}
			if kind == "no-readlink" {
				r.podStream = func(context.Context, cmdplan.PodExecRequest, []string, io.Writer, io.Writer) error {
					*count++
					return fmt.Errorf("binary missing")
				}
			}
			_, e = r.ExecPod(ctx, req)
			if kind == "allowed" {
				if e != nil || *count != 2 {
					t.Fatalf("count=%d err=%v", *count, e)
				}
			} else {
				if e == nil || *count != 1 {
					t.Fatalf("unsafe traversal: %d %v", *count, e)
				}
			}
		})
	}
}
func TestPodExecLimitsAndCancellation(t *testing.T) {
	r, _, req, _ := execFixture(t)
	ctx := approvedFixture(t, r, req)
	r.execPod.MaxOutputBytes = 64
	r.podStream = func(_ context.Context, _ cmdplan.PodExecRequest, _ []string, out, stderr io.Writer) error {
		_, _ = io.WriteString(out, strings.Repeat("a", 50))
		_, _ = io.WriteString(stderr, strings.Repeat("b", 50))
		return nil
	}
	res, e := r.ExecPod(ctx, req)
	if e != nil || !res.Truncated || len(res.Stdout)+len(res.Stderr) > 64+len("\n[TRUNCATED]") {
		t.Fatalf("%+v %v", res, e)
	}
	ctx = approvedFixture(t, r, req)
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, e = r.ExecPod(cancelCtx, req)
	if e == nil {
		t.Fatal("cancelled execution succeeded")
	}
}
func TestPodExecConcurrency(t *testing.T) {
	r, _, _, _ := execFixture(t)
	p := &podExecPermit{user: "limit-u", session: "limit-s"}
	release, e := r.acquirePodExec(p)
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	if _, e = r.acquirePodExec(p); e == nil {
		t.Fatal("same session ran concurrent exec")
	}
}
func TestKubectlExecUsesTheSamePermit(t *testing.T) {
	r, _, req, count := execFixture(t)
	ctx := approvedFixture(t, r, req)
	res, e := r.RunKubectl(ctx, "kubectl exec api -n team -- id")
	if e != nil || res.Stdout != "diagnostic-ok" || *count != 1 {
		t.Fatalf("%+v %v calls=%d", res, e, *count)
	}
}

func TestExecPodDisabledAndDenyOpsCoverBothEntrypoints(t *testing.T) {
	for _, kind := range []string{"disabled", "deny"} {
		r, _, req, count := execFixture(t)
		if kind == "disabled" {
			r.DisablePodExec()
		} else {
			r.denyOps = []string{"exec"}
		}
		if _, e := r.PreparePodExec(context.Background(), req, "u", "s"); e == nil {
			t.Fatal("prepared a disabled/denied exec")
		}
		if _, e := r.RunKubectl(context.Background(), "kubectl exec api -n team -- id"); e == nil {
			t.Fatal("kubectl bypassed switch")
		}
		if *count != 0 {
			t.Fatal("remote call")
		}
	}
}

func TestDuRejectsSensitiveDescendant(t *testing.T) {
	r, cs, req, count := execFixture(t)
	p, _ := cs.CoreV1().Pods("team").Get(context.Background(), "api", metav1.GetOptions{})
	p.Spec.Volumes = []corev1.Volume{{Name: "db", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "db"}}}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "db", MountPath: "/app/credentials"}}
	_, _ = cs.CoreV1().Pods("team").Update(context.Background(), p, metav1.UpdateOptions{})
	req.Argv = []string{"du", "-s", "/app"}
	if _, e := r.PreparePodExec(context.Background(), req, "u", "s"); e == nil {
		t.Fatal("recursive diagnostic traversed Secret mount")
	}
	if *count != 0 {
		t.Fatal("preapproval remote call")
	}
}

func TestBoundEnvDoesNotInheritCloudCredentials(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-other-cluster")
	r := NewRunner(Options{TargetBound: true, Kubeconfig: "bound"})
	for _, entry := range r.childEnv() {
		if strings.HasPrefix(entry, "AWS_SECRET_ACCESS_KEY=") {
			t.Fatal("cross-target cloud credential inherited")
		}
	}
}

func TestReviewedPodExecStillRequiresHumanPermit(t *testing.T) {
	runner, _, req, count := execFixture(t)
	req.Argv = []string{"ls", "-R", "/app"}
	ctx, err := runner.PreparePodExec(context.Background(), req, "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ExecPod(ctx, req); err == nil {
		t.Fatal("reviewed diagnostic executed without consent")
	}
	if *count != 0 {
		t.Fatal("remote execution started before consent")
	}
	ctx, err = runner.ApprovePodExec(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ExecPod(ctx, req); err != nil {
		t.Fatal(err)
	}
}
