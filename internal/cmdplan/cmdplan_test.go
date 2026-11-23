package cmdplan

import (
	"testing"
)

// TestAnalyzeReadOnlyCombinations 锁定方案 24 §7.1 的自动执行集合。
func TestAnalyzeReadOnlyCombinations(t *testing.T) {
	cases := []struct {
		cmd         string
		commands    int
		firstStages int
	}{
		{"kubectl get po -A | grep mysql", 1, 2},
		{"kubectl get pods -n team-a | grep -E 'mysql|redis' | head -n 20", 1, 3},
		{"kubectl get pods -n team-a -o json | jq -r '.items[].metadata.name'", 1, 2},
		{"kubectl get pods -n team-a | sort | uniq -c | wc -l", 1, 4},
		{"kubectl get pods -n team-a -o json | jq '.items[] | {name: .metadata.name, ns: .metadata.namespace}' | head -20", 1, 3},
		{"kubectl get events -n team-a --sort-by=.lastTimestamp | tail -n 50", 1, 2},
		{"kubectl logs mypod -n team-a | grep -i error | head -n 100", 1, 3},
		{"kubectl get pods -n team-a -o name | cut -d/ -f2 | tr '-' '_'", 1, 3},
	}
	for _, tc := range cases {
		a := Analyze(tc.cmd)
		if !a.ProvablyReadOnly {
			t.Errorf("%q: 期望可证明只读，实际 reason=%q", tc.cmd, a.Reason)
			continue
		}
		if !a.NeedsShell {
			t.Errorf("%q: 含管道组合应标记 NeedsShell", tc.cmd)
		}
		if len(a.Program.Commands) != tc.commands {
			t.Errorf("%q: commands=%d, want %d", tc.cmd, len(a.Program.Commands), tc.commands)
			continue
		}
		if got := len(a.Program.Commands[0].Stages); got != tc.firstStages {
			t.Errorf("%q: stages=%d, want %d", tc.cmd, got, tc.firstStages)
		}
		for _, cmd := range a.Program.Commands {
			for _, st := range cmd.Stages {
				if st.Kind == StageKubectl && st.Argv[0] != "kubectl" {
					t.Errorf("%q: kubectl 段 argv[0]=%q", tc.cmd, st.Argv[0])
				}
			}
		}
	}
}

// TestAnalyzeSingleCommandStaysArgv 单条 kubectl 也只读，但不需要 shell。
func TestAnalyzeSingleCommandStaysArgv(t *testing.T) {
	a := Analyze("kubectl get pods -n team-a")
	if !a.ProvablyReadOnly || a.NeedsShell {
		t.Fatalf("单条只读命令应可证明只读且不需要 shell：%+v", a)
	}
	if len(a.Calls) != 1 || a.Calls[0].Namespace != "team-a" || a.Calls[0].Kind != "pods" {
		t.Fatalf("调用目标解析错误：%+v", a.Calls)
	}
}

// TestAnalyzeConnectors 校验 ; / && / || 的连接语义与目标提取。
func TestAnalyzeConnectors(t *testing.T) {
	cases := []struct {
		cmd string
		ops []Op
	}{
		{"kubectl get pods -n a; kubectl get deploy -n a", []Op{OpSeq, OpSeq}},
		{"kubectl get pods -n a && kubectl get deploy -n a", []Op{OpSeq, OpAnd}},
		{"kubectl get pods -n a || kubectl get deploy -n a", []Op{OpSeq, OpOr}},
		{"kubectl get pods -n a && kubectl get pods -n b || kubectl get pods -n c", []Op{OpSeq, OpAnd, OpOr}},
	}
	for _, tc := range cases {
		a := Analyze(tc.cmd)
		if !a.ProvablyReadOnly {
			t.Errorf("%q: 期望可证明只读，实际 %q", tc.cmd, a.Reason)
			continue
		}
		if len(a.Program.Commands) != len(tc.ops) {
			t.Errorf("%q: commands=%d, want %d", tc.cmd, len(a.Program.Commands), len(tc.ops))
			continue
		}
		for i, op := range tc.ops {
			if a.Program.Commands[i].Op != op {
				t.Errorf("%q: command[%d].Op=%v, want %v", tc.cmd, i, a.Program.Commands[i].Op, op)
			}
		}
	}
}

// TestAnalyzeRejectsUnsafe 校验所有不得自动执行的构造。
func TestAnalyzeRejectsUnsafe(t *testing.T) {
	cases := []string{
		// 写操作与第二条写命令
		"kubectl delete pod x -n a",
		"kubectl get pods -n a && kubectl delete pod x -n a",
		"kubectl get pods -n a | kubectl apply -f -",
		// 过滤器参数越界
		"kubectl get pods -n a | grep foo /etc/passwd",
		"kubectl get pods -n a | grep -f patterns",
		"kubectl get pods -n a | grep -r foo",
		"kubectl get pods -n a | sort -o out.txt",
		"kubectl get pods -n a | head -n 100000",
		"kubectl get pods -n a | tail -f",
		"kubectl get pods -n a | uniq input.txt",
		"kubectl get pods -n a | wc -c file.txt",
		"kubectl get pods -n a | cut -f1 /etc/passwd",
		// jq 文件读取、模块与额外输入
		"kubectl get pods -n a -o json | jq -f filter.jq",
		"kubectl get pods -n a -o json | jq --rawfile x /tmp/x '.'",
		"kubectl get pods -n a -o json | jq --slurpfile x /tmp/x '.'",
		"kubectl get pods -n a -o json | jq 'input'",
		"kubectl get pods -n a -o json | jq 'inputs'",
		"kubectl get pods -n a -o json | jq 'env'",
		"kubectl get pods -n a -o json | jq 'debug'",
		"kubectl get pods -n a -o json | jq 'include \"x\"'",
		"kubectl get pods -n a -o json | jq '\"\\(.a)\"'",
		"kubectl get pods -n a -o json | jq -r '.' file.json",
		// 未放行的过滤器
		"kubectl get pods -n a | awk '{print $1}'",
		"kubectl get pods -n a | sed -n '1p'",
		"kubectl get pods -n a | xargs rm",
		"kubectl get pods -n a | tee out.txt",
		"kubectl get pods -n a | curl http://example.com",
		// 重定向、命令替换、变量展开、通配符与 shell 包装器
		"kubectl get pods -n a > out.txt",
		"kubectl get pods -n a | grep $(id)",
		"kubectl get pods -n ${NS}",
		"kubectl get pods -n a | grep $HOME",
		"kubectl get pods -n a | grep *",
		"bash -c 'kubectl get pods -n a'",
		"kubectl get pods -n a | sh",
		"kubectl get pods -n a | { cat; }",
		"(kubectl get pods -n a)",
		"kubectl get pods -n a | grep foo &",
		"FOO=bar kubectl get pods -n a",
		// 目标/身份/凭据覆盖参数
		"kubectl --kubeconfig /tmp/other get pods -n a",
		"kubectl --server https://evil get pods -n a",
		"kubectl --token abc get pods -n a",
		"kubectl --as system:admin get pods -n a",
		"kubectl --context prod get pods -n a",
		"kubectl --raw /api/v1/namespaces/a/secrets",
		// 本地文件读取
		"kubectl get pods -n a -o go-template-file=/tmp/tpl",
		"kubectl get pods -n a -o custom-columns-file=/tmp/cols",
		// 交互式与流式
		"kubectl exec pod -n a -- sh",
		"kubectl get pods -n a -w",
		"kubectl logs pod -n a -f",
		// 未知参数
		"kubectl get pods -n a --watch-only",
		// 动词不支持的参数
		"kubectl describe pods -A",
		"kubectl logs -A pod",
	}
	for _, cmd := range cases {
		a := Analyze(cmd)
		if a.ProvablyReadOnly {
			t.Errorf("%q: 不应判定为可证明只读", cmd)
			continue
		}
		if a.Reason == "" {
			t.Errorf("%q: 必须给出不可自动执行的原因", cmd)
		}
		if len(a.Program.Commands) != 0 {
			t.Errorf("%q: 拒绝时不应产生执行计划", cmd)
		}
	}
}

// TestAnalyzeExtractsCalls 校验每条调用的目标与动作。
func TestAnalyzeExtractsCalls(t *testing.T) {
	a := Analyze("kubectl get pods -n team-a | grep mysql && kubectl logs web-0 -n team-a | grep err")
	if !a.ProvablyReadOnly {
		t.Fatalf("期望可证明只读：%q", a.Reason)
	}
	if len(a.Calls) != 2 {
		t.Fatalf("calls=%d, want 2: %+v", len(a.Calls), a.Calls)
	}
	if a.Calls[0].Kind != "pods" || a.Calls[0].Namespace != "team-a" || a.Calls[0].Action != ActionRead {
		t.Errorf("first call: %+v", a.Calls[0])
	}
	if a.Calls[1].Kind != "pod" || a.Calls[1].Name != "web-0" || a.Calls[1].Action != ActionLogs {
		t.Errorf("second call: %+v", a.Calls[1])
	}

	a = Analyze("kubectl get deploy/api -A")
	if !a.ProvablyReadOnly {
		t.Fatalf("期望可证明只读：%q", a.Reason)
	}
	if !a.Calls[0].AllNamespaces || a.Calls[0].Kind != "deploy" || a.Calls[0].Name != "api" {
		t.Errorf("all-namespaces call: %+v", a.Calls[0])
	}

	a = Analyze("kubectl rollout status deployment/api -n team-a")
	if !a.ProvablyReadOnly || a.Calls[0].Subcommand != "status" || a.Calls[0].Kind != "deployment" {
		t.Fatalf("rollout status: %+v (reason=%q)", a.Calls, a.Reason)
	}
	if a.Calls[0].Action != ActionRead {
		t.Errorf("rollout status action=%q", a.Calls[0].Action)
	}
}

// TestAnalyzeJqSubset 单独覆盖 jq 表达式子集。
func TestAnalyzeJqSubset(t *testing.T) {
	allowed := [][]string{
		{"-r", ".items[].metadata.name"},
		{`.[] | select(.status.phase == "Running") | .metadata.name`},
		{".items | length"},
		{"map(.metadata.name) | sort | unique"},
		{"{n: .metadata.name, ns: .metadata.namespace}"},
		{".items[] | .spec.containers[0].image"},
	}
	for _, args := range allowed {
		if err := validateJq(args); err != nil {
			t.Errorf("jq %v 应被接受：%v", args, err)
		}
	}
	rejected := [][]string{
		{"-f", "x.jq"},
		{"--rawfile", "a", "/tmp/b", "."},
		{".a | input"},
		{".a | inputs"},
		{"$ENV"},
		{"$__loc__"},
		{"env"},
		{"halt_error"},
		{`"\(.a)"`},
		{".a; .b"},
		{"import \"x\" as y; y"},
		{".a", "file.json"},
		{"--args", "x"},
	}
	for _, args := range rejected {
		if err := validateJq(args); err == nil {
			t.Errorf("jq %v 应被拒绝", args)
		}
	}
}
