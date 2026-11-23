package store

import (
	"context"
	"errors"
	"fmt"
)

func (st *memoryStore) ListOwned(ctx context.Context, ownerID string, limit int) ([]*Memory, error) {
	if ownerID == "" || limit < 1 || limit > 200 {
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
