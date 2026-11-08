package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// IdentityStore associates a verified external subject with an immutable user ID.
type IdentityStore interface {
	HasExternalIdentity(context.Context, string) (bool, error)
	ResolveIdentity(context.Context, string, string, *User) (*User, error)
	BindIdentity(context.Context, string, int64, string, string) error
	UnbindIdentity(context.Context, string, int64) error
}

func (st *userStore) HasExternalIdentity(ctx context.Context, id string) (bool, error) {
	var n int
	err := st.s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM user_identities WHERE user_id=?", id).Scan(&n)
	return n > 0, err
}
func identityKey(issuer, subject string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%s", len(issuer), issuer, subject)))
	return hex.EncodeToString(digest[:])
}

func (st *userStore) ResolveIdentity(ctx context.Context, issuer, subject string, candidate *User) (*User, error) {
	if issuer == "" || subject == "" || candidate == nil {
		return nil, errors.New("invalid external identity")
	}
	key := identityKey(issuer, subject)
	st.s.userSecurity.Lock()
	defer st.s.userSecurity.Unlock()
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, "SELECT user_id FROM user_identities WHERE identity_key=?", key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = candidate.ID
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, "INSERT INTO users(id,username,display_name,email,password_hash,role_id,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", id, candidate.Username, candidate.DisplayName, candidate.Email, "", candidate.RoleID, "active", now, now)
		if err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO user_identities(identity_key,user_id,issuer,subject,created_at) VALUES(?,?,?,?,?)", key, id, issuer, subject, now)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return st.Get(ctx, id)
}

func (st *userStore) BindIdentity(ctx context.Context, id string, version int64, issuer, subject string) error {
	if issuer == "" || subject == "" {
		return errors.New("invalid external identity")
	}
	return st.changeBinding(ctx, id, version, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO user_identities(identity_key,user_id,issuer,subject,created_at) VALUES(?,?,?,?,?)", identityKey(issuer, subject), id, issuer, subject, time.Now().UTC())
		return mapErr(err, "bind identity")
	})
}
func (st *userStore) UnbindIdentity(ctx context.Context, id string, version int64) error {
	return st.changeBinding(ctx, id, version, func(tx *sql.Tx) error {
		var protected int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users u JOIN roles r ON u.role_id=r.id WHERE u.id=? AND r.name='admin' AND NOT EXISTS(SELECT 1 FROM users v JOIN roles q ON q.id=v.role_id WHERE v.id<>u.id AND q.name='admin' AND v.status='active' AND v.password_hash<>'')", id).Scan(&protected); err != nil {
			return err
		}
		if protected != 0 {
			return ErrProtectedUser
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM user_identities WHERE user_id=?", id)
		if err != nil {
			return err
		}
		return ensureAffected(res, "unlink identity")
	})
}
func (st *userStore) changeBinding(ctx context.Context, id string, version int64, change func(*sql.Tx) error) error {
	st.s.userSecurity.Lock()
	defer st.s.userSecurity.Unlock()
	tx, err := st.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE users SET session_version=session_version+1 WHERE id=? AND session_version=? AND status='active' AND password_hash<>''", id, version)
	if err != nil {
		return err
	}
	if err = ensureAffected(res, "validate local identity"); err != nil {
		return err
	}
	if err = change(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func migrateV10(ctx context.Context, tx *sql.Tx, d dialect) error {
	t := tableSpec{name: "user_identities", cols: []column{{name: "identity_key", kind: colText, primary: true}, {name: "user_id", kind: colText, unique: true}, {name: "issuer", kind: colLong}, {name: "subject", kind: colLong}, {name: "created_at", kind: colTime}}}
	_, err := tx.ExecContext(ctx, d.createTable(t))
	return err
}
