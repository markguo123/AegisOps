package decision

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPersistAnswerBeforeWakeAndRetryFailure(t *testing.T) {
	p, a, now := fixture(t)
	want := errors.New("database unavailable")
	if err := p.SubmitAfter(a, now, func() error { return want }); err != want {
		t.Fatalf("error=%v", err)
	}
	select {
	case <-p.Answers():
		t.Fatal("failed commit woke the Agent")
	default:
	}
	if err := p.SubmitAfter(a, now, func() error {
		select {
		case <-p.Answers():
			t.Fatal("answer visible before commit")
		default:
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-p.Answers(); got != a {
		t.Fatalf("answer=%+v", got)
	}
}

func fixture(t *testing.T) (*Pending, Answer, time.Time) {
	t.Helper()
	now := time.Now()
	p, err := New(Question{ID: "q", SessionID: "s", TurnID: "t", Question: "请选择存储", Options: []Option{{ID: "a", Label: "标准", Impact: "一般负载", Recommended: true}}, ExpiresAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return p, Answer{ID: "q", SessionID: "s", TurnID: "t", Status: "selected", OptionID: "a"}, now
}

func TestAnswerSemantics(t *testing.T) {
	for _, status := range []string{"selected", "text", "skipped"} {
		t.Run(status, func(t *testing.T) {
			p, a, now := fixture(t)
			select {
			case <-p.Answers():
				t.Fatal("recommendation automatically answered")
			default:
			}
			a.Status = status
			if status != "selected" {
				a.OptionID = ""
			}
			if status == "text" {
				a.Text = "先检查容量"
			}
			if err := p.Submit(a, now); err != nil {
				t.Fatal(err)
			}
			if got := <-p.Answers(); got != a {
				t.Fatalf("got %+v", got)
			}
			if err := p.Submit(a, now); err != ErrStale {
				t.Fatalf("duplicate: %v", err)
			}
		})
	}
}

func TestInvalidAnswerDoesNotConsume(t *testing.T) {
	p, a, now := fixture(t)
	bad := a
	bad.OptionID = "unknown"
	if p.Submit(bad, now) != ErrInvalid {
		t.Fatal("accepted unknown option")
	}
	bad = a
	bad.TurnID = "other"
	if p.Submit(bad, now) != ErrStale {
		t.Fatal("accepted different run")
	}
	bad = a
	bad.SessionID = "other"
	if p.Submit(bad, now) != ErrStale {
		t.Fatal("accepted different session")
	}
	if err := p.Submit(a, now); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredAndClosed(t *testing.T) {
	p, a, now := fixture(t)
	if p.Submit(a, now.Add(time.Minute)) != ErrStale {
		t.Fatal("accepted expired answer")
	}
	p.Close()
	if p.Submit(a, now) != ErrStale {
		t.Fatal("accepted closed question")
	}
}

func TestConcurrentSubmission(t *testing.T) {
	p, a, now := fixture(t)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Submit(a, now) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d", accepted.Load())
	}
}
