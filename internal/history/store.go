package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var endpoints = map[string]bool{"/v1/chat/completions": true, "/v1/completions": true, "/v1/responses": true, "/v1/messages": true, "/v1/embeddings": true}
var outcomes = map[string]bool{"queued": true, "active": true, "completed": true, "rejected": true, "timed_out": true, "cancelled": true, "upstream_error": true, "interrupted": true}

type Store struct{ DB *sql.DB }
type Record struct {
	RequestID, UserID, Endpoint            string
	RequestedModel                         *string
	EnqueuedAt                             time.Time
	StartedAt, CompletedAt                 *time.Time
	Outcome                                string
	ResponseStatus                         *int
	InputTokens, OutputTokens, TotalTokens *int64
	UsageStatus                            string
}
type Lifecycle struct {
	StartedAt, CompletedAt                 *time.Time
	Outcome                                string
	ResponseStatus                         *int
	InputTokens, OutputTokens, TotalTokens *int64
	UsageStatus                            string
}
type Query struct {
	Limit, Offset             int
	From, To                  *time.Time
	Endpoint, Outcome, Search string
}
type DailyUsage struct {
	Day                       string
	InputTokens, OutputTokens *int64
	Requests, Incomplete      int64
}

func ms(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMilli()
}
func (s *Store) Create(ctx context.Context, r Record) error {
	if r.RequestID == "" || r.UserID == "" || !endpoints[r.Endpoint] || !outcomes[r.Outcome] || !usageValid(r.UsageStatus) {
		return errors.New("invalid history record")
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO request_history(request_id,user_id,endpoint,requested_model,enqueued_at_ms,started_at_ms,completed_at_ms,outcome,response_status,input_tokens,output_tokens,total_tokens,usage_status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.RequestID, r.UserID, r.Endpoint, r.RequestedModel, r.EnqueuedAt.UTC().UnixMilli(), ms(r.StartedAt), ms(r.CompletedAt), r.Outcome, r.ResponseStatus, r.InputTokens, r.OutputTokens, r.TotalTokens, r.UsageStatus)
	return err
}
func usageValid(v string) bool { return v == "reported" || v == "partial" || v == "unavailable" }
func (s *Store) UpdateLifecycle(ctx context.Context, userID, requestID string, v Lifecycle) error {
	if !outcomes[v.Outcome] {
		return errors.New("invalid outcome")
	}
	if v.UsageStatus != "" && !usageValid(v.UsageStatus) {
		return errors.New("invalid usage status")
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE request_history SET outcome=?,started_at_ms=COALESCE(?,started_at_ms),completed_at_ms=COALESCE(?,completed_at_ms),response_status=COALESCE(?,response_status),input_tokens=COALESCE(?,input_tokens),output_tokens=COALESCE(?,output_tokens),total_tokens=COALESCE(?,total_tokens),usage_status=COALESCE(NULLIF(?,''),usage_status) WHERE user_id=? AND request_id=?`, v.Outcome, ms(v.StartedAt), ms(v.CompletedAt), v.ResponseStatus, v.InputTokens, v.OutputTokens, v.TotalTokens, v.UsageStatus, userID, requestID)
	if err == nil {
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("update history lifecycle rows affected: %w", err)
		}
		if n == 0 {
			return sql.ErrNoRows
		}
	}
	return err
}
func (s *Store) ListPage(ctx context.Context, userID string, q Query) ([]Record, int, error) {
	if q.Limit == 0 {
		q.Limit = 50
	}
	if q.Limit < 1 || q.Limit > 100 || q.Offset < 0 {
		return nil, 0, errors.New("invalid page bounds")
	}
	if q.Endpoint != "" && !endpoints[q.Endpoint] || q.Outcome != "" && !outcomes[q.Outcome] || len(q.Search) > 128 {
		return nil, 0, errors.New("invalid filter")
	}
	if q.From != nil && q.To != nil && (!q.To.After(*q.From) || q.To.Sub(*q.From) > 30*24*time.Hour) {
		return nil, 0, errors.New("invalid time range")
	}
	now := time.Now().UTC()
	oldest := now.Add(-30 * 24 * time.Hour)
	if q.From != nil && (q.From.Before(oldest) || q.From.After(now)) || q.To != nil && (q.To.Before(oldest) || q.To.After(now)) {
		return nil, 0, errors.New("time range outside retained window")
	}
	where := []string{"user_id=?"}
	args := []any{userID}
	add := func(sql string, v any) { where = append(where, sql); args = append(args, v) }
	if q.From != nil {
		add("enqueued_at_ms>=?", q.From.UTC().UnixMilli())
	}
	if q.To != nil {
		add("enqueued_at_ms<?", q.To.UTC().UnixMilli())
	}
	if q.Endpoint != "" {
		add("endpoint=?", q.Endpoint)
	}
	if q.Outcome != "" {
		add("outcome=?", q.Outcome)
	}
	if q.Search != "" {
		add("requested_model LIKE ?", "%"+strings.ReplaceAll(strings.ReplaceAll(q.Search, "%", "\\%"), "_", "\\_")+"%")
	}
	clause := strings.Join(where, " AND ")
	var total int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM request_history WHERE "+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := s.DB.QueryContext(ctx, `SELECT request_id,user_id,endpoint,requested_model,enqueued_at_ms,started_at_ms,completed_at_ms,outcome,response_status,input_tokens,output_tokens,total_tokens,usage_status FROM request_history WHERE `+clause+` ORDER BY enqueued_at_ms DESC,request_id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var model sql.NullString
		var enq int64
		var started, completed sql.NullInt64
		var status sql.NullInt64
		var in, outtok, totalTok sql.NullInt64
		if err = rows.Scan(&r.RequestID, &r.UserID, &r.Endpoint, &model, &enq, &started, &completed, &r.Outcome, &status, &in, &outtok, &totalTok, &r.UsageStatus); err != nil {
			return nil, 0, err
		}
		r.EnqueuedAt = time.UnixMilli(enq).UTC()
		if model.Valid {
			r.RequestedModel = &model.String
		}
		r.StartedAt = timePtr(started)
		r.CompletedAt = timePtr(completed)
		if status.Valid {
			x := int(status.Int64)
			r.ResponseStatus = &x
		}
		r.InputTokens = intPtr(in)
		r.OutputTokens = intPtr(outtok)
		r.TotalTokens = intPtr(totalTok)
		out = append(out, r)
	}
	return out, total, rows.Err()
}
func timePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}
func intPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
func (s *Store) Usage(ctx context.Context, userID string, from, to time.Time) ([]DailyUsage, error) {
	now := time.Now().UTC()
	oldest := now.Add(-30 * 24 * time.Hour)
	if !to.After(from) || to.Sub(from) > 30*24*time.Hour || from.Before(oldest) || to.Before(oldest) || from.After(now) || to.After(now) {
		return nil, errors.New("invalid time range")
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT date(enqueued_at_ms/1000,'unixepoch'),sum(input_tokens),sum(output_tokens),count(*),sum(CASE WHEN usage_status IN ('partial','unavailable') THEN 1 ELSE 0 END) FROM request_history WHERE user_id=? AND enqueued_at_ms>=? AND enqueued_at_ms<? GROUP BY date(enqueued_at_ms/1000,'unixepoch') ORDER BY 1`, userID, from.UTC().UnixMilli(), to.UTC().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailyUsage{}
	for rows.Next() {
		var v DailyUsage
		var input, output sql.NullInt64
		if err = rows.Scan(&v.Day, &input, &output, &v.Requests, &v.Incomplete); err != nil {
			return nil, err
		}
		v.InputTokens = intPtr(input)
		v.OutputTokens = intPtr(output)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) RecoverInterrupted(ctx context.Context) (int64, error) {
	r, e := s.DB.ExecContext(ctx, `UPDATE request_history SET outcome='interrupted' WHERE outcome IN ('queued','active')`)
	if e != nil {
		return 0, fmt.Errorf("recover interrupted history: %w", e)
	}
	n, e := r.RowsAffected()
	if e != nil {
		return 0, fmt.Errorf("recover interrupted history rows affected: %w", e)
	}
	return n, nil
}
func (s *Store) PurgeBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	r, e := s.DB.ExecContext(ctx, `DELETE FROM request_history WHERE enqueued_at_ms<?`, cutoff.UTC().UnixMilli())
	if e != nil {
		return 0, fmt.Errorf("purge history: %w", e)
	}
	n, e := r.RowsAffected()
	if e != nil {
		return 0, fmt.Errorf("purge history rows affected: %w", e)
	}
	return n, nil
}
