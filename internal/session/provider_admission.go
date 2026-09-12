package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProviderHealthTTL bounds the lifetime of a healthy observation.
const ProviderHealthTTL = 60 * time.Second

// ProviderRefreshGrace bounds how far past the publisher's published
// re-check deadline (next_check_at) a healthy record stays trusted. The
// publisher re-probes at next_check_at and the refreshed write lands within
// one probe interval, so short overruns are scheduling jitter, not a dead
// publisher. Measured 2026-09-12 (agent-forge-769p): the publisher wrote
// probed_at ~31-59 s behind the wall clock while probing healthy every
// 30-55 s (probed_at carries the upstream observation instant, and cached
// republications freeze it between real probes), so a strict probed_at TTL
// denied healthy providers for most of every probe cycle.
const ProviderRefreshGrace = 30 * time.Second

// ErrProviderUnavailable is a deferral, not a failed delivery or start attempt.
var ErrProviderUnavailable = errors.New("provider unavailable")

type providerHealthRecord struct {
	Provider    string  `json:"provider"`
	Status      string  `json:"status"`
	ProbedAt    float64 `json:"probed_at"`
	Reason      string  `json:"reason"`
	NextCheckAt float64 `json:"next_check_at"`
}

// ProviderAdmission is a transport decision, not a classifier of provider errors.
// Reason and NextCheckAt are supplied by the external policy owner.
type ProviderAdmission struct {
	Allowed     bool
	Observed    bool
	Reason      string
	NextCheckAt float64
}

// Err renders a denial through existing command and API error surfaces.
func (d ProviderAdmission) Err(provider string) error {
	if d.Allowed {
		return nil
	}
	if d.NextCheckAt > 0 {
		return fmt.Errorf("%w: %q: %s (next_check_at=%g)", ErrProviderUnavailable, provider, d.Reason, d.NextCheckAt)
	}
	return fmt.Errorf("%w: %q: %s", ErrProviderUnavailable, provider, d.Reason)
}

// ProviderHealthSnapshot is an immutable city policy/observation pair.
type ProviderHealthSnapshot struct {
	required    map[string]bool
	entries     map[string]providerHealthRecord
	policyError string
	healthError string
	now         float64
}

// LoadProviderHealthSnapshot reads the required-provider policy before health.
// Absent policy preserves optional-provider compatibility. Corrupt policy cannot
// turn enforcement off. Each lifecycle boundary reloads the files so restarting
// a controller never loses an externally published denial.
func LoadProviderHealthSnapshot(cityPath string, now time.Time) *ProviderHealthSnapshot {
	s := &ProviderHealthSnapshot{required: map[string]bool{}, entries: map[string]providerHealthRecord{}, now: float64(now.UnixNano()) / 1e9}
	if cityPath == "" {
		return s
	}
	data, err := os.ReadFile(filepath.Join(cityPath, ".gc/cache/provider-health-required.json"))
	if err == nil {
		var policy struct {
			SchemaVersion int      `json:"schema_version"`
			Providers     []string `json:"providers"`
		}
		if json.Unmarshal(data, &policy) != nil || policy.SchemaVersion != 1 || len(policy.Providers) == 0 {
			s.policyError = "required-provider policy is malformed"
		} else {
			for _, p := range policy.Providers {
				if strings.TrimSpace(p) == "" || strings.TrimSpace(p) != p || s.required[p] {
					s.policyError = "required-provider policy is malformed"
				}
				s.required[p] = true
			}
		}
	} else if !os.IsNotExist(err) {
		s.policyError = "required-provider policy is unreadable"
	}
	data, err = os.ReadFile(filepath.Join(cityPath, ".gc/cache/provider-health.json"))
	if err != nil {
		s.healthError = "health snapshot is unavailable"
		return s
	}
	var health struct {
		Providers []providerHealthRecord `json:"providers"`
	}
	if json.Unmarshal(data, &health) != nil || health.Providers == nil {
		s.healthError = "health snapshot is malformed"
		return s
	}
	for _, rec := range health.Providers {
		if _, exists := s.entries[rec.Provider]; exists {
			s.healthError = "health snapshot contains duplicate providers"
		}
		s.entries[rec.Provider] = rec
	}
	return s
}

// healthyEvidenceFresh reports whether a healthy record's evidence is fresh.
// The publisher's next_check_at is its refresh contract: a healthy record is
// trusted until the deadline plus one refresh grace, because probed_at
// carries the upstream observation instant (observed_at = probe time minus
// the proxy's quota-snapshot age, up to a minute old at publish) and cached
// republications freeze it between real probes. Records without a usable
// next_check_at keep the legacy probed_at TTL. A next_check_at implausibly
// far in the future (no live publisher writes one) is not trusted either.
func healthyEvidenceFresh(r providerHealthRecord, now float64) bool {
	if r.NextCheckAt > 0 && r.NextCheckAt <= now+ProviderHealthTTL.Seconds() {
		return now <= r.NextCheckAt+ProviderRefreshGrace.Seconds()
	}
	return r.ProbedAt > 0 && r.ProbedAt <= now && now-r.ProbedAt <= ProviderHealthTTL.Seconds()
}

// Check requires a fresh healthy observation for opted-in exact provider names.
// Unhealthy required records never expire into permission; next-check is only
// explanatory. Optional providers retain the legacy fail-open TTL behavior.
func (s *ProviderHealthSnapshot) Check(provider string) ProviderAdmission {
	if s == nil {
		return ProviderAdmission{Allowed: true}
	}
	deny := func(reason string) ProviderAdmission { return ProviderAdmission{Observed: true, Reason: reason} }
	if s.policyError != "" {
		return deny(s.policyError)
	}
	required := s.required[provider]
	if s.healthError != "" {
		if required {
			return deny(s.healthError)
		}
		return ProviderAdmission{Allowed: true}
	}
	r, exists := s.entries[provider]
	if !exists {
		if required {
			return deny("required provider has no health observation")
		}
		return ProviderAdmission{Allowed: true}
	}
	fresh := healthyEvidenceFresh(r, s.now)
	if !required && !fresh {
		return ProviderAdmission{Allowed: true}
	}
	if required && (r.ProbedAt <= 0 || r.ProbedAt > s.now || (r.Status != "healthy" && r.Status != "unhealthy") || r.NextCheckAt < 0) {
		return deny("required provider health observation is malformed")
	}
	if r.Status != "healthy" {
		reason := r.Reason
		if reason == "" {
			reason = "health observation is unhealthy"
		}
		return ProviderAdmission{Observed: true, Reason: reason, NextCheckAt: r.NextCheckAt}
	}
	if !fresh {
		return deny("required provider health observation is stale")
	}
	return ProviderAdmission{Allowed: true, Observed: true}
}

// HealthyProviders lists only confirmed fresh healthy providers.
func (s *ProviderHealthSnapshot) HealthyProviders() []string {
	if s == nil {
		return nil
	}
	var out []string
	for p := range s.entries {
		if d := s.Check(p); d.Allowed && d.Observed {
			out = append(out, p)
		}
	}
	return out
}

// CheckProviderAdmission checks the city policy at a lifecycle boundary.
func (m *Manager) CheckProviderAdmission(provider string) error {
	return LoadProviderHealthSnapshot(m.cityPath, time.Now()).Check(provider).Err(provider)
}
