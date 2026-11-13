package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidAndInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	db := filepath.Join(t.TempDir(), "router.db")
	if err := os.WriteFile(path, []byte("server:\n  address: ':8080'\ndatabase:\n  path: "+db+"\nlogging:\n  level: info\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if err := os.WriteFile(path, []byte("server: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected required field validation error")
	}
}

func TestLoadIgnoresLegacyAdminBootstrapKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	databasePath := filepath.Join(t.TempDir(), "router.db")
	data := "server:\n  address: ':8080'\ndatabase:\n  path: " + databasePath + "\nlogging:\n  level: info\nauth:\n  admin:\n    bootstrap_username: 123\n    display_name: false\n    password_env: NONEXISTENT_BOOTSTRAP_SECRET\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("legacy bootstrap keys should be ignored: %v", err)
	}
}

func TestContainerPortOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func() {
		t.Helper()
		if err := os.WriteFile(path, []byte("server:\n  address: '0.0.0.0:8080'\ndatabase:\n  path: "+filepath.Join(t.TempDir(), "router.db")+"\nlogging:\n  level: info\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, value, want string }{{"unset", "", "0.0.0.0:8080"}, {"custom", "9091", "0.0.0.0:9091"}} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unset" {
				previous, existed := os.LookupEnv("ROUTER_CONTAINER_PORT")
				t.Cleanup(func() {
					if existed {
						_ = os.Setenv("ROUTER_CONTAINER_PORT", previous)
					} else {
						_ = os.Unsetenv("ROUTER_CONTAINER_PORT")
					}
				})
				os.Unsetenv("ROUTER_CONTAINER_PORT")
			} else {
				t.Setenv("ROUTER_CONTAINER_PORT", tc.value)
			}
			write()
			c, err := Load(path)
			if err != nil || c.Server.Address != tc.want {
				t.Fatalf("address=%q err=%v", c.Server.Address, err)
			}
		})
	}
	for _, value := range []string{"", "abc", "0", "65536"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Setenv("ROUTER_CONTAINER_PORT", value)
			write()
			if _, err := Load(path); err == nil {
				t.Fatal("expected invalid port error")
			}
		})
	}
}

func TestRuntimeManagementDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	write := func(extra string) error {
		return os.WriteFile(path, []byte("server:\n  address: ':8080'\ndatabase:\n  path: "+filepath.Join(t.TempDir(), "router.db")+"\nlogging:\n  level: info\n"+extra), 0600)
	}
	if err := write(""); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.RuntimeManagement.Enabled || c.RuntimeManagement.APIKeyEnv != "" {
		t.Fatalf("unexpected defaults: %+v", c.RuntimeManagement)
	}
	for _, url := range []string{"https://runtime.example:9999", "http://localhost:9999", "http://127.0.0.1:9999", "http://[::1]:9999", "http://192.0.2.1:9999"} {
		if err := write("runtime_management:\n  enabled: true\n  base_url: " + url + "\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Errorf("valid runtime URL rejected: %s: %v", url, err)
		}
	}
	for _, url := range []string{"/relative", "ftp://host", "http:///path", "http://user:pass@host", "http://host?q=1", "http://host#fragment"} {
		if err := write("runtime_management:\n  enabled: true\n  base_url: " + url + "\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("invalid runtime URL accepted: %s", url)
		}
	}
	if err := write("runtime_management:\n  enabled: true\n  base_url: http://runtime.example\n  api_key_env: TEST_RUNTIME_KEY\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_RUNTIME_KEY", "")
	if _, err := Load(path); err == nil {
		t.Fatal("configured blank key should fail")
	}
	t.Setenv("TEST_RUNTIME_KEY", "key")
	if _, err := Load(path); err != nil {
		t.Fatalf("configured key: %v", err)
	}
}

func TestAuthConfigurationDefaultsAndValidation(t *testing.T) {
	load := func(extra string) (Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		data := "server:\n  address: ':8080'\ndatabase:\n  path: " + filepath.Join(t.TempDir(), "router.db") + "\nlogging:\n  level: info\n" + extra
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	c, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Auth.APIKeyEnabled || c.Auth.IPFallbackEnabled || c.Auth.Anonymous.Enabled || c.Gateway.Enabled {
		t.Fatalf("unsafe auth defaults: %+v", c)
	}
	for _, tc := range []struct{ name, extra string }{
		{"bad trusted CIDR", "auth:\n  trusted_proxy_cidrs: [not-a-cidr]\n"},
		{"bad anonymous CIDR", "auth:\n  anonymous:\n    enabled: true\n    secret_env: TEST_ANON_KEY\n    cidrs: [not-a-cidr]\n"},
		{"missing secret variable name", "auth:\n  anonymous:\n    enabled: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(tc.extra); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if err := ValidatePublicOrigin("https://example.com/path"); err == nil {
		t.Fatal("expected invalid public origin")
	}
	if _, err := load("auth:\n  anonymous:\n    max_identities: -1\n"); err == nil {
		t.Fatal("expected invalid anonymous identity capacity")
	}
	t.Setenv("TEST_ANON_KEY", "persistent-test-secret-that-is-at-least-32-bytes")
	if _, err := load("auth:\n  anonymous:\n    enabled: true\n    secret_env: TEST_ANON_KEY\n    cidrs: [192.0.2.0/24]\n  trusted_proxy_cidrs: [127.0.0.1/32]\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_ANON_KEY", "")
	if _, err := load("auth:\n  anonymous:\n    enabled: true\n    secret_env: TEST_ANON_KEY\n"); err == nil || !strings.Contains(err.Error(), "TEST_ANON_KEY") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
}

func TestGatewayRejectsInvalidUpstreamURLs(t *testing.T) {
	for _, base := range []string{"", "ftp://example.com", "http:///path", "http://user:pass@example.com", "http://example.com/?q=x", "http://example.com/#frag", "//example.com"} {
		t.Run(base, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			data := "server:\n  address: ':8080'\ndatabase:\n  path: " + filepath.Join(t.TempDir(), "router.db") + "\nlogging:\n  level: info\ngateway:\n  enabled: true\n  base_url: '" + base + "'\n"
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("accepted invalid gateway URL %q", base)
			}
		})
	}
}

func TestSchedulerDefaultsAndValidation(t *testing.T) {
	load := func(extra string) (Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		data := "server:\n  address: ':8080'\ndatabase:\n  path: " + filepath.Join(t.TempDir(), "router.db") + "\nlogging:\n  level: info\n" + extra
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	c, err := load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Scheduler.Policy != "weighted_round_robin" || c.Scheduler.GlobalCapacity != 4 || c.Scheduler.DefaultWeight != 1 || c.Scheduler.MaxQueued != 100 || c.Scheduler.MaxActiveWhenContended != 2 || c.Scheduler.MaxWait != "30m" || c.Scheduler.GlobalMaxQueued != 500 || !c.Scheduler.WorkConserving || c.Scheduler.CancellationGrace != "30s" {
		t.Fatalf("unexpected scheduler defaults: %+v", c.Scheduler)
	}
	if fractional, err := load("scheduler:\n  default_weight: 1.5\n"); err != nil || fractional.Scheduler.DefaultWeight != 1.5 {
		t.Fatalf("fractional scheduler weight not preserved: config=%+v err=%v", fractional.Scheduler, err)
	}
	for _, extra := range []string{
		"scheduler:\n  policy: fifo\n", "scheduler:\n  global_capacity: 0\n", "scheduler:\n  default_weight: 0\n",
		"scheduler:\n  max_queued: 0\n", "scheduler:\n  max_active_when_contended: 0\n", "scheduler:\n  global_max_queued: 0\n",
		"scheduler:\n  max_wait: 0s\n", "scheduler:\n  max_wait: nope\n", "scheduler:\n  cancellation_grace: -1s\n",
		"scheduler:\n  max_wait: 1500ms\n", "scheduler:\n  cancellation_grace: 1500ms\n",
		"scheduler:\n  global_capacity: 65\n", "scheduler:\n  default_weight: 100.1\n", "scheduler:\n  max_queued: 10001\n",
		"scheduler:\n  max_active_when_contended: 65\n", "scheduler:\n  global_max_queued: 10001\n", "scheduler:\n  max_wait: 24h1s\n", "scheduler:\n  cancellation_grace: 5m1s\n",
		"scheduler:\n  global_capacity: 1\n  max_active_when_contended: 2\n", "scheduler:\n  default_weight: .nan\n",
	} {
		if _, err := load(extra); err == nil {
			t.Errorf("accepted invalid scheduler config %q", extra)
		}
	}
}

func TestBackupConfig(t *testing.T) {
	load := func(extra string) (Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		data := "server:\n  address: ':8080'\ndatabase:\n  path: " + filepath.Join(t.TempDir(), "router.db") + "\nlogging:\n  level: info\n" + extra
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	c, err := load("")
	if err != nil || c.Backup.Enabled {
		t.Fatalf("omitted backup must be disabled: %+v, %v", c.Backup, err)
	}
	c, err = load("backup:\n  enabled: true\n  directory: /backups\n")
	if err != nil || !c.Backup.Enabled || c.Backup.Interval != "24h" || c.Backup.MaxAge != "720h" || c.Backup.MaxBackups != 14 {
		t.Fatalf("unexpected enabled defaults: %+v, %v", c.Backup, err)
	}
	c, err = load("backup:\n  enabled: true\n  directory: /custom\n  interval: 1h\n  max_age: 0s\n")
	if err != nil || c.Backup.Directory != "/custom" || c.Backup.Interval != "1h" {
		t.Fatalf("custom backup config: %+v, %v", c.Backup, err)
	}
	c, err = load("backup:\n  enabled: true\n  directory: /backups\n  max_backups: 0\n")
	if err != nil || c.Backup.MaxBackups != 0 || c.Backup.MaxAge != "720h" {
		t.Fatalf("count-only retention config: %+v, %v", c.Backup, err)
	}
	for _, extra := range []string{
		"backup:\n  enabled: true\n  interval: 1h\n",
		"backup:\n  enabled: true\n  directory: /backups\n  interval: 0s\n",
		"backup:\n  enabled: true\n  directory: /backups\n  interval: nope\n",
		"backup:\n  enabled: true\n  directory: /backups\n  max_age: -1h\n",
		"backup:\n  enabled: true\n  directory: /backups\n  max_backups: -1\n",
		"backup:\n  enabled: true\n  directory: /backups\n  max_age: 0s\n  max_backups: 0\n",
	} {
		if _, err := load(extra); err == nil {
			t.Errorf("accepted invalid backup config: %q", extra)
		}
	}
}
