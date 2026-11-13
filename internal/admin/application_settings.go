package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"time"
)

// ApplicationSettings intentionally excludes listener, database, backup-directory and spool paths.
type ApplicationSettings struct {
	PublicURL               string   `json:"public_url"`
	LogLevel                string   `json:"log_level"`
	BackupEnabled           bool     `json:"backup_enabled"`
	BackupInterval          string   `json:"backup_interval"`
	BackupMaxAge            string   `json:"backup_max_age"`
	BackupMaxBackups        int      `json:"backup_max_backups"`
	GatewayEnabled          bool     `json:"gateway_enabled"`
	GatewayBaseURL          string   `json:"gateway_base_url"`
	GatewayMaxBodyBytes     int64    `json:"gateway_max_body_bytes"`
	GatewaySpoolBudgetBytes int64    `json:"gateway_spool_budget_bytes"`
	RuntimeEnabled          bool     `json:"runtime_management_enabled"`
	RuntimeBaseURL          string   `json:"runtime_management_base_url"`
	RuntimeAPIKeyEnv        string   `json:"runtime_management_api_key_env"`
	IPFallbackEnabled       bool     `json:"ip_fallback_enabled"`
	AnonymousEnabled        bool     `json:"anonymous_enabled"`
	AnonymousCIDRs          []string `json:"anonymous_cidrs"`
	AnonymousMaxIdentities  int64    `json:"anonymous_max_identities"`
	AnonymousSecretEnv      string   `json:"anonymous_secret_env"`
	TrustedProxyCIDRs       []string `json:"trusted_proxy_cidrs"`
	APIKeyEnabled           bool     `json:"api_key_enabled"`
}

func ValidateApplicationSettings(s ApplicationSettings) error {
	if s.PublicURL != "" {
		u, e := url.Parse(s.PublicURL)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid server.public_url")
		}
	}
	switch s.LogLevel {
	case "trace", "debug", "info", "warn", "error":
	default:
		return errors.New("invalid logging.level")
	}
	if s.BackupEnabled {
		d, e := time.ParseDuration(s.BackupInterval)
		age, ae := time.ParseDuration(s.BackupMaxAge)
		if e != nil || d <= 0 || ae != nil || age < 0 || s.BackupMaxBackups < 0 || age == 0 && s.BackupMaxBackups == 0 {
			return errors.New("invalid backup settings")
		}
	}
	if s.GatewayMaxBodyBytes < 0 || s.GatewaySpoolBudgetBytes < 0 {
		return errors.New("invalid gateway limits")
	}
	for _, v := range []struct {
		enabled bool
		raw     string
	}{{s.GatewayEnabled, s.GatewayBaseURL}, {s.RuntimeEnabled, s.RuntimeBaseURL}} {
		if v.enabled {
			u, e := url.Parse(v.raw)
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return errors.New("invalid HTTP service URL")
			}
		}
	}
	if s.AnonymousMaxIdentities < 0 {
		return errors.New("invalid auth.anonymous.max_identities")
	}
	for _, raw := range append(append([]string{}, s.AnonymousCIDRs...), s.TrustedProxyCIDRs...) {
		if _, _, e := net.ParseCIDR(raw); e != nil {
			return errors.New("invalid auth CIDR")
		}
	}
	return nil
}
func (s Store) SeedApplicationSettings(ctx context.Context, v ApplicationSettings) error {
	if e := ValidateApplicationSettings(v); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO application_settings(id,value_json) VALUES(1,?)`, b)
	return e
}
func (s Store) ApplicationSettings(ctx context.Context) (ApplicationSettings, error) {
	var v ApplicationSettings
	var b string
	e := s.DB.QueryRowContext(ctx, `SELECT value_json FROM application_settings WHERE id=1`).Scan(&b)
	if e == nil {
		e = json.Unmarshal([]byte(b), &v)
	}
	if e == nil {
		e = ValidateApplicationSettings(v)
	}
	return v, e
}
func (s Store) UpdateApplicationSettings(ctx context.Context, actor string, v ApplicationSettings) error {
	if e := ValidateApplicationSettings(v); e != nil {
		return e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `UPDATE application_settings SET value_json=?,updated_at=CURRENT_TIMESTAMP WHERE id=1`, b); e != nil {
		return e
	}
	if e = insertAudit(ctx, tx, actor, "settings.update", "settings", "application", Redacted{}, Redacted{}); e != nil {
		return e
	}
	return tx.Commit()
}
