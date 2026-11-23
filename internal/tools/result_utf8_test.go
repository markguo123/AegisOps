package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestResultTruncationPreservesUTF8AndByteBudget(t *testing.T) {
	for _, limit := range []int{1, 3, 8, 20, 31, 128} {
		text := strings.Repeat("中文日志🙂", 50)
		if !truncate(&text, limit) || !utf8.ValidString(text) || len(text) > limit {
			t.Fatalf("invalid UTF-8 or byte budget at %d: %q", limit, text)
		}
	}
}
