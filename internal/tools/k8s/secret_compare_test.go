package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func TestSafeConfigDataPreservesOnlyReferenceMetadata(t *testing.T) {
	data, refs := safeConfigData(map[string]string{
		"mail.properties": "mail.password=${MAIL_PASSWORD:-fallback-fixture}\nmail.token=literal-fixture",
	})
	result, _ := tools.MarshalForLLM(tools.ToolResult{Data: map[string]any{"data": data, "references": refs}}, 0)
	if strings.Contains(result, "fallback-fixture") || strings.Contains(result, "literal-fixture") ||
		!strings.Contains(result, "MAIL_PASSWORD") || !strings.Contains(result, "mail.password") {
		t.Fatalf("unexpected configuration projection: %s", result)
	}
}

type comparisonReader struct {
	Reader
	pod    *corev1.Pod
	config *corev1.ConfigMap
}

func (r comparisonReader) GetPod(context.Context, string, string) (*corev1.Pod, error) {
	return r.pod, nil
}

func (r comparisonReader) GetConfigMap(context.Context, string, string) (*corev1.ConfigMap, error) {
	return r.config, nil
}

func TestSecretCompareBindsConfigurationVersionAndHidesValues(t *testing.T) {
	const fixture = "test-credential-value"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "10"}, Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app", Env: []corev1.EnvVar{
			{Name: "A", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "mail"}, Key: "password"}}},
			{Name: "B", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "password"}}},
		}}},
	}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "20"}, Data: map[string]string{"password": fixture}}
	reader := &secretValueReaderStub{values: map[string][]byte{"password": []byte(fixture)}}
	tool := &secretValueTool{reader: reader, config: comparisonReader{pod: pod, config: cm}}
	args := json.RawMessage(`{"namespace":"team","name":"mail","keys":["password"],"reason":"verify config","operation":"compare_config","pod":"worker","container":"app","configmap":"settings","config_key":"password","pod_version":"10","config_version":"20"}`)
	facts, err := tool.OperationFacts(args)
	if err != nil || len(facts.Targets) != 3 {
		t.Fatalf("comparison did not declare all targets: %+v %v", facts, err)
	}
	result, err := tool.Execute(context.Background(), args)
	output, _ := tools.MarshalForLLM(result, 0)
	if err != nil || result.ModelOnly != nil || !strings.Contains(output, `"status":"equal"`) || strings.Contains(output, fixture) {
		t.Fatalf("invalid comparison result: %s (%v)", output, err)
	}
	cm.ResourceVersion = "21"
	reader.keys = nil
	result, err = tool.Execute(context.Background(), args)
	output, _ = tools.MarshalForLLM(result, 0)
	if err != nil || len(reader.keys) != 0 || !strings.Contains(output, `"status":"unknown"`) {
		t.Fatalf("stale comparison reached Secret read: %s (%v)", output, err)
	}
}
