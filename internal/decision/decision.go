// Package decision defines user decisions independently of operation approvals.
package decision

import (
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Impact      string `json:"impact"`
	Recommended bool   `json:"recommended,omitempty"`
}

type Question struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	Question  string    `json:"question"`
	Options   []Option  `json:"options"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Answer struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Status    string `json:"status"`
	OptionID  string `json:"option_id,omitempty"`
	Text      string `json:"text,omitempty"`
}

var ErrInvalid = errors.New("invalid decision")
var ErrStale = errors.New("decision expired or already answered")

func bounded(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max
}

func (q Question) Validate() error {
	if !bounded(q.ID, 128) || !bounded(q.SessionID, 128) || !bounded(q.TurnID, 128) || !bounded(q.Question, 1000) || q.ExpiresAt.IsZero() || len(q.Options) > 6 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	recommended := 0
	for _, o := range q.Options {
		if !bounded(o.ID, 64) || seen[o.ID] || !bounded(o.Label, 120) || !bounded(o.Impact, 500) {
			return ErrInvalid
		}
		seen[o.ID] = true
		if o.Recommended {
			recommended++
		}
	}
	if recommended > 1 {
		return ErrInvalid
	}
	return nil
}

// Pending owns a single question and consumes exactly one valid answer.
// The caller remains responsible for authentication and session ownership.
type Pending struct {
	mu       sync.Mutex
	question Question
	closed   bool
	reply    chan Answer
}

func New(q Question) (*Pending, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	q.Options = append([]Option(nil), q.Options...)
	return &Pending{question: q, reply: make(chan Answer, 1)}, nil
}

func (p *Pending) Answers() <-chan Answer { return p.reply }

func (p *Pending) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

func (p *Pending) Submit(a Answer, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.question
	if p.closed || !now.Before(q.ExpiresAt) || a.ID != q.ID || a.SessionID != q.SessionID || a.TurnID != q.TurnID {
		return ErrStale
	}
	switch a.Status {
	case "selected":
		found := false
		for _, o := range q.Options {
			if o.ID == a.OptionID {
				found = true
			}
		}
		if !found || a.Text != "" {
			return ErrInvalid
		}
	case "text":
		if a.OptionID != "" || !bounded(a.Text, 2000) {
			return ErrInvalid
		}
	case "skipped":
		if a.OptionID != "" || a.Text != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	p.closed = true
	p.reply <- a
	return nil
}
