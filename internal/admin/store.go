package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Store struct{ DB *sql.DB }

// Redacted contains only fields approved for audit storage; secrets and arbitrary payloads cannot enter it.
type Redacted struct {
	Username           string             `json:"username,omitempty"`
	DisplayName        string             `json:"display_name,omitempty"`
	Role               string             `json:"role,omitempty"`
	Status             string             `json:"status,omitempty"`
	SchedulingWeight   *float64           `json:"scheduling_weight,omitempty"`
	MaxQueued          *int               `json:"max_queued,omitempty"`
	MaxActiveContended *int               `json:"max_active_contended,omitempty"`
	Label              string             `json:"label,omitempty"`
	IPAddress          string             `json:"ip_address,omitempty"`
	Trusted            *bool              `json:"trusted,omitempty"`
	Name               string             `json:"name,omitempty"`
	KeyPrefix          string             `json:"key_prefix,omitempty"`
	Scheduler          *SchedulerSettings `json:"scheduler,omitempty"`
}
type AuditEvent struct {
	ID         int64    `json:"id"`
	ActorID    string   `json:"actor_id"`
	Action     string   `json:"action"`
	TargetType string   `json:"target_type"`
	TargetID   string   `json:"target_id"`
	Before     Redacted `json:"before"`
	After      Redacted `json:"after"`
	CreatedAt  string   `json:"created_at"`
}

func (s Store) SeedSettings(ctx context.Context, settings SchedulerSettings) error {
	if err := ValidateSettings(settings); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO scheduler_settings (id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES (1,?,?,?,?,?,?,?,?,?)`, settings.Policy, settings.GlobalCapacity, settings.DefaultWeight, settings.MaxQueued, settings.MaxActiveContended, settings.GlobalMaxQueued, int64(settings.MaxWait.Seconds()), settings.WorkConserving, int64(settings.CancellationGrace.Seconds()))
	return err
}
func (s Store) Settings(ctx context.Context) (SchedulerSettings, error) {
	var v SchedulerSettings
	var wait, grace int64
	err := s.DB.QueryRowContext(ctx, `SELECT policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds FROM scheduler_settings WHERE id=1`).Scan(&v.Policy, &v.GlobalCapacity, &v.DefaultWeight, &v.MaxQueued, &v.MaxActiveContended, &v.GlobalMaxQueued, &wait, &v.WorkConserving, &grace)
	v.MaxWait = durationSeconds(wait)
	v.CancellationGrace = durationSeconds(grace)
	return v, err
}
func durationSeconds(v int64) time.Duration { return time.Duration(v) * time.Second }

func (s Store) UpdateSettings(ctx context.Context, actor string, next SchedulerSettings) error {
	if err := ValidateSettings(next); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous SchedulerSettings
	var wait, grace int64
	err = tx.QueryRowContext(ctx, `SELECT policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds FROM scheduler_settings WHERE id=1`).Scan(&previous.Policy, &previous.GlobalCapacity, &previous.DefaultWeight, &previous.MaxQueued, &previous.MaxActiveContended, &previous.GlobalMaxQueued, &wait, &previous.WorkConserving, &grace)
	if err != nil {
		return err
	}
	previous.MaxWait = durationSeconds(wait)
	previous.CancellationGrace = durationSeconds(grace)
	if _, err = tx.ExecContext(ctx, `UPDATE scheduler_settings SET policy=?,global_capacity=?,default_weight=?,max_queued=?,max_active_contended=?,global_max_queued=?,max_wait_seconds=?,work_conserving=?,cancellation_grace_seconds=?,updated_at=CURRENT_TIMESTAMP WHERE id=1`, next.Policy, next.GlobalCapacity, next.DefaultWeight, next.MaxQueued, next.MaxActiveContended, next.GlobalMaxQueued, int64(next.MaxWait.Seconds()), next.WorkConserving, int64(next.CancellationGrace.Seconds())); err != nil {
		return err
	}
	if err := insertAudit(ctx, tx, actor, "scheduler.update", "scheduler", "1", Redacted{Scheduler: &previous}, Redacted{Scheduler: &next}); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditedExec performs a single trusted, parameterized mutation and its allowlisted audit event atomically.
func (s Store) AuditedExec(ctx context.Context, actor, action, targetType, targetID, statement string, args []any, before, after Redacted) error {
	if !validAuditAction(action) || !validTarget(targetType) {
		return errors.New("invalid audit action or target")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, statement, args...); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, actor, action, targetType, targetID, before, after); err != nil {
		return err
	}
	return tx.Commit()
}
func validAuditAction(v string) bool {
	return v == "admin.bootstrap" || v == "user.create" || v == "user.update" || v == "user.disable" || v == "user.enable" || v == "user.reset_password" || v == "device.create" || v == "device.update" || v == "api_key.create" || v == "api_key.revoke" || v == "scheduler.update" || v == "request.cancel"
}
func validTarget(v string) bool {
	return v == "user" || v == "device" || v == "api_key" || v == "scheduler" || v == "request"
}
func insertAudit(ctx context.Context, tx *sql.Tx, actor, action, targetType, targetID string, before, after Redacted) error {
	if !validAuditAction(action) || !validTarget(targetType) || len(targetID) == 0 || len(targetID) > 128 {
		return errors.New("invalid audit event")
	}
	b, e := json.Marshal(before)
	if e != nil {
		return e
	}
	a, e := json.Marshal(after)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO audit_events(actor_id,action,target_type,target_id,before_json,after_json) VALUES(?,?,?,?,?,?)`, actor, action, targetType, targetID, string(b), string(a))
	return e
}
func (s Store) ListAudit(ctx context.Context, beforeID int64, limit int) ([]AuditEvent, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("page size must be between 1 and 100")
	}
	if beforeID < 0 {
		return nil, errors.New("invalid cursor")
	}
	query := `SELECT id,actor_id,action,target_type,target_id,before_json,after_json,created_at FROM audit_events`
	args := []any{}
	if beforeID > 0 {
		query += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var v AuditEvent
		var b, a string
		if err = rows.Scan(&v.ID, &v.ActorID, &v.Action, &v.TargetType, &v.TargetID, &b, &a, &v.CreatedAt); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &v.Before); err != nil {
			return nil, fmt.Errorf("decode audit before: %w", err)
		}
		if err = json.Unmarshal([]byte(a), &v.After); err != nil {
			return nil, fmt.Errorf("decode audit after: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
