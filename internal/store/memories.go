package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

func (st *memoryStore) ListOwned(ctx context.Context, ownerID string, limit int) ([]*Memory, error) {
	if ownerID == "" || limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("list owned memories: invalid owner or limit")
	}
	rows, err := st.s.db.QueryContext(ctx, `
		SELECT id, user_id, scope, namespace, cluster_id, content, created_at, updated_at
		FROM memories WHERE user_id = ? ORDER BY updated_at DESC, id DESC LIMIT ?`, ownerID, limit)
	if err != nil {
		return nil, fmt.Errorf("list owned memories: %w", err)
	}
	defer rows.Close()

	memories := make([]*Memory, 0)
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan owned memory: %w", err)
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (st *memoryStore) DeleteOwned(ctx context.Context, ownerID, id string) error {
	if ownerID == "" || id == "" {
		return ErrNotFound
	}
	result, err := st.s.db.ExecContext(ctx, `
		DELETE FROM memories WHERE user_id = ? AND id = ?`, ownerID, id)
	if err != nil {
		return fmt.Errorf("delete owned memory: %w", err)
	}
	return ensureAffected(result, "delete owned memory")
}

var ErrMemoryConflict = errors.New("diagnostic memory has changed")

func (st *memoryStore) UpdateOwned(ctx context.Context, next *Memory, previousContent string) error {
	res, err := st.s.db.ExecContext(ctx, `
		UPDATE memories SET cluster_id = ?, namespace = ?, content = ?, updated_at = ?
		WHERE id = ? AND user_id = ? AND scope = ? AND content = ?`,
		next.ClusterID, next.Namespace, next.Content, next.UpdatedAt, next.ID, next.UserID, next.Scope, previousContent)
	if err != nil {
		return fmt.Errorf("update owned memory: %w", err)
	}
	if err := ensureAffected(res, "update owned memory"); errors.Is(err, ErrNotFound) {
		return ErrMemoryConflict
	} else {
		return err
	}
}

const (
	DefaultDiagnosticMemoryQuota = 200
	MaxDiagnosticMemoryQuota     = 1000
)

var ErrMemoryQuota = errors.New("diagnostic memory quota reached")

func (st *memoryStore) CreateWithQuota(ctx context.Context, memory *Memory) error {
	return st.s.WithTx(ctx, func(tx *Tx) error {
		// Serialize memory creation with other requests for this owner.
		if _, err := tx.tx.ExecContext(ctx, `UPDATE users SET id = id WHERE id = ?`, memory.UserID); err != nil {
			return err
		}
		limit := DefaultDiagnosticMemoryQuota
		var value string
		err := tx.tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE group_name = ? AND key_name = ?`, "memory", "max_items").Scan(&value)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if configured, err := strconv.Atoi(value); err == nil && configured > 0 {
			limit = min(configured, MaxDiagnosticMemoryQuota)
		}
		rows, err := tx.tx.QueryContext(ctx, `SELECT content FROM memories WHERE user_id = ? AND scope = ?`, memory.UserID, memory.Scope)
		if err != nil {
			return err
		}
		now := time.Now()
		active := 0
		for rows.Next() {
			var content string
			if err := rows.Scan(&content); err != nil {
				rows.Close()
				return err
			}
			var expiry struct {
				ExpiresAt *time.Time `json:"expiresAt"`
			}
			if json.Unmarshal([]byte(content), &expiry) != nil || expiry.ExpiresAt == nil || expiry.ExpiresAt.After(now) {
				active++
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if active >= limit {
			return ErrMemoryQuota
		}
		_, err = tx.tx.ExecContext(ctx, `
			INSERT INTO memories (id, user_id, scope, namespace, cluster_id, content, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			memory.ID, memory.UserID, memory.Scope, memory.Namespace, memory.ClusterID, memory.Content, now, memory.UpdatedAt)
		return mapErr(err, "create memory with quota")
	})
}

func (st *memoryStore) ListOwnedForCluster(ctx context.Context, ownerID, clusterID string) ([]*Memory, error) {
	rows, err := st.s.db.QueryContext(ctx, `
		SELECT id, user_id, scope, namespace, cluster_id, content, created_at, updated_at
		FROM memories WHERE user_id = ? AND cluster_id = ? ORDER BY updated_at DESC, id DESC`, ownerID, clusterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var memories []*Memory
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}

func (st *memoryStore) ListOwnedPage(ctx context.Context, ownerID, scope, after string, limit int) ([]*Memory, error) {
	rows, err := st.s.db.QueryContext(ctx, `
		SELECT id, user_id, scope, namespace, cluster_id, content, created_at, updated_at
		FROM memories WHERE user_id = ? AND scope = ? AND id > ? ORDER BY id LIMIT ?`,
		ownerID, scope, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	memories := make([]*Memory, 0, limit)
	for rows.Next() {
		memory, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, memory)
	}
	return memories, rows.Err()
}
