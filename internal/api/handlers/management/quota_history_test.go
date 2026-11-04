package management

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestObservedQuotaSampleClaudeWeekly(t *testing.T) {
	observedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	reset := observedAt.Add(3 * 24 * time.Hour)
	// Header names and values as captured in quota_signals_test.go.
	auth := &coreauth.Auth{
		ID:       "claude-a.json",
		FileName: "claude-a.json",
		Provider: "claude",
		Quota: coreauth.QuotaState{ObservedAt: observedAt, Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-5h-Utilization": "0.9",
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.53",
			"Anthropic-Ratelimit-Unified-7d-Reset":       fmt.Sprint(reset.Unix()),
		}},
	}
	sample, ok := observedQuotaSample(auth)
	if !ok {
		t.Fatal("expected a sample")
	}
	if sample.Name != "claude-a.json" || sample.Provider != "claude" || sample.AuthIndex == "" {
		t.Fatalf("identity: %+v", sample)
	}
	if sample.WindowID != "seven-day" || sample.PeriodHours == nil || *sample.PeriodHours != 168 {
		t.Fatalf("window: %+v", sample)
	}
	if diff := sample.RemainingPercent - 47; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("remaining=%v want 47", sample.RemainingPercent)
	}
	if sample.ResetAt == nil || !sample.ResetAt.Equal(reset) || !sample.ObservedAt.Equal(observedAt) {
		t.Fatalf("times: %+v", sample)
	}
}

func TestObservedQuotaSampleCodexPicksLongestWindow(t *testing.T) {
	observedAt := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	auth := &coreauth.Auth{
		ID:       "codex-a.json",
		FileName: "codex-a.json",
		Provider: "codex",
		Quota: coreauth.QuotaState{ObservedAt: observedAt, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":          "90",
			"X-Codex-Primary-Window-Minutes":        "300",
			"X-Codex-Primary-Reset-After-Seconds":   "600",
			"X-Codex-Secondary-Used-Percent":        "25",
			"X-Codex-Secondary-Window-Minutes":      "10080",
			"X-Codex-Secondary-Reset-After-Seconds": "86400",
		}},
	}
	sample, ok := observedQuotaSample(auth)
	if !ok {
		t.Fatal("expected a sample")
	}
	if sample.WindowID != "weekly" || *sample.PeriodHours != 168 || sample.RemainingPercent != 75 {
		t.Fatalf("window: %+v", sample)
	}
	if sample.ResetAt == nil || !sample.ResetAt.Equal(observedAt.Add(24*time.Hour)) {
		t.Fatalf("reset: %v", sample.ResetAt)
	}

	// Free plans report the weekly window as primary only.
	auth.Quota.Signals = map[string]string{
		"X-Codex-Primary-Used-Percent":   "10",
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       fmt.Sprint(observedAt.Add(time.Hour).Unix()),
	}
	sample, ok = observedQuotaSample(auth)
	if !ok || sample.RemainingPercent != 90 || !sample.ResetAt.Equal(observedAt.Add(time.Hour)) {
		t.Fatalf("primary-only: ok=%v %+v", ok, sample)
	}
}

func TestObservedQuotaSampleSkipsUnusable(t *testing.T) {
	now := time.Now()
	for _, auth := range []*coreauth.Auth{
		nil,
		{ID: "a", Provider: "claude"},
		{ID: "a", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{"Retry-After": "5"}}},
		{ID: "a", Provider: "claude", Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "bad"}}},
		{ID: "a", Provider: "gemini", Quota: coreauth.QuotaState{ObservedAt: now, Signals: map[string]string{"X-Codex-Primary-Used-Percent": "5"}}},
	} {
		if _, ok := observedQuotaSample(auth); ok {
			t.Fatalf("unexpected sample for %+v", auth)
		}
	}
}

func serveQuotaHistory(t *testing.T, h *Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/history", h.GetQuotaHistory)
	router.POST("/history", h.PostQuotaHistory)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestQuotaHistoryHandlers(t *testing.T) {
	h := &Handler{quotaHistory: quotahistory.New("")}
	now := time.Now().UTC().Truncate(time.Second)
	body := fmt.Sprintf(`{"samples":[
		{"name":"claude-a.json","provider":"claude","remaining_percent":80,"period_hours":168,"observed_at":%q,"reset_at":%q},
		{"name":"codex-a.json","auth_index":"x1","provider":"codex","remaining_percent":120,"observed_at":%q},
		{"name":"","provider":"codex","remaining_percent":10},
		{"name":"missing-remaining.json","provider":"codex"}
	]}`, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339), now.Format(time.RFC3339))

	rec := serveQuotaHistory(t, h, http.MethodPost, "/history", body)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"accepted":2}` {
		t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Samples        []quotahistory.Sample `json:"samples"`
		RetentionHours int                   `json:"retention_hours"`
	}
	rec = serveQuotaHistory(t, h, http.MethodGet, "/history", "")
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil || rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if len(payload.Samples) != 2 || payload.RetentionHours != 336 {
		t.Fatalf("get payload: %+v", payload)
	}
	if payload.Samples[0].Source != quotahistory.SourceManagement || payload.Samples[1].RemainingPercent != 100 {
		t.Fatalf("samples: %+v", payload.Samples)
	}

	since := now.Add(-time.Hour).Format(time.RFC3339)
	rec = serveQuotaHistory(t, h, http.MethodGet, "/history?since="+since, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if len(payload.Samples) != 1 || payload.Samples[0].Name != "codex-a.json" || payload.Samples[0].AuthIndex != "x1" {
		t.Fatalf("since filter: %+v", payload.Samples)
	}
	rec = serveQuotaHistory(t, h, http.MethodGet, "/history?provider=claude", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if len(payload.Samples) != 1 || payload.Samples[0].Provider != "claude" {
		t.Fatalf("provider filter: %+v", payload.Samples)
	}

	if rec = serveQuotaHistory(t, h, http.MethodGet, "/history?since=yesterday", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since: %d", rec.Code)
	}
	if rec = serveQuotaHistory(t, h, http.MethodPost, "/history", "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", rec.Code)
	}
	many := `{"samples":[` + strings.TrimSuffix(strings.Repeat(`{"name":"a","provider":"codex","remaining_percent":1},`, 501), ",") + `]}`
	if rec = serveQuotaHistory(t, h, http.MethodPost, "/history", many); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch: %d", rec.Code)
	}
}

func TestSampleObservedQuotaRecordsFromManager(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	observedAt := time.Now().UTC().Add(-time.Minute)
	if _, errRegister := manager.Register(t.Context(), &coreauth.Auth{
		ID:       "codex-a.json",
		FileName: "codex-a.json",
		Provider: "codex",
		Quota: coreauth.QuotaState{ObservedAt: observedAt, Signals: map[string]string{
			"X-Codex-Primary-Used-Percent":   "40",
			"X-Codex-Primary-Window-Minutes": "10080",
		}},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := &Handler{authManager: manager, quotaHistory: quotahistory.New("")}
	h.sampleObservedQuota()
	h.sampleObservedQuota() // same observation: recorded once
	got := h.quotaHistory.Query(time.Time{}, "")
	if len(got) != 1 || got[0].Source != quotahistory.SourceObserved || got[0].RemainingPercent != 60 {
		t.Fatalf("observed samples: %+v", got)
	}
}
