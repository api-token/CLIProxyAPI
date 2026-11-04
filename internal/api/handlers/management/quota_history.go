package management

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

const (
	// quotaHistoryFileName lives next to the config file.
	quotaHistoryFileName = "quota-history.jsonl"
	// quotaHistoryMaxBatch bounds one POST.
	quotaHistoryMaxBatch = 500
	// quotaHistoryScanInterval is how often passive observations are sampled.
	// The store throttles unchanged readings, so a short interval only costs a
	// pass over the in-memory auth list.
	quotaHistoryScanInterval = time.Minute
)

func quotaHistoryPath(configFilePath string) string {
	if strings.TrimSpace(configFilePath) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configFilePath), quotaHistoryFileName)
}

// startQuotaHistorySampler records passive quota observations in the
// background. It reads auth snapshots only, so the proxy hot path is untouched.
func (h *Handler) startQuotaHistorySampler() {
	if h.authManager == nil || h.quotaHistory == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(quotaHistoryScanInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.sampleObservedQuota()
		}
	}()
}

func (h *Handler) sampleObservedQuota() {
	if h.authManager == nil || h.quotaHistory == nil {
		return
	}
	auths := h.authManager.List()
	samples := make([]quotahistory.Sample, 0, len(auths))
	for _, auth := range auths {
		if sample, ok := observedQuotaSample(auth); ok {
			samples = append(samples, sample)
		}
	}
	h.quotaHistory.Add(quotahistory.SourceObserved, samples...)
}

// observedQuotaSample derives the credential-level weekly reading from the
// passive header snapshot. Only the longest window is used, because the
// management UI pools credentials on their weekly window.
func observedQuotaSample(auth *coreauth.Auth) (quotahistory.Sample, bool) {
	if auth == nil || auth.Quota.ObservedAt.IsZero() || len(auth.Quota.Signals) == 0 {
		return quotahistory.Sample{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	sample := quotahistory.Sample{
		Name:       name,
		AuthIndex:  auth.EnsureIndex(),
		Provider:   provider,
		ObservedAt: auth.Quota.ObservedAt,
	}
	signals := auth.Quota.Signals
	observedAt := auth.Quota.ObservedAt

	switch provider {
	case "claude":
		// Unified utilization is a 0..1 fraction; reset is unix seconds.
		raw, ok := signalValue(signals, "Anthropic-Ratelimit-Unified-7d-Utilization")
		if !ok {
			return quotahistory.Sample{}, false
		}
		utilization, errParse := strconv.ParseFloat(raw, 64)
		if errParse != nil {
			return quotahistory.Sample{}, false
		}
		period := 7 * 24.0
		sample.WindowID = "seven-day"
		sample.PeriodHours = &period
		sample.RemainingPercent = (1 - utilization) * 100
		if resetAt, okReset := unixSignalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset"); okReset {
			sample.ResetAt = &resetAt
		}
		return sample, true

	case "codex":
		// Pick whichever of primary/secondary is the longer window.
		bestMinutes := 0
		bestPrefix := ""
		for _, prefix := range []string{"X-Codex-Primary-", "X-Codex-Secondary-"} {
			raw, ok := signalValue(signals, prefix+"Window-Minutes")
			if !ok {
				continue
			}
			minutes, errParse := strconv.Atoi(raw)
			if errParse != nil || minutes <= 0 {
				continue
			}
			if _, okUsed := signalValue(signals, prefix+"Used-Percent"); !okUsed {
				continue
			}
			if minutes > bestMinutes {
				bestMinutes, bestPrefix = minutes, prefix
			}
		}
		if bestPrefix == "" {
			return quotahistory.Sample{}, false
		}
		rawUsed, _ := signalValue(signals, bestPrefix+"Used-Percent")
		used, errParse := strconv.ParseFloat(rawUsed, 64)
		if errParse != nil {
			return quotahistory.Sample{}, false
		}
		period := float64(bestMinutes) / 60
		sample.PeriodHours = &period
		switch bestMinutes {
		case 7 * 24 * 60:
			sample.WindowID = "weekly"
		case 5 * 60:
			sample.WindowID = "five-hour"
		}
		sample.RemainingPercent = 100 - used
		if resetAt, okReset := unixSignalValue(signals, bestPrefix+"Reset-At"); okReset {
			sample.ResetAt = &resetAt
		} else if rawAfter, okAfter := signalValue(signals, bestPrefix+"Reset-After-Seconds"); okAfter {
			if seconds, errAfter := strconv.ParseInt(rawAfter, 10, 64); errAfter == nil && seconds > 0 {
				resetAt := observedAt.Add(time.Duration(seconds) * time.Second)
				sample.ResetAt = &resetAt
			}
		}
		return sample, true
	}
	return quotahistory.Sample{}, false
}

// signalValue looks a header up case-insensitively; signals are stored under
// canonical keys, but some limit names are not canonicalizable.
func signalValue(signals map[string]string, name string) (string, bool) {
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

func unixSignalValue(signals map[string]string, name string) (time.Time, bool) {
	raw, ok := signalValue(signals, name)
	if !ok {
		return time.Time{}, false
	}
	seconds, errParse := strconv.ParseInt(raw, 10, 64)
	if errParse != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

type quotaHistoryInput struct {
	Name             string     `json:"name"`
	AuthIndex        string     `json:"auth_index"`
	Provider         string     `json:"provider"`
	ObservedAt       *time.Time `json:"observed_at"`
	WindowID         string     `json:"window_id"`
	PeriodHours      *float64   `json:"period_hours"`
	RemainingPercent *float64   `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at"`
}

// GetQuotaHistory returns recorded quota samples, oldest first.
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	since := time.Now().Add(-quotahistory.Retention)
	if raw := strings.TrimSpace(c.Query("since")); raw != "" {
		parsed, errParse := time.Parse(time.RFC3339, raw)
		if errParse != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid since; expected RFC 3339"})
			return
		}
		since = parsed
	}
	samples := h.quotaHistory.Query(since, c.Query("provider"))
	if samples == nil {
		samples = []quotahistory.Sample{}
	}
	c.JSON(http.StatusOK, gin.H{
		"samples":         samples,
		"retention_hours": int(quotahistory.Retention / time.Hour),
	})
}

// PostQuotaHistory records samples a management client read from provider
// usage endpoints. Invalid samples are skipped rather than failing the batch.
func (h *Handler) PostQuotaHistory(c *gin.Context) {
	var body struct {
		Samples []quotaHistoryInput `json:"samples"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if len(body.Samples) > quotaHistoryMaxBatch {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many samples; at most 500 per request"})
		return
	}
	samples := make([]quotahistory.Sample, 0, len(body.Samples))
	for _, input := range body.Samples {
		if input.RemainingPercent == nil {
			continue
		}
		sample := quotahistory.Sample{
			Name:             input.Name,
			AuthIndex:        input.AuthIndex,
			Provider:         input.Provider,
			WindowID:         input.WindowID,
			PeriodHours:      input.PeriodHours,
			RemainingPercent: *input.RemainingPercent,
			ResetAt:          input.ResetAt,
		}
		if input.ObservedAt != nil {
			sample.ObservedAt = *input.ObservedAt
		}
		samples = append(samples, sample)
	}
	accepted := h.quotaHistory.Add(quotahistory.SourceManagement, samples...)
	c.JSON(http.StatusOK, gin.H{"accepted": accepted})
}
