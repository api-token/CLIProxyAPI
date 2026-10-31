package auth

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// weeklyWindowMinutes is the length of the subscription windows this selector ranks by.
const weeklyWindowMinutes = 7 * 24 * 60

// SoonestResetSelector is fill-first ordered by weekly quota reset time instead of ID.
// The credential whose weekly window resets soonest is drained first, so quota that is
// about to expire is spent before quota that would carry over for longer.
//
// The reset time comes from the passive quota snapshot recorded on every upstream
// response (Anthropic unified 7d headers, Codex 10080-minute window headers).
// Credentials without a known future reset are tried first so one request learns their
// window; credentials sharing a reset time keep the fill-first ID order.
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
		resets := make(map[*Auth]time.Time, len(ordered))
		for _, candidate := range ordered {
			resetAt, ok := WeeklyQuotaResetAt(candidate)
			if !ok || !resetAt.After(now) {
				// Unknown or already rolled over: rank first so the next response refreshes it.
				resetAt = time.Time{}
			}
			resets[candidate] = resetAt
		}
		sort.SliceStable(ordered, func(i, j int) bool {
			return resets[ordered[i]].Before(resets[ordered[j]])
		})
		available = ordered
	}
	return available[0], nil
}

// WeeklyQuotaResetAt reports when the credential's weekly quota window next resets,
// as last observed in upstream response headers.
func WeeklyQuotaResetAt(auth *Auth) (time.Time, bool) {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return time.Time{}, false
	}
	signals := auth.Quota.Signals
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

// quotaSignal looks up a signal by header name. Signals are stored under canonical
// header keys, but names such as "7d_oi" are not canonicalizable, so fall back to a
// case-insensitive scan.
func quotaSignal(signals map[string]string, name string) (string, bool) {
	if value, ok := signals[http.CanonicalHeaderKey(name)]; ok {
		return strings.TrimSpace(value), true
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}
