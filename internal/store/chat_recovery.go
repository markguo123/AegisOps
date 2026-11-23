package store

import (
	"context"
	"fmt"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
)

// RecoverChatRuns is called once by the single Backend before accepting runs.
// The former process's workers and approval channels cannot be resumed.
func (s *dbStore) RecoverChatRuns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT session_id FROM turns WHERE status IN ('pending','running','waiting')`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.InterruptChatRun(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// InterruptChatRun requires that the owner has stopped the session's worker.
func (s *dbStore) InterruptChatRun(ctx context.Context, id string) error {
	turns, err := s.Turns().ListBySession(ctx, id)
	if err != nil {
		return err
	}
	head, _, err := s.ChatHead(ctx, id)
	if err != nil {
		return err
	}
	c := ChatCommit{SessionID: id, ExpectedHead: head, CommandID: fmt.Sprintf("interrupt-%d", time.Now().UnixNano())}
	var active []string
	now := time.Now().UTC()
	for _, turn := range turns {
		if turn.Status != session.TurnPending && turn.Status != session.TurnRunning && turn.Status != session.TurnWaiting {
			continue
		}
		turn.Status, turn.Error, turn.FinishedAt = session.TurnFailed, "PROCESS_INTERRUPTED: 运行已中断，请重新发起诊断", &now
		c.ExtraTurns = append(c.ExtraTurns, turn)
		active = append(active, turn.ID)
		c.Events = append(c.Events, sse.NewEvent(id, turn.ID, sse.EventTurnStatus, sse.TurnStatusData{
			TurnID: turn.ID, SessionID: id, Input: turn.Input, Status: sse.TurnFailed, Error: turn.Error,
			FinishedAt: &now,
		}))
	}
	if len(active) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	items, err := readSnapshotItems(ctx, tx, id, active)
	_ = tx.Rollback()
	if err != nil {
		return err
	}
	for _, item := range items {
		if in := item.Interaction; in != nil && in.Status == sse.InteractionPending {
			in.Status = sse.InteractionInvalidated
			c.Interactions = append(c.Interactions, in)
			c.Events = append(c.Events, sse.NewEvent(id, in.TurnID, sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: in}))
		}
		if m := item.Message; m != nil && m.Status == sse.MessageStreaming {
			m.Status, m.Error = sse.MessageInterrupted, "运行已中断"
			c.Messages = append(c.Messages, m)
			c.Collapse = append(c.Collapse, m.MessageID)
			c.Events = append(c.Events, sse.NewEvent(id, m.TurnID, sse.EventMessageFailed, sse.MessageFailedData{Message: m, Code: "PROCESS_INTERRUPTED"}))
		}
		if tool := item.Tool; tool != nil && (tool.Status == sse.ToolRunning || tool.Status == sse.ToolStarted) {
			tool.Status = sse.ToolUnknown
			c.Tools = append(c.Tools, tool)
			c.Events = append(c.Events, sse.NewEvent(id, tool.TurnID, sse.EventToolUpdated, sse.ToolUpdatedData{Tool: tool}))
		}
	}
	_, err = s.CommitChat(ctx, c)
	return err
}
