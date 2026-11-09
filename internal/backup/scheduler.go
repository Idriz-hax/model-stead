package backup

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// RunScheduled starts a ticker-driven backup loop; it does not run on startup.
func RunScheduled(ctx context.Context, interval time.Duration, run func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var mu sync.Mutex
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !mu.TryLock() {
				continue
			}
			if err := run(ctx); err != nil {
				log.Error().Str("error_type", fmt.Sprintf("%T", err)).Msg("scheduled backup failed")
			} else {
				log.Info().Msg("scheduled backup completed")
			}
			mu.Unlock()
		}
	}
}

// PrepareDestination validates/creates an enabled scheduler destination.
func PrepareDestination(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".router-backup-write-test-*")
	if err != nil {
		return err
	}
	path := probe.Name()
	if err := probe.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return os.Remove(path)
}
