package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/security"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

var ErrBranchCutUnavailable = errors.New("只能从已完成且有最终回复的回合开启新会话")
var ErrBranchSourceRunning = errors.New("源会话仍有运行或待确认操作")

func (s *chatService) BranchSession(ctx context.Context, source *session.Session, turnID, branchID string, recheck func() error) error {
	chatStore, ok := s.store.(store.ChatStore)
	if !ok || s.startupErr != nil {
		return errors.New("会话存储暂不可用")
	}
	if existing, err := s.store.Sessions().Get(ctx, branchID); err == nil {
		if existing.UserID == source.UserID && existing.Branch != nil &&
			existing.Branch.SessionID == source.ID && existing.Branch.TurnID == turnID {
			return recheck()
		}
		return store.ErrExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	turns, err := s.store.Turns().ListBySession(ctx, source.ID)
	if err != nil {
		return err
	}
	prefix := make([]*session.Turn, 0, len(turns))
	found := false
	for _, turn := range turns {
		if turn.Status == session.TurnPending || turn.Status == session.TurnRunning || turn.Status == session.TurnWaiting {
			return ErrBranchSourceRunning
		}
		if !found && turn.Status == session.TurnDone && strings.TrimSpace(turn.Output) != "" && strings.TrimSpace(turn.Input) != "" {
			prefix = append(prefix, turn)
			found = turn.ID == turnID
		}
	}
	if !found {
		return ErrBranchCutUnavailable
	}
	branch := &session.Session{
		ID: branchID, UserID: source.UserID, Name: "问诊分支",
		Cluster: source.Cluster, Namespace: source.Namespace, Coverage: append([]byte(nil), source.Coverage...),
		State: "idle", CreatedAt: time.Now().UTC(),
		Branch: &session.BranchOrigin{SessionID: source.ID, TurnID: turnID},
	}
	commit := store.ChatCommit{CommandID: "branch", SessionID: branchID, CreateSession: true, Session: branch}
	podHistory := make(map[string][]session.Message)
	for _, message := range source.Messages {
		pod := message.PodObservation
		if pod != nil && pod.Cluster == source.Cluster {
			podHistory[pod.TurnID] = append(podHistory[pod.TurnID], message)
		}
	}
	redactor := security.NewRedactor()
	for index, turn := range prefix {
		input, output := redactor.Redact(turn.Input), redactor.Redact(turn.Output)
		id := fmt.Sprintf("%s-t%08d", branchID, index+1)
		branch.AddMessage(session.Message{Role: "user", Type: "text", Content: input, Timestamp: turn.CreatedAt})
		for _, message := range podHistory[turn.ID] {
			observation := *message.PodObservation
			observation.TurnID = id
			branch.AddMessage(session.Message{
				Role: "tool", Type: message.Type, Name: message.Name,
				PodObservation: &observation, Timestamp: message.Timestamp, ToolCallID: message.ToolCallID,
			})
		}
		branch.AddMessage(session.Message{Role: "assistant", Type: "text", Content: output, Timestamp: turn.UpdatedAt})
		commit.ExtraTurns = append(commit.ExtraTurns, &session.Turn{
			ID: id, SessionID: branchID, UserID: source.UserID, Status: session.TurnDone,
			Input: input, Output: output, CreatedAt: turn.CreatedAt, UpdatedAt: branch.CreatedAt,
		})
		message := &sse.Message{
			MessageID: id + "-answer", InvocationID: "inherited", TurnID: id, Role: "assistant", Purpose: sse.PurposeFinal,
			Status: sse.MessageCompleted, ItemOrder: int64(index + 1),
			Parts: []sse.Part{{PartID: id + "-text", Type: sse.PartText, Text: output, LastIndex: 1}},
		}
		commit.Messages = append(commit.Messages, message)
		commit.Events = append(commit.Events,
			sse.NewEvent(branchID, id, sse.EventMessageCompleted, sse.MessageCompletedData{Message: message}),
			sse.NewEvent(branchID, id, sse.EventTurnStatus, sse.TurnStatusData{
				TurnID: id, SessionID: branchID, Status: sse.TurnDone, Input: input, FinalMessageID: message.MessageID,
			}),
		)
	}
	if err := recheck(); err != nil {
		return err
	}
	_, err = chatStore.CommitChat(ctx, commit)
	return err
}
