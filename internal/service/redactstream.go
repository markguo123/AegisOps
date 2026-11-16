package service

import (
	"fmt"
	"regexp"
	"strings"
)

// 有状态流式脱敏（方案 25 §11）：不能对每个 chunk 单独正则替换就宣称完成，
// 必须覆盖跨分片的敏感格式。实现保留一段未决尾部（可能被后续分片补齐的前缀），
// 只在能判定边界时释放前缀；无法在阈值内判定时返回错误，由上层停止输出而不是
// 直接释放原文。
const (
	// streamRedactMaxPending 是允许缓冲的上限；超过即安全失败（不新增无限缓冲）。
	streamRedactMaxPending = 256 * 1024
)

type redactRule struct {
	re   *regexp.Regexp
	repl string
}

var streamSecretRules = []redactRule{
	// JSON 形态的敏感键（"secretKey":"..." / "token": "..."）。
	{regexp.MustCompile(`(?i)"([a-z0-9_]*(?:password|passwd|secret|token|api[_-]?key|authorization)[a-z0-9_]*)"\s*:\s*("[^"\n]*"|[^,}\s]+)`), `"$1":"[REDACTED]"`},
	// HTTP Authorization 头：连 scheme 后的凭据一起替换。
	{regexp.MustCompile(`(?i)(authorization\s*:\s*)(bearer|basic)?\s*\S+`), `${1}${2} [REDACTED]`},
	// key=value / key: value 形态（含引号包裹的值）。
	{regexp.MustCompile(`(?i)(password|passwd|secret[a-z_]*|token|api[_-]?key)(\s*[:=]\s*)("[^"\n]*"|'[^'\n]*'|\S+)`), `${1}${2}[REDACTED]`},
	// 私钥块（完整块或未闭合时一直遮蔽到结尾）。
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), `[REDACTED]`},
	// 常见云厂商/模型 API Key 形态。
	{regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`), `[REDACTED]`},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), `[REDACTED]`},
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), `[REDACTED]`},
	// JWT。
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), `[REDACTED]`},
	// kubeconfig 私钥/证书数据。
	{regexp.MustCompile(`(?i)(client-key-data|client-certificate-data)(\s*:\s*)\S+`), `${1}${2}[REDACTED]`},
}

// redactText 对完整文本执行通用敏感格式脱敏（SSE、快照、CLI 共用）。
func redactText(text string) string {
	if text == "" {
		return text
	}
	out := text
	for _, rule := range streamSecretRules {
		out = rule.re.ReplaceAllString(out, rule.repl)
	}
	return out
}

// streamRedactor 是有状态流式脱敏器：Push 返回本次可安全释放的文本，
// Flush 返回剩余可安全释放的文本（消息结束时调用）。
//
// 普通文本立即释放。敏感字段从关键字起保留到行边界，私钥保留到块结束，
// 同时保护包括单字符在内的关键字前缀，避免未来分片补齐后泄漏。
type streamRedactor struct {
	pending strings.Builder
}

func newStreamRedactor() *streamRedactor { return &streamRedactor{} }

// openSecretRe 匹配“已开始但可能未结束”的敏感字段/凭据前缀。
var secretKeywords = []string{
	"password", "passwd", "secret", "client-secret", "token", "api_key", "apikey",
	"authorization", "bearer", "client-key-data", "client-certificate-data",
	"api-key", "sk-", "akia", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "eyj", "-----begin",
}

func safeStreamCut(s string) int {
	lower := strings.ToLower(s)
	cut := len(s)
	// An incomplete private-key block can span lines.
	if start := strings.Index(lower, "-----begin"); start >= 0 {
		if !strings.Contains(lower[start:], "-----end ") || !strings.Contains(lower[start:], "private key-----\n") {
			cut = start
		}
	}
	lineStart := strings.LastIndexByte(s, '\n') + 1
	tail := lower[lineStart:]
	for _, keyword := range secretKeywords {
		if pos := strings.Index(tail, keyword); pos >= 0 && lineStart+pos < cut {
			cut = lineStart + pos
		}
		limit := len(keyword) - 1
		if limit > len(lower) {
			limit = len(lower)
		}
		for n := limit; n >= 1; n-- {
			if strings.HasSuffix(lower, keyword[:n]) {
				if start := len(s) - n; start < cut {
					cut = start
				}
				break
			}
		}
	}
	// Keep the opening quote with a JSON key so complete-text redaction retains context.
	if cut > 0 && s[cut-1] == '"' {
		cut--
	}
	return cut
}

// Push 追加一段模型增量，返回现在可以安全释放的部分（可能为空）。
func (r *streamRedactor) Push(text string) (string, error) {
	if text == "" {
		return "", nil
	}
	r.pending.WriteString(text)
	raw := r.pending.String()
	if len(raw) > streamRedactMaxPending {
		return "", fmt.Errorf("stream redaction exceeded %d bytes without a safe boundary", streamRedactMaxPending)
	}
	cut := safeStreamCut(raw)
	if cut <= 0 {
		return "", nil
	}
	safe, rest := raw[:cut], raw[cut:]
	r.pending.Reset()
	r.pending.WriteString(rest)
	return redactText(safe), nil
}

// Flush 在消息结束时释放全部剩余文本（整块可见，可安全脱敏）。
func (r *streamRedactor) Flush() string {
	raw := r.pending.String()
	r.pending.Reset()
	return redactText(raw)
}

// PendingLen 返回尚未释放的字节数（测试与诊断用）。
func (r *streamRedactor) PendingLen() int { return r.pending.Len() }
