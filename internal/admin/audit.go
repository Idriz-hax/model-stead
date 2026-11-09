package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ListAuditFiltered returns newest-first audit records, with optional exact-match filters.
func (s Store) ListAuditFiltered(ctx context.Context, beforeID int64, limit int, actor, action, target string) ([]AuditEvent, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 {
		return nil, errors.New("page size must be between 1 and 100")
	}
	if beforeID < 0 {
		return nil, errors.New("invalid cursor")
	}
	for _, filter := range []string{actor, action, target} {
		if len(filter) > 128 {
			return nil, errors.New("audit filter exceeds 128 characters")
		}
	}
	conditions := make([]string, 0, 4)
	args := make([]any, 0, 5)
	if beforeID > 0 {
		conditions = append(conditions, "id < ?")
		args = append(args, beforeID)
	}
	for _, filter := range []struct{ column, value string }{{"actor_id", actor}, {"action", action}, {"target_type", target}} {
		if filter.value != "" {
			conditions = append(conditions, filter.column+" = ?")
			args = append(args, filter.value)
		}
	}
	query := `SELECT id,actor_id,action,target_type,target_id,before_json,after_json,created_at FROM audit_events`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
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
		var event AuditEvent
		var before, after string
		if err := rows.Scan(&event.ID, &event.ActorID, &event.Action, &event.TargetType, &event.TargetID, &before, &after, &event.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(before), &event.Before); err != nil {
			return nil, fmt.Errorf("decode audit before: %w", err)
		}
		if err := json.Unmarshal([]byte(after), &event.After); err != nil {
			return nil, fmt.Errorf("decode audit after: %w", err)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
