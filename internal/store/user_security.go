package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrProtectedUser = errors.New("store: at least one active local administrator is required")
var ErrConcurrentUpdate = errors.New("store: user security state changed; reload and retry")

// updateUser atomically changes security state and its revocation version.
// The platform is single-instance; the store lock also serializes service
// instances sharing this store. The check and write use one SQL transaction.
func (st *userStore) updateUser(ctx context.Context, u *User) error {
	st.s.userSecurity.Lock()
	defer st.s.userSecurity.Unlock()
	tx, err := st.s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var password, roleID, status string
	var version int64
	err = tx.QueryRowContext(ctx, `SELECT password_hash, role_id, status, session_version FROM users WHERE id=?`, u.ID).Scan(&password, &roleID, &status, &version)
	if err != nil {
		return mapErr(err, "get security state")
	}
	if version != u.SessionVersion {
		return ErrConcurrentUpdate
	}
	var oldRole, newRole string
	if roleID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT name FROM roles WHERE id=?`, roleID).Scan(&oldRole); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if u.RoleID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT name FROM roles WHERE id=?`, u.RoleID).Scan(&newRole); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if oldRole == "admin" && status == "active" && password != "" && (newRole != "admin" || u.Status != "active" || u.PasswordHash == "") {
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u JOIN roles r ON r.id=u.role_id WHERE r.name='admin' AND u.status='active' AND u.password_hash<>'' AND u.id<>?`, u.ID).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			return ErrProtectedUser
		}
	}
	if password != u.PasswordHash || roleID != u.RoleID || status != u.Status {
		version++
	}
	u.UpdatedAt = time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE users SET username=?, display_name=?, password_hash=?, role_id=?, status=?, email=?, session_version=?, updated_at=? WHERE id=?`, u.Username, u.DisplayName, u.PasswordHash, u.RoleID, u.Status, u.Email, version, u.UpdatedAt, u.ID)
	if err != nil {
		return fmt.Errorf("update user: %w", err)
	}
	if err := ensureAffected(res, "update user"); err != nil {
		return err
	}
	if u.Status == "deleted" {
		if _, err := tx.ExecContext(ctx, "DELETE FROM authz_grants WHERE principal_type='user' AND principal_id=?", u.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	u.SessionVersion = version
	return nil
}
