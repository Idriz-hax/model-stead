package admin

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/google/uuid"
)

type User struct {
	ID                 string  `json:"id"`
	Username           string  `json:"username"`
	DisplayName        string  `json:"display_name"`
	Role               string  `json:"role"`
	Status             string  `json:"status"`
	SchedulingWeight   float64 `json:"scheduling_weight"`
	MaxQueued          int     `json:"max_queued"`
	MaxActiveContended int     `json:"max_active_contended"`
	MustChangePassword bool    `json:"must_change_password"`
}

func validRole(v string) bool { return v == "admin" || v == "user" }
func validOverrides(queue, active, capacity int) error {
	if queue < 0 || queue > 10000 {
		return errors.New("queue override must be 0-10000")
	}
	if active < 0 || active > 64 || active > capacity {
		return errors.New("active override exceeds capacity")
	}
	return nil
}
func validateUser(u User, capacity int) error {
	if err := ValidateUsername(u.Username); err != nil {
		return err
	}
	if err := ValidateDisplayName(u.DisplayName); err != nil {
		return err
	}
	if !validRole(u.Role) {
		return errors.New("invalid role")
	}
	if err := ValidateWeight(u.SchedulingWeight); err != nil {
		return err
	}
	return validOverrides(u.MaxQueued, u.MaxActiveContended, capacity)
}
func (s Store) capacity(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT global_capacity FROM scheduler_settings WHERE id=1`).Scan(&n)
	return n, err
}
func (s Store) CreateUser(ctx context.Context, actor string, u User, temporaryPassword string) (User, error) {
	capacity, err := s.capacity(ctx)
	if err != nil {
		return User{}, err
	}
	u.Username = strings.TrimSpace(u.Username)
	if err = validateUser(u, capacity); err != nil {
		return User{}, err
	}
	if len(temporaryPassword) < 12 || len(temporaryPassword) > 1024 {
		return User{}, errors.New("temporary password must be 12-1024 bytes")
	}
	hash, err := auth.HashPassword(temporaryPassword)
	if err != nil {
		return User{}, err
	}
	u.ID = uuid.NewString()
	u.Status = "active"
	u.MustChangePassword = true
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,display_name,password_hash,role,status,scheduling_weight,max_queued,max_active_contended,must_change_password) VALUES(?,?,?,?,?,'active',?,?,?,1)`, u.ID, u.Username, u.DisplayName, hash, u.Role, u.SchedulingWeight, u.MaxQueued, u.MaxActiveContended)
	if err != nil {
		return User{}, err
	}
	err = insertAudit(ctx, tx, actor, "user.create", "user", u.ID, Redacted{}, userRedacted(u))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}
func userRedacted(u User) Redacted {
	return Redacted{Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Status: u.Status, SchedulingWeight: &u.SchedulingWeight, MaxQueued: &u.MaxQueued, MaxActiveContended: &u.MaxActiveContended}
}
func (s Store) ListUsers(ctx context.Context) ([]User, error) {
	users, _, err := s.ListUsersPage(ctx, "", 100, 0)
	return users, err
}

func (s Store) GetUser(ctx context.Context, id string) (User, error) {
	var u User
	var must int
	err := s.DB.QueryRowContext(ctx, `SELECT id,username,display_name,role,status,scheduling_weight,max_queued,max_active_contended,must_change_password FROM users WHERE id=?`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.SchedulingWeight, &u.MaxQueued, &u.MaxActiveContended, &must)
	u.MustChangePassword = must != 0
	return u, err
}

func (s Store) ListUsersPage(ctx context.Context, search string, limit, offset int) ([]User, int, error) {
	limit, offset, err := pageBounds(limit, offset)
	if err != nil {
		return nil, 0, err
	}
	search = strings.TrimSpace(search)
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE (?='' OR instr(lower(username),lower(?))>0 OR instr(lower(display_name),lower(?))>0)`, search, search, search).Scan(&count); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,username,display_name,role,status,scheduling_weight,max_queued,max_active_contended,must_change_password FROM users WHERE (?='' OR instr(lower(username),lower(?))>0 OR instr(lower(display_name),lower(?))>0) ORDER BY username COLLATE NOCASE,id LIMIT ? OFFSET ?`, search, search, search, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var must int
		if err = rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.SchedulingWeight, &u.MaxQueued, &u.MaxActiveContended, &must); err != nil {
			return nil, 0, err
		}
		u.MustChangePassword = must != 0
		out = append(out, u)
	}
	return out, count, rows.Err()
}
func (s Store) UpdateUser(ctx context.Context, actor string, u User) error {
	capacity, err := s.capacity(ctx)
	if err != nil {
		return err
	}
	if err = validateUser(u, capacity); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old User
	err = tx.QueryRowContext(ctx, `SELECT id,username,display_name,role,status,scheduling_weight,max_queued,max_active_contended FROM users WHERE id=?`, u.ID).Scan(&old.ID, &old.Username, &old.DisplayName, &old.Role, &old.Status, &old.SchedulingWeight, &old.MaxQueued, &old.MaxActiveContended)
	if err != nil {
		return err
	}
	if old.Status == "active" && old.Role == "admin" && u.Role != "admin" {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return errors.New("cannot demote the last active administrator")
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE users SET username=?,display_name=?,role=?,scheduling_weight=?,max_queued=?,max_active_contended=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, u.Username, u.DisplayName, u.Role, u.SchedulingWeight, u.MaxQueued, u.MaxActiveContended, u.ID)
	if err != nil {
		return err
	}
	return auditedCommit(ctx, tx, actor, "user.update", u.ID, userRedacted(old), userRedacted(u))
}
func auditedCommit(ctx context.Context, tx *sql.Tx, actor, action, id string, before, after Redacted) error {
	if err := insertAudit(ctx, tx, actor, action, "user", id, before, after); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) setUserStatus(ctx context.Context, actor, id, status string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var u User
	err = tx.QueryRowContext(ctx, `SELECT id,username,display_name,role,status,scheduling_weight,max_queued,max_active_contended FROM users WHERE id=?`, id).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Status, &u.SchedulingWeight, &u.MaxQueued, &u.MaxActiveContended)
	if err != nil {
		return err
	}
	if status == "disabled" && u.Status != "disabled" && u.Role == "admin" {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND status='active'`).Scan(&n); err != nil {
			return err
		}
		if n <= 1 {
			return errors.New("cannot disable the last active administrator")
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET status=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, id); err != nil {
		return err
	}
	if status == "disabled" {
		if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET status='revoked',revoked_at=CURRENT_TIMESTAMP WHERE user_id=? AND status='active'`, id); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=CURRENT_TIMESTAMP WHERE user_id=? AND revoked_at IS NULL`, id); err != nil {
			return err
		}
	}
	before := userRedacted(u)
	u.Status = status
	action := "user.disable"
	if status == "active" {
		action = "user.enable"
	}
	return auditedCommit(ctx, tx, actor, action, id, before, userRedacted(u))
}
func (s Store) DisableUser(ctx context.Context, actor, id string) error {
	return s.setUserStatus(ctx, actor, id, "disabled")
}
func (s Store) EnableUser(ctx context.Context, actor, id string) error {
	return s.setUserStatus(ctx, actor, id, "active")
}
func (s Store) ResetTemporaryPassword(ctx context.Context, actor, id, password string) error {
	if len(password) < 12 || len(password) > 1024 {
		return errors.New("temporary password must be 12-1024 bytes")
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,must_change_password=1,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='active'`, hash, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=CURRENT_TIMESTAMP WHERE user_id=? AND revoked_at IS NULL`, id); err != nil {
		return err
	}
	return auditedCommit(ctx, tx, actor, "user.reset_password", id, Redacted{}, Redacted{})
}
