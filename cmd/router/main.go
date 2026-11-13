package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"time"

	"github.com/Idriz-hax/model-stead/internal/admin"
	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/backup"
	"github.com/Idriz-hax/model-stead/internal/config"
	"github.com/Idriz-hax/model-stead/internal/database"
	"github.com/Idriz-hax/model-stead/internal/history"
	"github.com/Idriz-hax/model-stead/internal/scheduler"
	"github.com/Idriz-hax/model-stead/internal/server"
	"github.com/Idriz-hax/model-stead/web"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		target, err := healthcheckURL()
		if err != nil {
			os.Exit(1)
		}
		resp, err := http.Get(target)
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	configPath := os.Getenv("CONFIG_FILE")
	if configPath == "" {
		configPath = "config.yaml"
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Error().Err(err).Msg("configuration failed")
		os.Exit(1)
	}
	if len(os.Args) >= 2 && (os.Args[1] == "backup" || os.Args[1] == "restore") {
		if err := runOperatorCommand(os.Args[1:], cfg); err != nil {
			fmt.Fprintf(os.Stderr, "backup/restore command failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	level, err := zerolog.ParseLevel(cfg.Logging.Level)
	if err != nil {
		log.Error().Err(err).Msg("invalid logging level")
		os.Exit(1)
	}
	zerolog.SetGlobalLevel(level)
	var processLock *database.ProcessLock
	if !(len(os.Args) == 3 && os.Args[1] == "create-api-key") {
		processLock, err = database.AcquireProcessLock(cfg.Database.Path)
		if err != nil {
			log.Error().Msg("database is already in use")
			os.Exit(1)
		}
		defer processLock.Close()
	}
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		log.Error().Err(err).Msg("database initialization failed")
		os.Exit(1)
	}
	defer db.Close()
	settingsStore := admin.Store{DB: db}
	if err := settingsStore.SeedApplicationSettings(context.Background(), applicationSettings(cfg)); err != nil {
		log.Error().Err(err).Msg("application settings initialization failed")
		os.Exit(1)
	}
	appSettings, err := settingsStore.ApplicationSettings(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("persisted application settings invalid")
		os.Exit(1)
	}
	applyApplicationSettings(&cfg, appSettings)
	if err := config.ValidateApplication(cfg); err != nil {
		log.Error().Err(err).Msg("effective application settings invalid")
		os.Exit(1)
	}
	historyStore := &history.Store{DB: db}
	startupCtx, startupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := historyStore.RecoverInterrupted(startupCtx); err != nil {
		startupCancel()
		log.Error().Err(err).Msg("request history recovery failed")
		os.Exit(1)
	}
	if _, err := historyStore.PurgeBefore(startupCtx, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		startupCancel()
		log.Error().Err(err).Msg("request history purge failed")
		os.Exit(1)
	}
	startupCancel()
	adminStore := admin.Store{DB: db}
	maxWait, err := time.ParseDuration(cfg.Scheduler.MaxWait)
	if err != nil {
		log.Error().Err(err).Msg("invalid scheduler max wait")
		os.Exit(1)
	}
	grace, err := time.ParseDuration(cfg.Scheduler.CancellationGrace)
	if err != nil {
		log.Error().Err(err).Msg("invalid scheduler cancellation grace")
		os.Exit(1)
	}
	if err = adminStore.SeedSettings(context.Background(), admin.SchedulerSettings{Policy: cfg.Scheduler.Policy, GlobalCapacity: cfg.Scheduler.GlobalCapacity, DefaultWeight: cfg.Scheduler.DefaultWeight, MaxQueued: cfg.Scheduler.MaxQueued, MaxActiveContended: cfg.Scheduler.MaxActiveWhenContended, GlobalMaxQueued: cfg.Scheduler.GlobalMaxQueued, MaxWait: maxWait, WorkConserving: cfg.Scheduler.WorkConserving, CancellationGrace: grace}); err != nil {
		log.Error().Err(err).Msg("scheduler settings initialization failed")
		os.Exit(1)
	}
	persistedSettings, err := adminStore.Settings(context.Background())
	if err != nil {
		log.Error().Err(err).Msg("scheduler settings load failed")
		os.Exit(1)
	}
	if len(os.Args) == 3 && os.Args[1] == "create-api-key" {
		var id string
		if err = db.QueryRowContext(context.Background(), `SELECT id FROM users WHERE username=? AND status='active'`, os.Args[2]).Scan(&id); err != nil {
			fmt.Fprintln(os.Stderr, "active user not found")
			os.Exit(1)
		}
		secret, _, e := auth.CreateAPIKey(context.Background(), db, id, "provisioned")
		if e != nil {
			fmt.Fprintln(os.Stderr, "key creation failed")
			os.Exit(1)
		}
		fmt.Println(secret)
		return
	}
	setupManager, setupCode, err := server.NewSetupManager(context.Background(), db)
	if err != nil {
		log.Error().Err(err).Msg("admin setup initialization failed")
		os.Exit(1)
	}
	if !setupURLValid(cfg.Server.PublicURL, setupManager.Required()) {
		log.Error().Msg("server.public_url must be configured as an HTTPS origin while first-admin setup is pending")
		os.Exit(1)
	}
	if setupCode != "" {
		log.Warn().Str("code", setupCode).Msg("first-admin setup code; keep logs private until setup is complete")
	}
	trusted := make([]netip.Prefix, 0, len(cfg.Auth.TrustedProxyCIDRs))
	for _, v := range cfg.Auth.TrustedProxyCIDRs {
		p, e := netip.ParsePrefix(v)
		if e == nil {
			trusted = append(trusted, p)
		}
	}
	anonymousCIDRs := make([]netip.Prefix, 0, len(cfg.Auth.Anonymous.CIDRs))
	for _, v := range cfg.Auth.Anonymous.CIDRs {
		p, e := netip.ParsePrefix(v)
		if e == nil {
			anonymousCIDRs = append(anonymousCIDRs, p)
		}
	}
	resolver := auth.NewResolver(db, auth.ResolverConfig{
		DisableAPIKeys:         !cfg.Auth.APIKeyEnabled,
		IPFallbackEnabled:      cfg.Auth.IPFallbackEnabled,
		TrustedProxyPrefixes:   trusted,
		AnonymousEnabled:       cfg.Auth.Anonymous.Enabled,
		AllowedAnonymousCIDRs:  anonymousCIDRs,
		AnonymousSecret:        []byte(os.Getenv(cfg.Auth.Anonymous.SecretEnv)),
		MaxAnonymousIdentities: cfg.Auth.Anonymous.MaxIdentities,
	})
	log.Info().Str("address", cfg.Server.Address).Msg("starting server")
	grace = persistedSettings.CancellationGrace
	sched, err := scheduler.New(scheduler.Config{Capacity: persistedSettings.GlobalCapacity, DefaultWeight: persistedSettings.DefaultWeight, DefaultMaxQueued: persistedSettings.MaxQueued, DefaultMaxActiveWhenContended: persistedSettings.MaxActiveContended, GlobalMaxQueued: persistedSettings.GlobalMaxQueued, MaxWait: persistedSettings.MaxWait, WorkConserving: persistedSettings.WorkConserving, DisableWorkConserving: !persistedSettings.WorkConserving, CancellationGrace: grace})
	if err != nil {
		log.Error().Err(err).Msg("scheduler initialization failed")
		os.Exit(1)
	}
	httpServer := server.NewHTTPServer(cfg.Server.Address, server.NewWithSchedulerConfig(web.Files, db, server.GatewayConfig{Enabled: cfg.Gateway.Enabled, BaseURL: cfg.Gateway.BaseURL, APIKey: os.Getenv("LLAMA_SWAP_API_KEY"), SpoolDir: cfg.Gateway.SpoolDir, MaxBodyBytes: cfg.Gateway.MaxBodyBytes, SpoolBudgetBytes: cfg.Gateway.SpoolBudgetBytes, RuntimeEnabled: cfg.RuntimeManagement.Enabled, RuntimeBaseURL: cfg.RuntimeManagement.BaseURL, RuntimeAPIKey: os.Getenv(cfg.RuntimeManagement.APIKeyEnv)}, server.AuthConfig{PublicOrigin: cfg.Server.PublicURL, Resolver: resolver, Setup: setupManager}, server.SchedulerConfig{Scheduler: sched, CancellationGrace: grace, History: historyStore}))
	var stopBackup context.CancelFunc
	var backupDone chan struct{}
	if cfg.Backup.Enabled {
		if err := backup.PrepareDestination(cfg.Backup.Directory); err != nil {
			log.Error().Msg("backup destination unavailable")
			os.Exit(1)
		}
		interval, _ := time.ParseDuration(cfg.Backup.Interval)
		maxAge, _ := time.ParseDuration(cfg.Backup.MaxAge)
		backupCtx, cancel := context.WithCancel(context.Background())
		stopBackup, backupDone = cancel, make(chan struct{})
		go func() {
			defer close(backupDone)
			backup.RunScheduled(backupCtx, interval, func(ctx context.Context) error {
				if _, err := backup.OnlineBackupContext(ctx, db, cfg.Backup.Directory); err != nil {
					return err
				}
				return backup.ApplyRetention(cfg.Backup.Directory, maxAge, cfg.Backup.MaxBackups, time.Now().UTC())
			})
		}()
	}
	purgeCtx, stopPurge := context.WithCancel(context.Background())
	purgeDone := make(chan struct{})
	go func() {
		defer close(purgeDone)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-purgeCtx.Done():
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(purgeCtx, 5*time.Second)
				_, e := historyStore.PurgeBefore(ctx, time.Now().UTC().Add(-30*24*time.Hour))
				cancel()
				if e != nil {
					log.Error().Err(e).Msg("request history purge failed")
				}
			}
		}
	}()
	serveErr := httpServer.ListenAndServe()
	if stopBackup != nil {
		stopBackup()
		<-backupDone
	}
	stopPurge()
	<-purgeDone
	if serveErr != nil {
		log.Error().Err(serveErr).Msg("server stopped")
		os.Exit(1)
	}
}

func applicationSettings(c config.Config) admin.ApplicationSettings {
	return admin.ApplicationSettings{PublicURL: c.Server.PublicURL, LogLevel: c.Logging.Level, BackupEnabled: c.Backup.Enabled, BackupInterval: c.Backup.Interval, BackupMaxAge: c.Backup.MaxAge, BackupMaxBackups: c.Backup.MaxBackups, GatewayEnabled: c.Gateway.Enabled, GatewayBaseURL: c.Gateway.BaseURL, GatewayMaxBodyBytes: c.Gateway.MaxBodyBytes, GatewaySpoolBudgetBytes: c.Gateway.SpoolBudgetBytes, RuntimeEnabled: c.RuntimeManagement.Enabled, RuntimeBaseURL: c.RuntimeManagement.BaseURL, RuntimeAPIKeyEnv: c.RuntimeManagement.APIKeyEnv, IPFallbackEnabled: c.Auth.IPFallbackEnabled, AnonymousEnabled: c.Auth.Anonymous.Enabled, AnonymousCIDRs: c.Auth.Anonymous.CIDRs, AnonymousMaxIdentities: c.Auth.Anonymous.MaxIdentities, AnonymousSecretEnv: c.Auth.Anonymous.SecretEnv, TrustedProxyCIDRs: c.Auth.TrustedProxyCIDRs, APIKeyEnabled: c.Auth.APIKeyEnabled}
}
func applyApplicationSettings(c *config.Config, s admin.ApplicationSettings) {
	c.Server.PublicURL = s.PublicURL
	c.Logging.Level = s.LogLevel
	c.Backup.Enabled = s.BackupEnabled
	c.Backup.Interval = s.BackupInterval
	c.Backup.MaxAge = s.BackupMaxAge
	c.Backup.MaxBackups = s.BackupMaxBackups
	c.Gateway.Enabled = s.GatewayEnabled
	c.Gateway.BaseURL = s.GatewayBaseURL
	c.Gateway.MaxBodyBytes = s.GatewayMaxBodyBytes
	c.Gateway.SpoolBudgetBytes = s.GatewaySpoolBudgetBytes
	c.RuntimeManagement.Enabled = s.RuntimeEnabled
	c.RuntimeManagement.BaseURL = s.RuntimeBaseURL
	c.RuntimeManagement.APIKeyEnv = s.RuntimeAPIKeyEnv
	c.Auth.IPFallbackEnabled = s.IPFallbackEnabled
	c.Auth.Anonymous.Enabled = s.AnonymousEnabled
	c.Auth.Anonymous.CIDRs = s.AnonymousCIDRs
	c.Auth.Anonymous.MaxIdentities = s.AnonymousMaxIdentities
	c.Auth.Anonymous.SecretEnv = s.AnonymousSecretEnv
	c.Auth.TrustedProxyCIDRs = s.TrustedProxyCIDRs
	c.Auth.APIKeyEnabled = s.APIKeyEnabled
}

func healthcheckURL() (string, error) {
	port, err := config.ContainerPort()
	if err != nil {
		return "", err
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/ready", nil
}

func runOperatorCommand(args []string, cfg config.Config) error {
	switch {
	case len(args) == 1 && args[0] == "backup":
		if err := backup.PrepareDestination(cfg.Backup.Directory); err != nil {
			return err
		}
		db, err := database.OpenExisting(cfg.Database.Path)
		if err != nil {
			return err
		}
		defer db.Close()
		if _, err := backup.OnlineBackup(db, cfg.Backup.Directory); err != nil {
			return err
		}
		maxAge, err := time.ParseDuration(cfg.Backup.MaxAge)
		if err != nil {
			return err
		}
		return backup.ApplyRetention(cfg.Backup.Directory, maxAge, cfg.Backup.MaxBackups, time.Now().UTC())
	case len(args) == 2 && args[0] == "restore":
		return backup.Restore(args[1], cfg.Database.Path)
	default:
		return fmt.Errorf("invalid command")
	}
}

func setupURLValid(publicURL string, pending bool) bool {
	if !pending {
		return true
	}
	if config.ValidatePublicOrigin(publicURL) != nil {
		return false
	}
	u, _ := url.Parse(publicURL)
	return u.Scheme == "https"
}
