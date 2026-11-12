package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// maxResetAfterSeconds is the longest relative reset a time.Duration can hold.
	maxResetAfterSeconds = float64(math.MaxInt64) / float64(time.Second)
	// maxResetUnixSeconds is the end of year 9999, past any real reset.
	maxResetUnixSeconds = 253402300799
)

// soonestResetProbeInterval bounds how often a credential without a usable
// quota observation is sent a request to learn its reset time.
const soonestResetProbeInterval = time.Hour

type quotaProbeDisabledKey struct{}

// withoutQuotaProbe keeps selection-only and count-token requests from
// consuming an observation opportunity that they cannot complete.
func withoutQuotaProbe(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, quotaProbeDisabledKey{}, true)
}

// SoonestResetSelector selects the available credential whose longest quota
// window resets soonest, so allowance that would otherwise expire unused at
// the next reset is spent first. Reset times come from the passive quota
// snapshot (Quota.Signals) recorded from upstream response headers.
//
// A Codex or Claude credential without a future reset (for example after a
// restart or a window rollover) is picked once per probe interval so its reset is
// learned; otherwise the first observed credential would win every pick and
// the strategy would degrade to fill-first. A newly expired window can be
// probed immediately, even if it was sampled shortly before its reset. Between
// probes, unknown and expired windows come last. Ties keep fill-first order.
type SoonestResetSelector struct {
	nowFunc func() time.Time

	mu       sync.Mutex
	probedAt map[string]time.Time
}

// Pick selects the available auth with the soonest observed quota reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)

	probeDisabled := ctx != nil && ctx.Value(quotaProbeDisabledKey{}) == true
	if !probeDisabled {
		if probe := s.nextProbe(available, now); probe != nil {
			return probe, nil
		}
	}

	var picked *Auth
	var pickedReset time.Time
	for _, candidate := range available {
		reset, ok := quotaWindowResetAt(candidate, now)
		if !ok {
			continue
		}
		if picked == nil || reset.Before(pickedReset) {
			picked = candidate
			pickedReset = reset
		}
	}
	if picked == nil {
		return available[0], nil
	}
	return picked, nil
}

// nextProbe returns an eligible credential needing a fresh reset observation
// and records the probe. A sample before the last window reset does not delay
// the first sample of the new window.
func (s *SoonestResetSelector) nextProbe(available []*Auth, now time.Time) *Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expired entries no longer block a probe; dropping them keeps the map
	// bounded by the credentials probed within the last interval.
	for key, last := range s.probedAt {
		if now.Sub(last) >= soonestResetProbeInterval {
			delete(s.probedAt, key)
		}
	}
	for _, candidate := range available {
		if candidate == nil || !providerReportsQuotaReset(candidate.Provider) {
			continue
		}
		reset, known := quotaWindowReset(candidate)
		if known && reset.After(now) {
			continue
		}
		// Key by account too, so an account swapped into the same auth ID is
		// probed without waiting for the former account's interval. Without an
		// identity, the credential version marks a replaced login instead.
		identity := quotaAccountIdentity(candidate)
		if identity == "" {
			identity = "version:" + strconv.FormatUint(candidate.CredentialVersion, 10)
		}
		key := candidate.ID + "\x00" + identity
		if last, ok := s.probedAt[key]; ok && now.Sub(last) < soonestResetProbeInterval && (!known || !last.Before(reset)) {
			continue
		}
		if s.probedAt == nil {
			s.probedAt = make(map[string]time.Time)
		}
		s.probedAt[key] = now
		return candidate
	}
	return nil
}

// providerReportsQuotaReset reports whether quotaWindowReset understands the
// provider's quota snapshot, so a probe request can teach its reset time.
func providerReportsQuotaReset(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "claude":
		return true
	default:
		return false
	}
}

// quotaWindowResetAt reports the future reset time of the longest quota
// window in the auth's passive quota snapshot.
func quotaWindowResetAt(auth *Auth, now time.Time) (time.Time, bool) {
	reset, ok := quotaWindowReset(auth)
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

// quotaWindowReset reports the reset time of the longest quota window in the
// auth's passive quota snapshot, past or future. Only the current provider's
// keys are read, so a snapshot kept across an in-place provider change cannot
// leak the former provider's deadline. Codex reports a primary and an
// optional secondary window; Claude reports unified 5h and 7d windows.
func quotaWindowReset(auth *Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	reset, _, ok := quotaSnapshotReset(auth.Provider, auth.Quota)
	return reset, ok
}

// quotaSnapshotReset also returns the scored window length so an incomplete
// response cannot displace an observation of a longer quota window.
func quotaSnapshotReset(provider string, quota QuotaState) (time.Time, int64, bool) {
	signals := quota.Signals

	var reset time.Time
	longest := int64(-1)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		for _, prefix := range codexQuotaWindowPrefixes {
			windowReset, ok := codexWindowResetAt(signals, prefix, quota.ObservedAt)
			if !ok {
				continue
			}
			minutes, _ := strconv.ParseInt(strings.TrimSpace(signals[prefix+"Window-Minutes"]), 10, 64)
			if minutes > longest {
				longest = minutes
				reset = windowReset
			}
		}
	case "claude":
		if parsed, ok := parseResetTimestamp(signals["Anthropic-Ratelimit-Unified-7d-Reset"]); ok {
			reset = parsed
			longest = 7 * 24 * 60
		} else if parsed, ok := parseResetTimestamp(signals["Anthropic-Ratelimit-Unified-Reset"]); ok &&
			!claudeResetIsBillingBoundary(signals, parsed, quota.ObservedAt) {
			reset = parsed
			longest = 0
		}
	}
	return reset, longest, !reset.IsZero()
}

func codexWindowResetAt(signals map[string]string, prefix string, observedAt time.Time) (time.Time, bool) {
	if reset, ok := parseResetTimestamp(signals[prefix+"Reset-At"]); ok {
		return reset, true
	}
	if observedAt.IsZero() {
		return time.Time{}, false
	}
	after, ok := parseResetAfterSeconds(signals[prefix+"Reset-After-Seconds"])
	if !ok {
		return time.Time{}, false
	}
	return observedAt.Add(time.Duration(after * float64(time.Second))), true
}

// claudeResetIsBillingBoundary reports whether a representative unified reset
// is the monthly overage billing boundary rather than a quota-window reset:
// either the representative claim is overage and the reset equals the
// overage reset, or the reset lies beyond the longest (7d) quota window.
func claudeResetIsBillingBoundary(signals map[string]string, reset, observedAt time.Time) bool {
	claim := strings.ToLower(strings.TrimSpace(signals["Anthropic-Ratelimit-Unified-Representative-Claim"]))
	if strings.Contains(claim, "overage") {
		if overageReset, ok := parseResetTimestamp(signals["Anthropic-Ratelimit-Unified-Overage-Reset"]); ok && overageReset.Equal(reset) {
			return true
		}
	}
	return !observedAt.IsZero() && reset.Sub(observedAt) > 7*24*time.Hour+time.Hour
}

// parseResetAfterSeconds parses a relative reset value: a finite, non-negative
// number of seconds that fits in a time.Duration.
func parseResetAfterSeconds(raw string) (float64, bool) {
	after, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(after) || after < 0 || after > maxResetAfterSeconds {
		return 0, false
	}
	return after, true
}

// parseResetTimestamp accepts the reset formats upstreams send: Unix seconds
// (optionally fractional), RFC3339, or an HTTP date.
func parseResetTimestamp(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		if math.IsNaN(seconds) || seconds <= 0 || seconds > maxResetUnixSeconds {
			return time.Time{}, false
		}
		whole := int64(seconds)
		return time.Unix(whole, int64((seconds-float64(whole))*1e9)), true
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, true
	}
	if parsed, err := http.ParseTime(raw); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}
