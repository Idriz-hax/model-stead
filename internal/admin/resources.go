package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Device struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id,omitempty"`
	Label      string  `json:"label"`
	IPAddress  string  `json:"ip_address"`
	Trusted    bool    `json:"trusted"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
	LastSeenAt *string `json:"last_seen_at"`
}

type APIKey struct {
	ID         string  `json:"id"`
	UserID     string  `json:"user_id"`
	Name       string  `json:"name"`
	KeyPrefix  string  `json:"key_prefix"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at"`
	RevokedAt  *string `json:"revoked_at"`
	LastUsedAt *string `json:"last_used_at"`
}

type nullableString struct{ value *string }

func (n *nullableString) Scan(src any) error {
	var v sql.NullString
	if err := v.Scan(src); err != nil {
		return err
	}
	if v.Valid {
		n.value = &v.String
	} else {
		n.value = nil
	}
	return nil
}

func (s Store) ListDevices(ctx context.Context) ([]Device, error) {
	return s.ListDevicesPage(ctx, 100, 0)
}

func pageBounds(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, errors.New("invalid pagination")
	}
	return limit, offset, nil
}

func (s Store) ListDevicesPage(ctx context.Context, limit, offset int) ([]Device, error) {
	limit, offset, err := pageBounds(limit, offset)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,COALESCE(user_id,''),label,ip_address,trusted,created_at,updated_at,last_seen_at FROM devices ORDER BY ip_address,id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var v Device
		var last nullableString
		if err := rows.Scan(&v.ID, &v.UserID, &v.Label, &v.IPAddress, &v.Trusted, &v.CreatedAt, &v.UpdatedAt, &last); err != nil {
			return nil, err
		}
		v.LastSeenAt = last.value
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s Store) GetDevice(ctx context.Context, id string) (Device, error) {
	var v Device
	var last nullableString
	err := s.DB.QueryRowContext(ctx, `SELECT id,COALESCE(user_id,''),label,ip_address,trusted,created_at,updated_at,last_seen_at FROM devices WHERE id=?`, id).Scan(&v.ID, &v.UserID, &v.Label, &v.IPAddress, &v.Trusted, &v.CreatedAt, &v.UpdatedAt, &last)
	v.LastSeenAt = last.value
	return v, err
}

func (s Store) CreateDevice(ctx context.Context, actor, userID, label, ip string) (Device, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Device{}, errors.New("invalid IP address")
	}
	ip = addr.Unmap().String()
	if strings.TrimSpace(label) == "" || len([]rune(label)) > 80 {
		return Device{}, errors.New("label must be 1-80 characters")
	}
	id := uuid.NewString()
	v := Device{ID: id, UserID: userID, Label: label, IPAddress: ip, Trusted: true}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO devices(id,user_id,label,ip_address,trusted) VALUES(?,?,?,?,1)`, id, nullString(userID), label, ip); err != nil {
		return Device{}, err
	}
	if err = insertAudit(ctx, tx, actor, "device.create", "device", id, Redacted{}, Redacted{Label: label, IPAddress: ip, Trusted: boolPtr(true)}); err != nil {
		return Device{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT created_at,updated_at FROM devices WHERE id=?`, id).Scan(&v.CreatedAt, &v.UpdatedAt); err != nil {
		return Device{}, err
	}
	return v, tx.Commit()
}

func (s Store) UpdateDevice(ctx context.Context, actor, id, userID, label, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return errors.New("invalid IP address")
	}
	ip = addr.Unmap().String()
	if strings.TrimSpace(label) == "" || len([]rune(label)) > 80 {
		return errors.New("label must be 1-80 characters")
	}
	return s.changeDevice(ctx, actor, id, "device.update", userID, label, ip, nil)
}
func (s Store) TrustDevice(ctx context.Context, actor, id string) error {
	return s.changeDevice(ctx, actor, id, "device.update", "", "", "", boolPtr(true))
}
func (s Store) UntrustDevice(ctx context.Context, actor, id string) error {
	return s.changeDevice(ctx, actor, id, "device.update", "", "", "", boolPtr(false))
}
func (s Store) changeDevice(ctx context.Context, actor, id, action, userID, label, ip string, trusted *bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old Device
	if err = tx.QueryRowContext(ctx, `SELECT id,COALESCE(user_id,''),label,ip_address,trusted FROM devices WHERE id=?`, id).Scan(&old.ID, &old.UserID, &old.Label, &old.IPAddress, &old.Trusted); err != nil {
		return err
	}
	next := old
	if trusted != nil {
		next.Trusted = *trusted
	} else {
		next.UserID = userID
		next.Label = label
		next.IPAddress = ip
	}
	if _, err = tx.ExecContext(ctx, `UPDATE devices SET user_id=?,label=?,ip_address=?,trusted=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, nullString(next.UserID), next.Label, next.IPAddress, next.Trusted, id); err != nil {
		return err
	}
	before := Redacted{Label: old.Label, IPAddress: old.IPAddress, Trusted: boolPtr(old.Trusted)}
	after := Redacted{Label: next.Label, IPAddress: next.IPAddress, Trusted: boolPtr(next.Trusted)}
	if err = insertAudit(ctx, tx, actor, action, "device", id, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	return s.ListAPIKeysPage(ctx, userID, 100, 0)
}

func (s Store) ListAPIKeysPage(ctx context.Context, userID string, limit, offset int) ([]APIKey, error) {
	limit, offset, err := pageBounds(limit, offset)
	if err != nil {
		return nil, err
	}
	query := `SELECT id,user_id,name,key_prefix,status,created_at,expires_at,revoked_at,last_used_at FROM api_keys`
	args := []any{}
	if userID != "" {
		query += ` WHERE user_id=?`
		args = append(args, userID)
	}
	query += ` ORDER BY created_at,id LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		var v APIKey
		var expires, revoked, used nullableString
		if err = rows.Scan(&v.ID, &v.UserID, &v.Name, &v.KeyPrefix, &v.Status, &v.CreatedAt, &expires, &revoked, &used); err != nil {
			return nil, err
		}
		v.ExpiresAt, v.RevokedAt, v.LastUsedAt = expires.value, revoked.value, used.value
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s Store) CreateAPIKey(ctx context.Context, actor, userID, name string, expiresAt *time.Time) (APIKey, string, error) {
	if len([]rune(name)) < 1 || len([]rune(name)) > 80 {
		return APIKey{}, "", errors.New("name must be 1-80 characters")
	}
	var expiry any
	if expiresAt != nil {
		if !expiresAt.After(time.Now()) {
			return APIKey{}, "", errors.New("expiry must be in the future")
		}
		expiry = expiresAt.UTC().Format(time.RFC3339Nano)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return APIKey{}, "", err
	}
	secret := "afr_v1_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	id := uuid.NewString()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return APIKey{}, "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO api_keys(id,user_id,name,key_prefix,key_hash,status,expires_at) SELECT ?,?,?,?,?,'active',? WHERE EXISTS(SELECT 1 FROM users WHERE id=? AND status='active')`, id, userID, name, secret[:14], hash, expiry, userID)
	if err != nil {
		return APIKey{}, "", err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return APIKey{}, "", err
	}
	if n != 1 {
		return APIKey{}, "", errors.New("owner is not active")
	}
	if err = insertAudit(ctx, tx, actor, "api_key.create", "api_key", id, Redacted{}, Redacted{Name: name, KeyPrefix: secret[:14]}); err != nil {
		return APIKey{}, "", err
	}
	var v APIKey
	var expires, revoked, used nullableString
	err = tx.QueryRowContext(ctx, `SELECT id,user_id,name,key_prefix,status,created_at,expires_at,revoked_at,last_used_at FROM api_keys WHERE id=?`, id).Scan(&v.ID, &v.UserID, &v.Name, &v.KeyPrefix, &v.Status, &v.CreatedAt, &expires, &revoked, &used)
	if err != nil {
		return APIKey{}, "", err
	}
	v.ExpiresAt, v.RevokedAt, v.LastUsedAt = expires.value, revoked.value, used.value
	return v, secret, tx.Commit()
}

func (s Store) RevokeAPIKey(ctx context.Context, actor, userID, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v APIKey
	if err = tx.QueryRowContext(ctx, `SELECT id,name,key_prefix FROM api_keys WHERE id=? AND user_id=? AND status='active'`, id, userID).Scan(&v.ID, &v.Name, &v.KeyPrefix); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET status='revoked',revoked_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, actor, "api_key.revoke", "api_key", id, Redacted{Name: v.Name, KeyPrefix: v.KeyPrefix}, Redacted{Name: v.Name, KeyPrefix: v.KeyPrefix}); err != nil {
		return err
	}
	return tx.Commit()
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func boolPtr(v bool) *bool { return &v }
