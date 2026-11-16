package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamRedactorEverySplit(t *testing.T) {
	for _, input := range []string{
		`{"password":"hunter2"}`, "password: hunter2\n", "Authorization: Bearer hunter2\n",
		"client-key-data: hunter2\n", "api-key: hunter2\n",
	} {
		for split := 1; split < len(input); split++ {
			r := newStreamRedactor()
			first, err := r.Push(input[:split])
			if err != nil {
				t.Fatal(err)
			}
			second, err := r.Push(input[split:])
			if err != nil {
				t.Fatal(err)
			}
			got := first + second + r.Flush()
			if strings.Contains(got, "hunter2") || strings.Contains(got, "ter2") {
				t.Fatalf("split=%d input=%q leaked=%q", split, input, got)
			}
		}
	}
}

// TestStreamRedactorCrossChunkSecret 锁定跨分片敏感格式：一个密钥被拆成
// 多个 chunk 时，任何一次释放都不得出现完整明文。
func TestStreamRedactorCrossChunkSecret(t *testing.T) {
	r := newStreamRedactor()
	chunks := []string{"这里是配置：\nsecretKey: sk-", "live-abcdefghijklm", "nopqrstuvwxyz\n结束"}
	var released strings.Builder
	for _, chunk := range chunks {
		safe, err := r.Push(chunk)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(safe, "sk-live-abcdefghijklmnopqrstuvwxyz") {
			t.Fatalf("跨分片密钥被明文释放: %q", safe)
		}
		released.WriteString(safe)
	}
	tail := r.Flush()
	if strings.Contains(released.String()+tail, "live-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("最终输出仍含明文: %q", released.String()+tail)
	}
	if !strings.Contains(released.String()+tail, "[REDACTED]") {
		t.Fatalf("敏感值应被替换为 REDACTED: %q", released.String()+tail)
	}
}

func TestStreamRedactorReleasesNormalTextImmediately(t *testing.T) {
	r := newStreamRedactor()
	safe, err := r.Push("已确认")
	if err != nil {
		t.Fatal(err)
	}
	if safe != "已确认" {
		t.Fatalf("普通正文应立即释放以保证真实增量，实际 %q", safe)
	}
	if got := r.Flush(); got != "" {
		t.Fatalf("flush 应无剩余文本: %q", got)
	}
}

func TestStreamRedactorHoldsSplitKeyword(t *testing.T) {
	r := newStreamRedactor()
	safe, err := r.Push("配置 pass")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(safe, "pass") {
		t.Fatalf("关键字前缀不应被提前释放: %q", safe)
	}
	safe2, err := r.Push("word: hunter2\n后续")
	if err != nil {
		t.Fatal(err)
	}
	combined := safe + safe2 + r.Flush()
	if strings.Contains(combined, "hunter2") {
		t.Fatalf("跨分片键值未脱敏: %q", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("应保留脱敏标记: %q", combined)
	}
}

func TestRedactTextFormats(t *testing.T) {
	cases := []struct{ in, wantAbsent string }{
		{`{"password":"hunter2","name":"x"}`, "hunter2"},
		{`token: abcdefghijklmnop`, "abcdefghijklmnop"},
		{`Authorization: Bearer abcdefghijklmnop`, "abcdefghijklmnop"},
		{`api_key = abcd1234`, "abcd1234"},
		{`-----BEGIN RSA PRIVATE KEY-----`, "BEGIN RSA PRIVATE KEY"},
	}
	for _, tc := range cases {
		got := redactText(tc.in)
		if strings.Contains(got, tc.wantAbsent) {
			t.Errorf("redactText(%q)=%q 仍含敏感值 %q", tc.in, got, tc.wantAbsent)
		}
	}
	if got := redactText("普通诊断结论，没有敏感数据"); got != "普通诊断结论，没有敏感数据" {
		t.Errorf("普通文本被误伤: %q", got)
	}
}

func TestRedactValueNested(t *testing.T) {
	value := map[string]any{
		"password": "hunter2",
		"nested":   map[string]any{"token": "abcdefghijklmnop"},
		"items":    []any{map[string]any{"secretKey": "xyz123"}},
	}
	out, ok := redactValue(value).(map[string]any)
	if !ok {
		t.Fatal("redactValue 未返回结构化对象")
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "abcdefghijklmnop", "xyz123"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("嵌套敏感值未脱敏: %s", raw)
		}
	}
}
