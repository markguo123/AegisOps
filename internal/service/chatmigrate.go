package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// EnsureChatDisplay 把旧会话（sessions.messages + turns）一次性转换成规范展示对象，
// 让历史与实时共用同一套实体与事件日志（方案 25 §12「旧历史」）。
//
// 迁移是幂等的：会话级标记 + 确定性 legacy ID，重复运行不会重复生成条目。
// 无法恢复的耗时/审批/执行结果一律留空并显式标注来源缺失，不补造事实。
func (s *chatService) EnsureChatDisplay(ctx context.Context, sessionID string) error {
	s.ensureFlow()
	if s.flow == nil || s.store == nil {
		return nil
	}
	cs, ok := s.store.(store.ChatStore)
	if !ok {
		return nil
	}
	migrated, err := cs.LegacyMigrated(ctx, sessionID)
	if err != nil {
		return err
	}
	if migrated {
		return nil
	}
	sess, err := s.sessions.Get(sessionID)
	if err != nil {
		if errors.Is(err, session.ErrNotFound) {
			return nil
		}
		return err
	}
	if len(sess.Messages) == 0 {
		// 没有历史正文：登记标记，避免每次读会话都重新检查。
		return cs.MarkLegacyMigrated(ctx, sessionID, "", 0)
	}
	commit, digest, count, err := buildLegacyCommit(ctx, s.store, sess)
	if err != nil {
		return err
	}
	head, _, err := cs.ChatHead(ctx, sessionID)
	if err != nil {
		return err
	}
	commit.ExpectedHead = head
	commit.CommandID = "legacy-" + sessionID
	if _, err := cs.CommitChat(ctx, commit); err != nil && !errors.Is(err, store.ErrChatHeadConflict) {
		return fmt.Errorf("migrate legacy chat display: %w", err)
	}
	if err := cs.MarkLegacyMigrated(ctx, sessionID, digest, count); err != nil {
		return err
	}
	slog.Info("chat: 旧会话已转换为规范展示对象", "session", sessionID, "items", count)
	return nil
}

// buildLegacyCommit 把旧 messages 转成规范对象与事件。
func buildLegacyCommit(ctx context.Context, st store.Store, sess *session.Session) (store.ChatCommit, string, int, error) {
	// 旧 Turn 记录按创建顺序对应消息分段；数量不匹配时用确定性 legacy ID，
	// 并保留 status 来源缺失的说明。
	var turnIDs []string
	var turnStatus []string
	if st != nil {
		turns, err := st.Turns().ListBySession(ctx, sess.ID)
		if err == nil {
			for _, t := range turns {
				turnIDs = append(turnIDs, t.ID)
				turnStatus = append(turnStatus, string(t.Status))
			}
		}
	}
	// 按 user 消息切分历史回合；无 user 前缀的孤立消息单独成段。
	type segment struct {
		index int
		input string
		msgs  []session.Message
	}
	var segments []segment
	for _, m := range sess.Messages {
		if m.Role == "user" && m.Type == "text" {
			segments = append(segments, segment{index: len(segments), input: m.Content})
			continue
		}
		if len(segments) == 0 {
			segments = append(segments, segment{index: 0})
		}
		segments[len(segments)-1].msgs = append(segments[len(segments)-1].msgs, m)
	}
	if len(segments) == 0 {
		return store.ChatCommit{}, "", 0, nil
	}

	raw, err := json.Marshal(sess.Messages)
	if err != nil {
		return store.ChatCommit{}, "", 0, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))

	commit := store.ChatCommit{SessionID: sess.ID}
	order := int64(0)
	take := func() int64 { order++; return order }
	now := time.Now().UTC()

	for _, seg := range segments {
		turnID := fmt.Sprintf("legacy-%s-t%d", sess.ID, seg.index+1)
		status := session.TurnDone
		if seg.index < len(turnIDs) && turnIDs[seg.index] != "" {
			turnID = turnIDs[seg.index]
			if seg.index < len(turnStatus) && turnStatus[seg.index] != "" {
				status = session.TurnStatus(turnStatus[seg.index])
			}
		}
		turn := &session.Turn{
			ID: turnID, SessionID: sess.ID, Status: status, Input: seg.input,
			CreatedAt: sess.CreatedAt, UpdatedAt: now,
		}
		// 状态来源缺失的旧回合显式标注，不伪造耗时。
		notice := &sse.Notice{
			NoticeID: fmt.Sprintf("legacy-%s-n%d", sess.ID, seg.index+1),
			TurnID:   turnID, Severity: sse.NoticeInfo, Code: "LEGACY_HISTORY_IMPORTED",
			Message:   "该回合来自历史会话迁移：耗时、审批与执行结果按原记录保留，缺失项不补造。",
			ItemOrder: take(), CreatedAt: &now,
		}
		commit.ExtraTurns = append(commit.ExtraTurns, turn)
		commit.Notices = append(commit.Notices, notice)
		commit.Events = append(commit.Events, sse.NewEvent(sess.ID, turnID, sse.EventNoticeCreated, sse.NoticeCreatedData{Notice: notice}))

		lastAssistant := -1
		for i, m := range seg.msgs {
			if m.Role == "assistant" && m.Type != "reasoning" && m.Type != "error" {
				lastAssistant = i
			}
		}
		for i, m := range seg.msgs {
			switch {
			case m.Role == "tool" || m.Type == "tool-call-response":
				ok := m.Ok
				status := sse.ToolSucceeded
				if ok != nil && !*ok {
					status = sse.ToolFailed
				}
				tool := &sse.ToolCall{
					ToolCallID: legacyToolID(m, sess.ID, seg.index, i),
					TurnID:     turnID, Name: m.Name, Params: string(m.Arguments),
					Status: status, Ok: ok, Result: m.Content, Summary: m.Summary,
					DisplayType: m.DisplayType, DurationMs: m.DurationMs, Risk: m.Risk,
					Command: m.Command, Stdout: m.Stdout, Stderr: m.Stderr,
					ExitCode: m.ExitCode, ErrorCode: m.ErrorCode, Data: m.Data,
					ItemOrder: take(),
				}
				commit.Tools = append(commit.Tools, tool)
				commit.Events = append(commit.Events, sse.NewEvent(sess.ID, turnID, sse.EventToolUpdated, sse.ToolUpdatedData{Tool: tool}))
			case m.Type == "reasoning":
				msg := &sse.Message{
					MessageID: fmt.Sprintf("legacy-%s-m%d-%d", sess.ID, seg.index+1, i),
					TurnID:    turnID, InvocationID: "legacy", Role: "assistant",
					Purpose: sse.PurposeCommentary, Status: sse.MessageCompleted,
					Parts:     []sse.Part{{PartID: fmt.Sprintf("legacy-%s-p%d-%d", sess.ID, seg.index+1, i), Type: sse.PartReasoning, Text: m.Content, LastIndex: 1}},
					ItemOrder: take(),
				}
				commit.Messages = append(commit.Messages, msg)
				commit.Events = append(commit.Events,
					sse.NewEvent(sess.ID, turnID, sse.EventMessageStarted, sse.MessageStartedData{Message: msg}),
					sse.NewEvent(sess.ID, turnID, sse.EventMessageCompleted, sse.MessageCompletedData{Message: msg}))
			case m.Type == "error":
				msg := &sse.Message{
					MessageID: fmt.Sprintf("legacy-%s-m%d-%d", sess.ID, seg.index+1, i),
					TurnID:    turnID, InvocationID: "legacy", Role: "assistant",
					Purpose: sse.PurposeFinal, Status: sse.MessageFailed, Error: m.Content,
					Parts:     []sse.Part{{PartID: fmt.Sprintf("legacy-%s-p%d-%d", sess.ID, seg.index+1, i), Type: sse.PartText, LastIndex: 0}},
					ItemOrder: take(),
				}
				commit.Messages = append(commit.Messages, msg)
				commit.Events = append(commit.Events,
					sse.NewEvent(sess.ID, turnID, sse.EventMessageStarted, sse.MessageStartedData{Message: msg}),
					sse.NewEvent(sess.ID, turnID, sse.EventMessageFailed, sse.MessageFailedData{Message: msg, Error: m.Content}))
			case m.Role == "assistant" && m.Type == "text":
				purpose := sse.PurposeCommentary
				if i == lastAssistant {
					purpose = sse.PurposeFinal
				}
				msg := &sse.Message{
					MessageID: fmt.Sprintf("legacy-%s-m%d-%d", sess.ID, seg.index+1, i),
					TurnID:    turnID, InvocationID: "legacy", Role: "assistant",
					Purpose: purpose, Status: sse.MessageCompleted,
					Parts:     []sse.Part{{PartID: fmt.Sprintf("legacy-%s-p%d-%d", sess.ID, seg.index+1, i), Type: sse.PartText, Text: m.Content, LastIndex: 1}},
					ItemOrder: take(),
				}
				commit.Messages = append(commit.Messages, msg)
				commit.Events = append(commit.Events,
					sse.NewEvent(sess.ID, turnID, sse.EventMessageStarted, sse.MessageStartedData{Message: msg}),
					sse.NewEvent(sess.ID, turnID, sse.EventMessageCompleted, sse.MessageCompletedData{Message: msg}))
			}
		}
		commit.Events = append(commit.Events, sse.NewEvent(sess.ID, turnID, sse.EventTurnStatus, sse.TurnStatusData{
			TurnID: turnID, SessionID: sess.ID, Status: sse.TurnStatus(status), Input: seg.input,
		}))
	}
	return commit, digest, int(order), nil
}

func legacyToolID(m session.Message, sessionID string, segment, index int) string {
	if m.ToolCallID != "" {
		return m.ToolCallID
	}
	return fmt.Sprintf("legacy-%s-tool%d-%d", sessionID, segment+1, index)
}
