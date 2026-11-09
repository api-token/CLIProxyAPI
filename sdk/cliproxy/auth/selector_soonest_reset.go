package auth

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// weeklyWindowMinutes is the length of the subscription windows this selector ranks by.
	weeklyWindowMinutes = 7 * 24 * 60
	// WeeklyQuotaResetMetadataKey stores the last observed weekly reset (RFC 3339) in the
	// auth file, so the ranking survives a restart before any new response is observed.
	WeeklyQuotaResetMetadataKey = "weekly_quota_reset_at"
	// weeklyQuotaResetPersistTolerance absorbs reset times derived from relative headers,
	// which shift by a second or so per response, so the auth file is not rewritten each time.
	weeklyQuotaResetPersistTolerance = time.Minute
	// weeklyResetProbeGrace is how long after a reset a completed response still does not
	// count as a probe of the new window. A response that began before the reset can
	// finish just after it while carrying the old reset time.
	weeklyResetProbeGrace = 15 * time.Minute
)

// SoonestResetSelector is fill-first ordered by weekly quota reset time instead of ID.
// The credential whose weekly window resets soonest is drained first, so quota that is
// about to expire is spent before quota that would carry over for longer.
//
// The reset time comes from the passive quota snapshot recorded on every upstream
// response (Anthropic unified 7d headers, Codex 10080-minute window headers).
// Credentials without a known future reset are tried first until a response completes
// and teaches their window. A credential that answers without reporting a weekly window,
// or without renewing one that has elapsed, has nothing that expires, so it is then
// spent last. Credentials that rank equally keep the fill-first ID order. The last
// observed reset is also kept in auth metadata, which covers the time after a restart.
type SoonestResetSelector struct {
	// nowFunc overrides the clock in tests.
	nowFunc func() time.Time
}

// Pick selects the available credential whose weekly window resets soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	if s != nil && s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) > 1 {
		ordered := make([]*Auth, len(available))
		copy(ordered, available)
		ranks := make(map[*Auth]weeklyResetRank, len(ordered))
		for _, candidate := range ordered {
			ranks[candidate] = rankWeeklyReset(candidate, now)
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			a, b := ranks[ordered[i]], ranks[ordered[j]]
			if a.tier != b.tier {
				return a.tier < b.tier
			}
			return a.resetAt.Before(b.resetAt)
		})
		available = ordered
	}
	return available[0], nil
}

const (
	weeklyResetTierProbe = iota // reset unknown or rolled over: try first to learn it
	weeklyResetTierKnown        // known future reset: soonest first
	weeklyResetTierNone         // answered since without a future reset: spend last
)

// weeklyResetRank orders candidates by tier, then by reset time within the known tier.
type weeklyResetRank struct {
	tier    int
	resetAt time.Time
}

func rankWeeklyReset(auth *Auth, now time.Time) weeklyResetRank {
	// resetAt is zero when unknown, so any probe is after it.
	resetAt, ok := weeklyQuotaResetAt(auth)
	switch {
	case ok && resetAt.After(now):
		return weeklyResetRank{tier: weeklyResetTierKnown, resetAt: resetAt}
	case auth.weeklyResetProbedAt.After(resetAt.Add(weeklyResetProbeGrace)):
		return weeklyResetRank{tier: weeklyResetTierNone}
	default:
		return weeklyResetRank{tier: weeklyResetTierProbe}
	}
}

// weeklyQuotaResetAt reports when the credential's weekly quota window next resets, as
// last observed in upstream response headers, or as persisted in auth metadata when no
// response has been observed since the auth was loaded.
func weeklyQuotaResetAt(auth *Auth) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	if resetAt, ok := observedWeeklyQuotaResetAt(auth); ok {
		return resetAt, true
	}
	return persistedWeeklyQuotaResetAt(auth)
}

// syncWeeklyQuotaResetMetadata copies a newly observed weekly reset into the metadata the
// auth store persists. Auths without metadata (config API keys, runtime-only) are skipped.
func syncWeeklyQuotaResetMetadata(auth *Auth) {
	if auth == nil || auth.Metadata == nil {
		return
	}
	resetAt, ok := observedWeeklyQuotaResetAt(auth)
	if !ok {
		return
	}
	if persisted, okPersisted := persistedWeeklyQuotaResetAt(auth); okPersisted {
		delta := resetAt.Sub(persisted)
		if delta > -weeklyQuotaResetPersistTolerance && delta < weeklyQuotaResetPersistTolerance {
			return
		}
	}
	auth.Metadata[WeeklyQuotaResetMetadataKey] = resetAt.UTC().Format(time.RFC3339)
}

func persistedWeeklyQuotaResetAt(auth *Auth) (time.Time, bool) {
	raw, ok := auth.Metadata[WeeklyQuotaResetMetadataKey].(string)
	if !ok {
		return time.Time{}, false
	}
	resetAt, errParse := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if errParse != nil {
		return time.Time{}, false
	}
	return resetAt, true
}

func observedWeeklyQuotaResetAt(auth *Auth) (time.Time, bool) {
	signals := auth.Quota.Signals
	if len(signals) == 0 {
		return time.Time{}, false
	}
	if resetAt, ok := unixSignal(signals, "Anthropic-Ratelimit-Unified-7d-Reset"); ok {
		return resetAt, true
	}
	for _, window := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
		minutes, ok := quotaSignal(signals, window+"Window-Minutes")
		if !ok || minutes != strconv.Itoa(weeklyWindowMinutes) {
			continue
		}
		if resetAt, okReset := unixSignal(signals, window+"Reset-At"); okReset {
			return resetAt, true
		}
		after, okAfter := quotaSignal(signals, window+"Reset-After-Seconds")
		if !okAfter || auth.Quota.ObservedAt.IsZero() {
			continue
		}
		seconds, errParse := strconv.ParseInt(after, 10, 64)
		if errParse != nil || seconds <= 0 {
			continue
		}
		// ObservedAt is when the response finished, so a reset derived from a relative
		// header runs late by however long that response took.
		return auth.Quota.ObservedAt.Add(time.Duration(seconds) * time.Second), true
	}
	return time.Time{}, false
}

func unixSignal(signals map[string]string, name string) (time.Time, bool) {
	raw, ok := quotaSignal(signals, name)
	if !ok {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// quotaSignal looks up a signal by its canonical header name, the form signals are stored under.
func quotaSignal(signals map[string]string, name string) (string, bool) {
	value, ok := signals[name]
	return strings.TrimSpace(value), ok
}
