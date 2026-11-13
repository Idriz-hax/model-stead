package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Scheduler Scheduler `yaml:"scheduler"`
	Backup    Backup    `yaml:"backup"`
	Server    struct {
		Address   string `yaml:"address"`
		PublicURL string `yaml:"public_url"`
	} `yaml:"server"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Logging struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
	Gateway struct {
		Enabled          bool   `yaml:"enabled"`
		BaseURL          string `yaml:"base_url"`
		SpoolDir         string `yaml:"spool_dir"`
		MaxBodyBytes     int64  `yaml:"max_body_bytes"`
		SpoolBudgetBytes int64  `yaml:"spool_budget_bytes"`
	} `yaml:"gateway"`
	RuntimeManagement struct {
		Enabled   bool   `yaml:"enabled"`
		BaseURL   string `yaml:"base_url"`
		APIKeyEnv string `yaml:"api_key_env"`
	} `yaml:"runtime_management"`
	Auth struct {
		IPFallbackEnabled bool `yaml:"ip_fallback_enabled"`
		Anonymous         struct {
			Enabled       bool     `yaml:"enabled"`
			CIDRs         []string `yaml:"cidrs"`
			SecretEnv     string   `yaml:"secret_env"`
			MaxIdentities int64    `yaml:"max_identities"`
		} `yaml:"anonymous"`
		TrustedProxyCIDRs []string `yaml:"trusted_proxy_cidrs"`
		APIKeyEnabled     bool     `yaml:"api_key_enabled"`
	} `yaml:"auth"`
}

type Backup struct {
	Enabled    bool   `yaml:"enabled"`
	Directory  string `yaml:"directory"`
	Interval   string `yaml:"interval"`
	MaxAge     string `yaml:"max_age"`
	MaxBackups int    `yaml:"max_backups"`
}

type Scheduler struct {
	Policy                 string  `yaml:"policy"`
	GlobalCapacity         int     `yaml:"global_capacity"`
	DefaultWeight          float64 `yaml:"default_weight"`
	MaxQueued              int     `yaml:"max_queued"`
	MaxActiveWhenContended int     `yaml:"max_active_when_contended"`
	MaxWait                string  `yaml:"max_wait"`
	GlobalMaxQueued        int     `yaml:"global_max_queued"`
	WorkConserving         bool    `yaml:"work_conserving"`
	CancellationGrace      string  `yaml:"cancellation_grace"`
}

func Load(path string) (Config, error) {
	c := Config{}
	c.Auth.APIKeyEnabled = true
	c.Backup = Backup{Interval: "24h", MaxAge: "720h", MaxBackups: 14}
	c.Scheduler = Scheduler{Policy: "weighted_round_robin", GlobalCapacity: 4, DefaultWeight: 1, MaxQueued: 100, MaxActiveWhenContended: 2, MaxWait: "30m", GlobalMaxQueued: 500, WorkConserving: true, CancellationGrace: "30s"}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	if err = yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if _, present := os.LookupEnv("ROUTER_CONTAINER_PORT"); present {
		port, portErr := ContainerPort()
		if portErr != nil {
			return c, portErr
		}
		host, _, splitErr := net.SplitHostPort(c.Server.Address)
		if splitErr != nil {
			return c, fmt.Errorf("server.address must be host:port when ROUTER_CONTAINER_PORT is set: %w", splitErr)
		}
		c.Server.Address = net.JoinHostPort(host, port)
	}
	if c.Scheduler.Policy != "weighted_round_robin" {
		return c, errors.New("scheduler.policy must be weighted_round_robin")
	}
	if c.Scheduler.GlobalCapacity < 1 || c.Scheduler.GlobalCapacity > 64 || math.IsNaN(c.Scheduler.DefaultWeight) || c.Scheduler.DefaultWeight < .1 || c.Scheduler.DefaultWeight > 100 || c.Scheduler.MaxActiveWhenContended < 1 || c.Scheduler.MaxActiveWhenContended > 64 || c.Scheduler.MaxActiveWhenContended > c.Scheduler.GlobalCapacity || c.Scheduler.MaxQueued < 1 || c.Scheduler.MaxQueued > 10000 || c.Scheduler.GlobalMaxQueued < 1 || c.Scheduler.GlobalMaxQueued > 10000 {
		return c, errors.New("scheduler capacity must be 1-64, weight 0.1-100, contended active 1-64, and queue limits 1-10000")
	}
	for field, raw := range map[string]string{"max_wait": c.Scheduler.MaxWait, "cancellation_grace": c.Scheduler.CancellationGrace} {
		d, err := time.ParseDuration(raw)
		min, max := time.Second, 24*time.Hour
		if field == "cancellation_grace" {
			max = 5 * time.Minute
		}
		if err != nil || d < min || d > max || d%time.Second != 0 {
			return c, fmt.Errorf("scheduler.%s duration is out of range", field)
		}
	}
	if c.Server.PublicURL != "" {
		if err := ValidatePublicOrigin(c.Server.PublicURL); err != nil {
			return c, err
		}
	}
	for _, raw := range append(append([]string{}, c.Auth.TrustedProxyCIDRs...), c.Auth.Anonymous.CIDRs...) {
		if _, _, err := net.ParseCIDR(raw); err != nil {
			return c, fmt.Errorf("invalid auth CIDR %q: %w", raw, err)
		}
	}
	if c.Auth.Anonymous.MaxIdentities < 0 {
		return c, errors.New("auth.anonymous.max_identities must be positive")
	}
	if c.Auth.Anonymous.Enabled {
		if c.Auth.Anonymous.MaxIdentities == 0 {
			c.Auth.Anonymous.MaxIdentities = 100_000
		}
		if c.Auth.Anonymous.SecretEnv == "" {
			return c, errors.New("auth.anonymous.secret_env is required when anonymous auth is enabled")
		}
		if len(os.Getenv(c.Auth.Anonymous.SecretEnv)) < 32 {
			return c, fmt.Errorf("auth.anonymous secret environment variable %s must contain at least 32 bytes", c.Auth.Anonymous.SecretEnv)
		}
	}
	if c.Server.Address == "" || c.Database.Path == "" || c.Logging.Level == "" {
		return c, errors.New("server.address, database.path, and logging.level are required")
	}
	if c.Backup.Enabled {
		interval, intervalErr := time.ParseDuration(c.Backup.Interval)
		maxAge, ageErr := time.ParseDuration(c.Backup.MaxAge)
		if strings.TrimSpace(c.Backup.Directory) == "" {
			return c, errors.New("backup.directory is required when backups are enabled")
		}
		if intervalErr != nil || interval <= 0 {
			return c, errors.New("backup.interval must be a positive duration")
		}
		if ageErr != nil || maxAge < 0 || c.Backup.MaxBackups < 0 {
			return c, errors.New("backup retention limits must be non-negative durations/counts")
		}
		if maxAge == 0 && c.Backup.MaxBackups == 0 {
			return c, errors.New("backup requires at least one enabled retention limit")
		}
	}
	if err := os.MkdirAll(filepath.Dir(c.Database.Path), 0700); err != nil {
		return c, fmt.Errorf("database directory: %w", err)
	}
	if _, err := parseLevel(c.Logging.Level); err != nil {
		return c, err
	}
	if c.Gateway.Enabled {
		if err := validateGatewayURL(c.Gateway.BaseURL); err != nil {
			return c, err
		}
		if c.Gateway.MaxBodyBytes < 0 || c.Gateway.SpoolBudgetBytes < 0 {
			return c, errors.New("gateway body limits must be non-negative")
		}
		if c.Gateway.SpoolDir == "" {
			c.Gateway.SpoolDir = filepath.Join(filepath.Dir(c.Database.Path), "spool")
		}
		if err := os.MkdirAll(c.Gateway.SpoolDir, 0700); err != nil {
			return c, fmt.Errorf("gateway spool directory: %w", err)
		}
		if err := os.Chmod(c.Gateway.SpoolDir, 0700); err != nil {
			return c, fmt.Errorf("gateway spool directory permissions: %w", err)
		}
	}
	if c.RuntimeManagement.Enabled {
		if err := validateRuntimeURL(c.RuntimeManagement.BaseURL); err != nil {
			return c, errors.New("runtime_management.base_url must be an HTTP(S) URL with host and no userinfo, query, or fragment")
		}
		if c.RuntimeManagement.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(c.RuntimeManagement.APIKeyEnv)) == "" {
			return c, fmt.Errorf("runtime_management API key environment variable %s is required", c.RuntimeManagement.APIKeyEnv)
		}
	}
	return c, nil
}

// ValidateApplication validates editable settings without creating directories or resolving secrets.
func ValidateApplication(c Config) error {
	if c.Server.PublicURL != "" {
		if err := ValidatePublicOrigin(c.Server.PublicURL); err != nil {
			return err
		}
	}
	if _, err := parseLevel(c.Logging.Level); err != nil {
		return err
	}
	if c.Backup.Enabled {
		d, e := time.ParseDuration(c.Backup.Interval)
		a, ae := time.ParseDuration(c.Backup.MaxAge)
		if strings.TrimSpace(c.Backup.Directory) == "" || e != nil || d <= 0 || ae != nil || a < 0 || c.Backup.MaxBackups < 0 || a == 0 && c.Backup.MaxBackups == 0 {
			return errors.New("invalid backup settings")
		}
	}
	if c.Gateway.Enabled {
		if err := validateGatewayURL(c.Gateway.BaseURL); err != nil {
			return err
		}
	}
	if c.Gateway.MaxBodyBytes < 0 || c.Gateway.SpoolBudgetBytes < 0 {
		return errors.New("gateway body limits must be non-negative")
	}
	if c.RuntimeManagement.Enabled {
		if err := validateRuntimeURL(c.RuntimeManagement.BaseURL); err != nil {
			return err
		}
		if c.RuntimeManagement.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(c.RuntimeManagement.APIKeyEnv)) == "" {
			return errors.New("runtime management API key environment variable is required")
		}
	}
	for _, v := range append(append([]string{}, c.Auth.TrustedProxyCIDRs...), c.Auth.Anonymous.CIDRs...) {
		if _, _, e := net.ParseCIDR(v); e != nil {
			return errors.New("invalid auth CIDR")
		}
	}
	if c.Auth.Anonymous.MaxIdentities < 0 {
		return errors.New("invalid anonymous identity limit")
	}
	if c.Auth.Anonymous.Enabled {
		if c.Auth.Anonymous.MaxIdentities == 0 {
			c.Auth.Anonymous.MaxIdentities = 100000
		}
		if c.Auth.Anonymous.SecretEnv == "" || len(os.Getenv(c.Auth.Anonymous.SecretEnv)) < 32 {
			return errors.New("anonymous secret environment variable must contain at least 32 bytes")
		}
	}
	return nil
}

func ContainerPort() (string, error) {
	raw, ok := os.LookupEnv("ROUTER_CONTAINER_PORT")
	if !ok {
		return "8080", nil
	}
	port, err := strconv.Atoi(raw)
	if raw == "" || err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("ROUTER_CONTAINER_PORT must be a decimal TCP port from 1 to 65535")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return "", fmt.Errorf("ROUTER_CONTAINER_PORT must be a decimal TCP port from 1 to 65535")
		}
	}
	return strconv.Itoa(port), nil
}

func ValidatePublicOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return fmt.Errorf("server.public_url must be an absolute HTTP(S) origin with host and no path, query, userinfo, or fragment")
	}
	return nil
}

func validateGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return fmt.Errorf("gateway.base_url must be an HTTP(S) URL with a hostname and no userinfo, query, or fragment")
	}
	return nil
}

func validateRuntimeURL(raw string) error {
	return validateGatewayURL(raw)
}

func parseLevel(level string) (int, error) {
	switch level {
	case "trace":
		return -1, nil
	case "debug":
		return 0, nil
	case "info":
		return 1, nil
	case "warn":
		return 2, nil
	case "error":
		return 3, nil
	default:
		return 0, fmt.Errorf("invalid logging.level %q", level)
	}
}
