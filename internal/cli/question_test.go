package cli

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
)

func TestNumberedDecision(t *testing.T) {
	for _, test := range []struct{ input, status string }{{"\n9\n1\n", "selected"}, {"f\n自定义\n", "text"}, {"0\n", "skipped"}} {
		var out bytes.Buffer
		in := &bufInput{r: bufio.NewReader(strings.NewReader(test.input)), w: &out}
		q := decision.Question{ID: "q", SessionID: "s", TurnID: "t", Question: "请选择", Options: []decision.Option{{ID: "a", Label: "A", Impact: "影响", Recommended: true}}, ExpiresAt: time.Now().Add(time.Minute)}
		a, err := askDecision(in, &out)(context.Background(), q)
		if err != nil || a.Status != test.status {
			t.Fatalf("%+v %v", a, err)
		}
	}
}
