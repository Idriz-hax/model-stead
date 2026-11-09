package admin

import (
	"errors"
	"math"
	"net"
	"regexp"
	"time"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,64}$`)

func ValidateUsername(s string) error {
	if !usernamePattern.MatchString(s) {
		return errors.New("username must be 3-64 ASCII letters, digits, '.', '_' or '-'")
	}
	return nil
}
func ValidateDisplayName(s string) error {
	if len([]rune(s)) < 1 || len([]rune(s)) > 80 {
		return errors.New("display name must be 1-80 characters")
	}
	return nil
}
func ValidateWeight(v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < .1 || v > 100 {
		return errors.New("weight must be between 0.1 and 100")
	}
	return nil
}
func ValidateCapacity(v int) error {
	if v < 1 || v > 64 {
		return errors.New("capacity must be between 1 and 64")
	}
	return nil
}
func ValidateIP(s string) error {
	if net.ParseIP(s) == nil {
		return errors.New("invalid IP address")
	}
	return nil
}

type SchedulerSettings struct {
	Policy             string        `json:"policy"`
	GlobalCapacity     int           `json:"global_capacity"`
	DefaultWeight      float64       `json:"default_weight"`
	MaxQueued          int           `json:"max_queued"`
	MaxActiveContended int           `json:"max_active_when_contended"`
	GlobalMaxQueued    int           `json:"global_max_queued"`
	MaxWait            time.Duration `json:"max_wait"`
	WorkConserving     bool          `json:"work_conserving"`
	CancellationGrace  time.Duration `json:"cancellation_grace"`
}

func ValidateSettings(s SchedulerSettings) error {
	if s.Policy != "weighted_round_robin" {
		return errors.New("unsupported scheduler policy")
	}
	if err := ValidateCapacity(s.GlobalCapacity); err != nil {
		return err
	}
	if err := ValidateWeight(s.DefaultWeight); err != nil {
		return err
	}
	if s.MaxQueued < 1 || s.MaxQueued > 10000 || s.GlobalMaxQueued < 1 || s.GlobalMaxQueued > 10000 {
		return errors.New("queue limits must be between 1 and 10000")
	}
	if s.MaxActiveContended < 1 || s.MaxActiveContended > 64 {
		return errors.New("contended active limit must be between 1 and 64")
	}
	if s.MaxWait < time.Second || s.MaxWait > 24*time.Hour || s.CancellationGrace < time.Second || s.CancellationGrace > 5*time.Minute || s.MaxWait%time.Second != 0 || s.CancellationGrace%time.Second != 0 {
		return errors.New("scheduler durations are out of range")
	}
	return nil
}
