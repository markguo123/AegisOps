package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
)

func askDecision(in chatInput, w io.Writer) decision.Asker {
	return func(ctx context.Context, q decision.Question) (decision.Answer, error) {
		p, err := decision.New(q)
		if err != nil {
			return decision.Answer{}, err
		}
		defer p.Close()
		fmt.Fprintln(w, "\n"+q.Question)
		for i, o := range q.Options {
			recommended := ""
			if o.Recommended {
				recommended = "（推荐）"
			}
			fmt.Fprintf(w, "%d. %s%s\n   %s\n", i+1, o.Label, recommended, o.Impact)
		}
		fmt.Fprintln(w, "0. 跳过\nf. 自由输入（不要填写密码或 Secret）")
		for {
			if err := ctx.Err(); err != nil {
				return decision.Answer{}, err
			}
			in.SetPrompt("选择: ")
			line, err := decisionLine(ctx, in)
			if err != nil {
				return decision.Answer{}, err
			}
			a := decision.Answer{ID: q.ID, SessionID: q.SessionID, TurnID: q.TurnID}
			line = strings.TrimSpace(line)
			if line == "0" {
				a.Status = "skipped"
			} else if line == "f" {
				in.SetPrompt("请输入: ")
				a.Text, err = decisionLine(ctx, in)
				if err != nil {
					return decision.Answer{}, err
				}
				a.Status = "text"
			} else if n, e := strconv.Atoi(line); e == nil && n > 0 && n <= len(q.Options) {
				a.Status = "selected"
				a.OptionID = q.Options[n-1].ID
			}
			if err := ctx.Err(); err != nil {
				return decision.Answer{}, err
			}
			if err := p.Submit(a, time.Now()); err == nil {
				return a, nil
			} else if err == decision.ErrStale {
				return decision.Answer{}, err
			}
			fmt.Fprintln(w, "输入无效，请选择编号或输入 f。")
		}
	}
}

func terminalDecision(source io.Reader, in chatInput, w io.Writer) decision.Asker {
	f, ok := source.(*os.File)
	if !ok || !isTerminal(f) {
		return nil
	}
	return askDecision(in, w)
}

func decisionLine(ctx context.Context, in chatInput) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() { text, err := in.Readline(); ch <- result{text, err} }()
	select {
	case r := <-ch:
		return r.text, r.err
	case <-ctx.Done():
		_ = in.Close()
		return "", ctx.Err()
	}
}
