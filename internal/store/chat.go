package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
)

// Chat 展示事实源（方案 25 §5.2）：规范对象与事件日志在同一事务提交，
// 提交后由 service 广播。存储只负责原子写与一致读，不解释消息用途。

const (
	// chatReplayWindow 是事件日志的最长回放窗口（首版 24 小时）。
	chatReplayWindow = 24 * time.Hour
	// chatMaxEventsPerSession 同时按条目数限制回放日志容量。
	chatMaxEventsPerSession = 20000
	// chatSnapshotItemLimit 是首屏快照的条目上限（超出部分靠历史分页）。
	chatSnapshotItemLimit = 4000
)

// ErrChatHeadConflict 表示提交时会话水位已变化（另一写者先提交）。
var ErrChatHeadConflict = errors.New("store: chat head conflict")

// ErrChatReplayUnavailable 表示游标早于事件日志保留范围，必须重取快照。
var ErrChatReplayUnavailable = errors.New("store: chat replay unavailable")

// ChatDelta 是一条追加到 part 的安全文本增量（每行只随本次增量增长）。
type ChatDelta struct {
	SessionID string
	MessageID string
	PartID    string
	Index     int
	Text      string
}

// ChatCommit 是一次原子提交的完整变更集。
type ChatCommit struct {
	CreateSession bool
	// CommandID 是本次提交命令的稳定 ID：事务内唯一登记，结果不明确时先用它
	// 查询已提交结果，避免重复追加文本。
	CommandID string
	SessionID string
	// ExpectedHead 是提交者看到的水位；与库中不一致时返回 ErrChatHeadConflict。
	ExpectedHead uint64

	Turn *session.Turn
	// ExtraTurns 是本次提交需要一并写入的其它 Turn（历史迁移批量导入）。
	ExtraTurns   []*session.Turn
	Messages     []*sse.Message
	Tools        []*sse.ToolCall
	Interactions []*sse.Interaction
	Notices      []*sse.Notice
	Deltas       []ChatDelta
	// Collapse 中的消息已完成：合并后的正文已在消息载荷里，块行可以回收。
	Collapse []string
	// Events 是本次要追加的语义事件（Seq 由存储分配）。
	Events []sse.Event
	// Session 非空时在同一事务内同步模型历史（终态提交使用）。
	Session *session.Session
}

// ChatCommitResult 是提交结果。
type ChatCommitResult struct {
	Head     uint64
	FirstSeq uint64
	LastSeq  uint64
	// Duplicate 表示同 CommandID 已提交过；Events 为当时提交的事件。
	Duplicate bool
	Events    []sse.Event
}

// ChatStore 是 Chat 展示事实源的存储能力（SQLite/MySQL 同时实现）。
type ChatStore interface {
	CommitChat(ctx context.Context, c ChatCommit) (ChatCommitResult, error)
	ChatSnapshot(ctx context.Context, sessionID string, turnLimit int) (sse.Snapshot, error)
	ChatEventsAfter(ctx context.Context, sessionID string, cursor uint64, limit int) ([]sse.Event, error)
	ChatHead(ctx context.Context, sessionID string) (head, floor uint64, err error)
	// LegacyMigrated 标记旧会话已一次性转换为规范展示对象（幂等）。
	MarkLegacyMigrated(ctx context.Context, sessionID, digest string, itemCount int) error
	LegacyMigrated(ctx context.Context, sessionID string) (bool, error)
}

var _ ChatStore = (*dbStore)(nil)

// CommitChat 在一个事务内分配 seq、更新展示实体与 Turn 状态、追加事件并推进水位。
func (s *dbStore) CommitChat(ctx context.Context, c ChatCommit) (ChatCommitResult, error) {
	switch {
	case strings.TrimSpace(c.SessionID) == "":
		return ChatCommitResult{}, fmt.Errorf("commit chat: empty session id")
	case strings.TrimSpace(c.CommandID) == "":
		return ChatCommitResult{}, fmt.Errorf("commit chat: empty command id")
	}
	var result ChatCommitResult
	err := s.withChatTx(ctx, func(tx *sql.Tx) error {
		// 1. 幂等：同命令 ID 已提交时直接返回当时结果。
		dup, first, last, err := lookupChatCommit(ctx, tx, c.SessionID, c.CommandID)
		if err != nil {
			return err
		}
		if dup {
			events, err := readChatEvents(ctx, tx, c.SessionID, first-1, int(last-first+1))
			if err != nil {
				return err
			}
			result = ChatCommitResult{Head: last, FirstSeq: first, LastSeq: last, Duplicate: true, Events: events}
			return nil
		}
		if c.CreateSession {
			if c.Session == nil || c.Session.ID != c.SessionID {
				return fmt.Errorf("commit chat: new session must match commit session")
			}
			if err := insertSession(ctx, tx, c.Session, s.sessionMaxBytes); err != nil {
				return err
			}
		}
		// 2. 会话水位与回放下限（MySQL 用 FOR UPDATE 串行化同会话写者）。
		head, floor, err := lockChatHead(ctx, tx, s.dialect, c.SessionID)
		if err != nil {
			return err
		}
		result.Head, result.FirstSeq, result.LastSeq = head, head, head
		if floor > 0 {
			// 让调用方感知回放下限（首屏快照据此提示）。
			_ = floor
		}
		if c.ExpectedHead != head {
			return fmt.Errorf("%w: expected %d actual %d", ErrChatHeadConflict, c.ExpectedHead, head)
		}
		now := time.Now().UTC()
		// 3. Turn 主记录状态（复用 turns 表，与展示事实同事务）。
		for _, turn := range append([]*session.Turn{c.Turn}, c.ExtraTurns...) {
			if turn == nil {
				continue
			}
			if err := upsertTurnTx(ctx, tx, s.dialect, turn, now); err != nil {
				return err
			}
		}
		// 4. 追加文本块。
		for _, d := range c.Deltas {
			if err := insertChatChunk(ctx, tx, c.SessionID, d, now); err != nil {
				return err
			}
		}
		// 5. 消息完成：块行回收（正文已在消息载荷中）。
		for _, messageID := range c.Collapse {
			if _, err := tx.ExecContext(ctx, `DELETE FROM chat_chunks WHERE session_id = ? AND message_id = ?`, c.SessionID, messageID); err != nil {
				return fmt.Errorf("collapse chat chunks: %w", err)
			}
		}
		// 6. 追加语义事件并分配 seq；事件里携带的对象按该 seq 赋 revision。
		seq := head
		committed := make([]sse.Event, 0, len(c.Events))
		revisionOf := map[string]uint64{}
		for _, e := range c.Events {
			if err := validateChatEvent(e); err != nil {
				return err
			}
			seq++
			e.Seq = seq
			if e.Timestamp.IsZero() {
				e.Timestamp = now
			}
			if e.ProtocolVersion == 0 {
				e.ProtocolVersion = sse.ProtocolVersion
			}
			assignEventRevision(e.Data, seq, revisionOf)
			raw, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("marshal chat event: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chat_events (session_id, seq, event_type, turn_id, payload, created_at)
				VALUES (?, ?, ?, ?, ?, ?)`,
				c.SessionID, seq, string(e.EventType), e.TurnID, string(raw), now); err != nil {
				return fmt.Errorf("insert chat event: %w", err)
			}
			committed = append(committed, e)
		}
		// 7. 规范对象（整对象版本化载荷），revision 与携带它的事件 seq 一致。
		if err := upsertChatObjects(ctx, tx, s.dialect, c, revisionOf, head, now); err != nil {
			return err
		}
		// 8. 登记提交命令 ID。
		firstSeq, lastSeq := head, seq
		if len(committed) > 0 {
			firstSeq, lastSeq = committed[0].Seq, committed[len(committed)-1].Seq
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_commits (session_id, command_id, first_seq, last_seq, created_at)
			VALUES (?, ?, ?, ?, ?)`, c.SessionID, c.CommandID, firstSeq, lastSeq, now); err != nil {
			return fmt.Errorf("record chat commit: %w", err)
		}
		// 9. 推进水位并清理过期事件（清理推进 replay_floor）。
		if err := advanceChatHead(ctx, tx, c.SessionID, seq, now); err != nil {
			return err
		}
		if err := pruneChatEvents(ctx, tx, c.SessionID, seq, now); err != nil {
			return err
		}
		// 10. 终态提交同事务保存模型历史与终态，避免“先宣告成功再保存会话”。
		if c.Session != nil && !c.CreateSession {
			if _, err := updateSession(ctx, tx, c.Session, s.sessionMaxBytes); err != nil {
				return err
			}
		}
		result.Head, result.FirstSeq, result.LastSeq = seq, firstSeq, lastSeq
		result.Events = committed
		return nil
	})
	if err != nil {
		return ChatCommitResult{}, err
	}
	return result, nil
}

// withChatTx 执行事务；SQLite 驱动把连接池限成单连接，事务天然串行。
func (s *dbStore) withChatTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin chat tx: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit chat tx: %w", err)
	}
	return nil
}

func lockChatHead(ctx context.Context, tx *sql.Tx, d dialect, sessionID string) (head, floor uint64, err error) {
	query := `SELECT head, replay_floor FROM chat_stream_heads WHERE session_id = ?`
	if d != nil && d.name() == "mysql" {
		query += ` FOR UPDATE`
	}
	row := tx.QueryRowContext(ctx, query, sessionID)
	if err := row.Scan(&head, &floor); err != nil {
		if !errors.Is(mapErr(err, "get chat head"), ErrNotFound) {
			return 0, 0, mapErr(err, "get chat head")
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO chat_stream_heads (session_id, head, replay_floor, protocol_version, epoch, updated_at)
			VALUES (?, 0, 0, 2, 0, ?)`, sessionID, time.Now().UTC()); err != nil {
			// 并发插入同一会话时忽略唯一冲突，随后重读。
			if !isUniqueViolation(err) {
				return 0, 0, fmt.Errorf("create chat head: %w", err)
			}
		}
		row = tx.QueryRowContext(ctx, query, sessionID)
		if err := row.Scan(&head, &floor); err != nil {
			return 0, 0, mapErr(err, "get chat head")
		}
	}
	return head, floor, nil
}

func lookupChatCommit(ctx context.Context, tx *sql.Tx, sessionID, commandID string) (bool, uint64, uint64, error) {
	var first, last uint64
	err := tx.QueryRowContext(ctx, `
		SELECT first_seq, last_seq FROM chat_commits WHERE session_id = ? AND command_id = ?`,
		sessionID, commandID).Scan(&first, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, 0, nil
	}
	if err != nil {
		return false, 0, 0, fmt.Errorf("lookup chat commit: %w", err)
	}
	return true, first, last, nil
}

func upsertTurnTx(ctx context.Context, tx *sql.Tx, d dialect, t *session.Turn, now time.Time) error {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now
	cols := []string{"id", "session_id", "user_id", "status", "input", "output", "error",
		"prompt_tokens", "completion_tokens", "total_tokens", "started_at", "finished_at", "created_at", "updated_at"}
	query := d.upsert("turns", cols, []string{"id"}, cols[1:])
	_, err := tx.ExecContext(ctx, query,
		t.ID, t.SessionID, t.UserID, string(t.Status),
		redactJSONPayload(t.Input), redactJSONPayload(t.Output), redactJSONPayload(t.Error),
		t.Token.PromptTokens, t.Token.CompletionTokens, t.Token.TotalTokens,
		optionalTime(t.StartedAt), nilTime(t.FinishedAt), t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert turn: %w", err)
	}
	return nil
}

// assignEventRevision 给事件载荷中的对象赋 revision，并记录对象 ID → seq。
// 规范对象的 revision 必须等于更新它的那个事件的 seq（快照与事件同源）。
func assignEventRevision(data any, seq uint64, out map[string]uint64) {
	switch v := data.(type) {
	case sse.MessageStartedData:
		if v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case *sse.MessageStartedData:
		if v != nil && v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case sse.MessageCompletedData:
		if v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case *sse.MessageCompletedData:
		if v != nil && v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case sse.MessageFailedData:
		if v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case *sse.MessageFailedData:
		if v != nil && v.Message != nil {
			v.Message.Revision = seq
			out[v.Message.MessageID] = seq
		}
	case sse.ToolUpdatedData:
		if v.Tool != nil {
			v.Tool.Revision = seq
			out[v.Tool.ToolCallID] = seq
		}
	case *sse.ToolUpdatedData:
		if v != nil && v.Tool != nil {
			v.Tool.Revision = seq
			out[v.Tool.ToolCallID] = seq
		}
	case sse.InteractionUpdatedData:
		if v.Interaction != nil {
			v.Interaction.Revision = seq
			out[v.Interaction.InteractionID] = seq
		}
	case *sse.InteractionUpdatedData:
		if v != nil && v.Interaction != nil {
			v.Interaction.Revision = seq
			out[v.Interaction.InteractionID] = seq
		}
	case sse.NoticeCreatedData:
		if v.Notice != nil {
			v.Notice.Revision = seq
			out[v.Notice.NoticeID] = seq
		}
	case *sse.NoticeCreatedData:
		if v != nil && v.Notice != nil {
			v.Notice.Revision = seq
			out[v.Notice.NoticeID] = seq
		}
	}
}

func upsertChatObjects(ctx context.Context, tx *sql.Tx, d dialect, c ChatCommit, revisions map[string]uint64, head uint64, now time.Time) error {
	revFor := func(id string) uint64 {
		if rev := revisions[id]; rev > 0 {
			return rev
		}
		return head
	}
	upsertItem := func(itemID, itemType, turnID string, order int64, payload any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal chat item %s: %w", itemID, err)
		}
		cols := []string{"session_id", "item_id", "item_type", "turn_id", "item_order", "revision", "payload", "updated_at"}
		query := d.upsert("chat_items", cols, []string{"session_id", "item_id"}, cols[2:])
		_, err = tx.ExecContext(ctx, query, c.SessionID, itemID, itemType, turnID, order, revFor(itemID), string(raw), now)
		if err != nil {
			return fmt.Errorf("upsert chat item %s: %w", itemID, err)
		}
		return nil
	}
	for _, m := range c.Messages {
		if m == nil {
			continue
		}
		m.Revision = revFor(m.MessageID)
		if err := upsertItem(m.MessageID, string(sse.ItemMessage), m.TurnID, m.ItemOrder, m); err != nil {
			return err
		}
	}
	for _, t := range c.Tools {
		if t == nil {
			continue
		}
		t.Revision = revFor(t.ToolCallID)
		if err := upsertItem(t.ToolCallID, string(sse.ItemTool), t.TurnID, t.ItemOrder, t); err != nil {
			return err
		}
	}
	for _, in := range c.Interactions {
		if in == nil {
			continue
		}
		in.Revision = revFor(in.InteractionID)
		if err := upsertItem(in.InteractionID, string(sse.ItemInteraction), in.TurnID, in.ItemOrder, in); err != nil {
			return err
		}
	}
	for _, n := range c.Notices {
		if n == nil {
			continue
		}
		n.Revision = revFor(n.NoticeID)
		if err := upsertItem(n.NoticeID, string(sse.ItemNotice), n.TurnID, n.ItemOrder, n); err != nil {
			return err
		}
	}
	return nil
}

func insertChatChunk(ctx context.Context, tx *sql.Tx, sessionID string, d ChatDelta, now time.Time) error {
	if d.MessageID == "" || d.PartID == "" {
		return fmt.Errorf("insert chat chunk: missing message/part id")
	}
	if d.Text == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO chat_chunks (session_id, message_id, part_id, idx, text, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, d.MessageID, d.PartID, d.Index, d.Text, now)
	if err != nil {
		return fmt.Errorf("insert chat chunk: %w", err)
	}
	return nil
}

func validateChatEvent(e sse.Event) error {
	if e.EventType == "" {
		return fmt.Errorf("chat event: empty type")
	}
	if sse.IsControlFrame(e.EventType) {
		return fmt.Errorf("chat event: control frame %s must not be persisted", e.EventType)
	}
	if e.SessionID == "" {
		return fmt.Errorf("chat event %s: empty session id", e.EventType)
	}
	return nil
}

func advanceChatHead(ctx context.Context, tx *sql.Tx, sessionID string, head uint64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE chat_stream_heads SET head = ?, protocol_version = 2, updated_at = ?
		WHERE session_id = ? AND head < ?`, head, now, sessionID, head); err != nil {
		return fmt.Errorf("advance chat head: %w", err)
	}
	return nil
}

// pruneChatEvents 清理超出时间窗口与条目上限的事件，并推进 replay_floor。
// 清理只发生在文本已进入 chat_items/chat_chunks 之后（本事务先写实体再清理）。
func pruneChatEvents(ctx context.Context, tx *sql.Tx, sessionID string, head uint64, now time.Time) error {
	cutoff := now.Add(-chatReplayWindow)
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_events WHERE session_id = ? AND created_at < ?`, sessionID, cutoff); err != nil {
		return fmt.Errorf("prune chat events by age: %w", err)
	}
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_events WHERE session_id = ?`, sessionID).Scan(&total); err != nil {
		return fmt.Errorf("count chat events: %w", err)
	}
	if total > chatMaxEventsPerSession {
		keepFrom := head - chatMaxEventsPerSession + 1
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_events WHERE session_id = ? AND seq < ?`, sessionID, keepFrom); err != nil {
			return fmt.Errorf("prune chat events by count: %w", err)
		}
	}
	var minSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MIN(seq) FROM chat_events WHERE session_id = ?`, sessionID).Scan(&minSeq); err != nil {
		return fmt.Errorf("read chat event floor: %w", err)
	}
	floor := head
	if minSeq.Valid && minSeq.Int64 > 0 {
		floor = uint64(minSeq.Int64) - 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chat_stream_heads SET replay_floor = ? WHERE session_id = ? AND replay_floor < ?`,
		floor, sessionID, floor); err != nil {
		return fmt.Errorf("advance replay floor: %w", err)
	}
	return nil
}

// ChatHead 返回会话当前水位与可回放下限。
func (s *dbStore) ChatHead(ctx context.Context, sessionID string) (uint64, uint64, error) {
	var head, floor uint64
	err := s.db.QueryRowContext(ctx, `SELECT head, replay_floor FROM chat_stream_heads WHERE session_id = ?`, sessionID).
		Scan(&head, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, mapErr(err, "get chat head")
	}
	return head, floor, nil
}

// ChatSnapshot 在一个短一致读事务内获取规范实体与水位，形成首屏快照。
func (s *dbStore) ChatSnapshot(ctx context.Context, sessionID string, turnLimit int) (sse.Snapshot, error) {
	if turnLimit <= 0 {
		turnLimit = 30
	}
	snap := sse.Snapshot{ProtocolVersion: sse.ProtocolVersion, SessionID: sessionID}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snap, fmt.Errorf("begin chat snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	head, floor, err := lockChatHeadRead(ctx, tx, sessionID)
	if err != nil {
		return snap, err
	}
	snap.Cursor, snap.ReplayFloor = head, floor

	turns, ids, err := readSnapshotTurns(ctx, tx, sessionID, turnLimit)
	if err != nil {
		return snap, err
	}
	snap.Turns = turns
	items, err := readSnapshotItems(ctx, tx, sessionID, ids)
	if err != nil {
		return snap, err
	}
	snap.Items = items
	return snap, nil
}

func lockChatHeadRead(ctx context.Context, tx *sql.Tx, sessionID string) (uint64, uint64, error) {
	var head, floor uint64
	err := tx.QueryRowContext(ctx, `SELECT head, replay_floor FROM chat_stream_heads WHERE session_id = ?`, sessionID).
		Scan(&head, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, mapErr(err, "get chat head")
	}
	return head, floor, nil
}

func readSnapshotTurns(ctx context.Context, tx *sql.Tx, sessionID string, limit int) ([]sse.TurnView, []string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, session_id, status, input, output, error,
			prompt_tokens, completion_tokens, total_tokens, started_at, finished_at, updated_at,
			(SELECT e.payload FROM chat_events e WHERE e.session_id = turns.session_id
				AND e.turn_id = turns.id AND e.event_type = 'turn.status' ORDER BY e.seq DESC LIMIT 1)
		FROM turns WHERE session_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("read snapshot turns: %w", err)
	}
	defer rows.Close()
	var out []sse.TurnView
	ids := make([]string, 0, limit)
	for rows.Next() {
		t := &session.Turn{}
		var started, finished sql.NullTime
		var statusPayload []byte
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Status, &t.Input, &t.Output, &t.Error,
			&t.Token.PromptTokens, &t.Token.CompletionTokens, &t.Token.TotalTokens,
			&started, &finished, &t.UpdatedAt, &statusPayload); err != nil {
			return nil, nil, fmt.Errorf("scan snapshot turn: %w", err)
		}
		if started.Valid {
			ts := started.Time
			t.StartedAt = ts
		}
		view := sse.TurnView{
			TurnID: t.ID, SessionID: t.SessionID, Input: t.Input, Status: sse.TurnStatus(t.Status),
			Error: t.Error, StartedAt: nilOrTime(t.StartedAt), FinishedAt: nilOrTimePtr(finished),
			Usage: sse.TokenUsage{
				PromptTokens: t.Token.PromptTokens, CompletionTokens: t.Token.CompletionTokens, TotalTokens: t.Token.TotalTokens,
			},
		}
		if len(statusPayload) > 0 {
			var stored struct {
				Data sse.TurnStatusData `json:"data"`
			}
			if err := json.Unmarshal(statusPayload, &stored); err != nil {
				return nil, nil, fmt.Errorf("decode snapshot turn status: %w", err)
			}
			view.ContextBudget = stored.Data.ContextBudget
		}
		out = append(out, view)
		ids = append(ids, t.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// 查询是倒序取最近 N 条；快照按时间正序返回。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return out, ids, nil
}

func readSnapshotItems(ctx context.Context, tx *sql.Tx, sessionID string, turnIDs []string) ([]sse.Item, error) {
	if len(turnIDs) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(turnIDs))
	args := make([]any, 0, len(turnIDs)+2)
	args = append(args, sessionID)
	for i, id := range turnIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	args = append(args, chatSnapshotItemLimit)
	query := fmt.Sprintf(`
		SELECT item_type, item_order, revision, payload FROM chat_items
		WHERE session_id = ? AND turn_id IN (%s) ORDER BY item_order ASC LIMIT ?`,
		strings.Join(placeholders, ", "))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read snapshot items: %w", err)
	}
	type row struct {
		typ      string
		order    int64
		revision uint64
		payload  []byte
	}
	var raw []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.typ, &r.order, &r.revision, &r.payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan snapshot item: %w", err)
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]sse.Item, 0, len(raw))
	for _, r := range raw {
		item, err := decodeChatItem(ctx, tx, sessionID, r.typ, r.order, r.revision, r.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ItemOrder < out[j].ItemOrder })
	return out, nil
}

func decodeChatItem(ctx context.Context, tx *sql.Tx, sessionID, typ string, order int64, revision uint64, payload []byte) (sse.Item, error) {
	item := sse.Item{ItemOrder: order}
	switch sse.ItemType(typ) {
	case sse.ItemMessage:
		m := &sse.Message{}
		if err := json.Unmarshal(payload, m); err != nil {
			return item, fmt.Errorf("decode chat message: %w", err)
		}
		m.Revision = revision
		if err := hydrateMessageText(ctx, tx, sessionID, m); err != nil {
			return item, err
		}
		item.Type = sse.ItemMessage
		item.Message = m
	case sse.ItemTool:
		t := &sse.ToolCall{}
		if err := json.Unmarshal(payload, t); err != nil {
			return item, fmt.Errorf("decode chat tool: %w", err)
		}
		t.Revision = revision
		item.Type = sse.ItemTool
		item.Tool = t
	case sse.ItemInteraction:
		in := &sse.Interaction{}
		if err := json.Unmarshal(payload, in); err != nil {
			return item, fmt.Errorf("decode chat interaction: %w", err)
		}
		in.Revision = revision
		item.Type = sse.ItemInteraction
		item.Interaction = in
	case sse.ItemNotice:
		n := &sse.Notice{}
		if err := json.Unmarshal(payload, n); err != nil {
			return item, fmt.Errorf("decode chat notice: %w", err)
		}
		n.Revision = revision
		item.Type = sse.ItemNotice
		item.Notice = n
	}
	return item, nil
}

// hydrateMessageText 从追加块组装消息正文（未完成消息的文本尚未合并进载荷）。
func hydrateMessageText(ctx context.Context, tx *sql.Tx, sessionID string, m *sse.Message) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT part_id, idx, text FROM chat_chunks WHERE session_id = ? AND message_id = ? ORDER BY part_id, idx`,
		sessionID, m.MessageID)
	if err != nil {
		return fmt.Errorf("read chat chunks: %w", err)
	}
	defer rows.Close()
	texts := map[string]map[int]string{}
	for rows.Next() {
		var partID, text string
		var idx int
		if err := rows.Scan(&partID, &idx, &text); err != nil {
			return fmt.Errorf("scan chat chunk: %w", err)
		}
		if texts[partID] == nil {
			texts[partID] = map[int]string{}
		}
		texts[partID][idx] = text
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range m.Parts {
		chunks := texts[m.Parts[i].PartID]
		if len(chunks) == 0 {
			continue
		}
		indexes := make([]int, 0, len(chunks))
		for idx := range chunks {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)
		var b strings.Builder
		b.WriteString(m.Parts[i].Text)
		last := m.Parts[i].LastIndex
		for _, idx := range indexes {
			if idx < last {
				continue // 已完成消息可能已有合并正文
			}
			b.WriteString(chunks[idx])
			last = idx + 1
		}
		m.Parts[i].Text = b.String()
		m.Parts[i].LastIndex = last
	}
	return nil
}

// ChatEventsAfter 读取游标之后的持久事件；游标早于回放下限时返回
// ErrChatReplayUnavailable，调用方必须让客户端重取快照。
func (s *dbStore) ChatEventsAfter(ctx context.Context, sessionID string, cursor uint64, limit int) ([]sse.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	_, floor, err := s.ChatHead(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if cursor < floor {
		return nil, fmt.Errorf("%w: cursor %d < floor %d", ErrChatReplayUnavailable, cursor, floor)
	}
	return readChatEvents(ctx, s.db, sessionID, cursor, limit)
}

// queryer 是 *sql.DB 与 *sql.Tx 共有的读能力。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readChatEvents(ctx context.Context, q queryer, sessionID string, after uint64, limit int) ([]sse.Event, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := q.QueryContext(ctx, `
		SELECT payload FROM chat_events WHERE session_id = ? AND seq > ? ORDER BY seq ASC LIMIT ?`,
		sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read chat events: %w", err)
	}
	defer rows.Close()
	var out []sse.Event
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan chat event: %w", err)
		}
		var e sse.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("decode chat event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkLegacyMigrated 登记一次旧会话展示迁移（幂等）。
func (s *dbStore) MarkLegacyMigrated(ctx context.Context, sessionID, digest string, itemCount int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chat_legacy_migrations (session_id, source_digest, item_count, migrated_at)
		VALUES (?, ?, ?, ?)`, sessionID, digest, itemCount, time.Now().UTC())
	if err != nil {
		if isUniqueViolation(err) {
			return nil
		}
		return fmt.Errorf("mark legacy migrated: %w", err)
	}
	return nil
}

// LegacyMigrated 报告旧会话是否已转换。
func (s *dbStore) LegacyMigrated(ctx context.Context, sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_legacy_migrations WHERE session_id = ?`, sessionID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check legacy migrated: %w", err)
	}
	return n > 0, nil
}

func nilOrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}

func nilOrTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	tt := t.Time
	return &tt
}
