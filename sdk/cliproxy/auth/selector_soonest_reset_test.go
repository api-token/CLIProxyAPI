package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeWeeklyResetAuth(id string, resetAt time.Time) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Status:   StatusActive,
		Quota: QuotaState{
			ObservedAt: resetAt.Add(-24 * time.Hour),
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":  strconv.FormatInt(resetAt.Unix(), 10),
				"Anthropic-Ratelimit-Unified-7d-Status": "allowed",
			},
		},
	}
}

func pickSoonestReset(t *testing.T, now time.Time, auths []*Auth) string {
	t.Helper()
	selector := &SoonestResetSelector{nowFunc: func() time.Time { return now }}
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	return got.ID
}

func TestSoonestResetSelectorPick_PrefersSoonestWeeklyReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_793_400_000, 0)
	// The ID-first account resets later; fill-first would pick it, soonest-reset must not.
	auths := []*Auth{
		claudeWeeklyResetAuth("claude-823", now.Add(56*time.Hour)),
		claudeWeeklyResetAuth("claude-8e9", now.Add(17*time.Hour)),
	}
	if got := pickSoonestReset(t, now, auths); got != "claude-8e9" {
		t.Fatalf("Pick() = %q, want claude-8e9", got)
	}
	if auths[0].ID != "claude-823" {
		t.Fatal("Pick() reordered the caller's slice")
	}
}

func TestSoonestResetSelectorPick_UnknownResetRanksFirst(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_793_400_000, 0)
	auths := []*Auth{
		claudeWeeklyResetAuth("a-known", now.Add(time.Hour)),
		{ID: "b-unknown", Provider: "claude", Status: StatusActive},
	}
	if got := pickSoonestReset(t, now, auths); got != "b-unknown" {
		t.Fatalf("Pick() = %q, want b-unknown", got)
	}
}

func TestSoonestResetSelectorPick_ElapsedResetRanksFirst(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_793_400_000, 0)
	// a-rolled-over's recorded window already reset, so its real next reset is unknown.
	auths := []*Auth{
		claudeWeeklyResetAuth("b-pending", now.Add(time.Hour)),
		claudeWeeklyResetAuth("a-rolled-over", now.Add(-time.Minute)),
	}
	if got := pickSoonestReset(t, now, auths); got != "a-rolled-over" {
		t.Fatalf("Pick() = %q, want a-rolled-over", got)
	}
}

func TestSoonestResetSelectorPick_TiesKeepFillFirstOrder(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_793_400_000, 0)
	resetAt := now.Add(10 * time.Hour)
	auths := []*Auth{
		claudeWeeklyResetAuth("c", resetAt),
		claudeWeeklyResetAuth("a", resetAt),
		claudeWeeklyResetAuth("b", resetAt),
	}
	if got := pickSoonestReset(t, now, auths); got != "a" {
		t.Fatalf("Pick() = %q, want a", got)
	}
	unknown := []*Auth{{ID: "y", Provider: "claude"}, {ID: "x", Provider: "claude"}}
	if got := pickSoonestReset(t, now, unknown); got != "x" {
		t.Fatalf("Pick() = %q, want x", got)
	}
}

func TestSoonestResetSelectorPick_PriorityWinsOverReset(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_793_400_000, 0)
	high := claudeWeeklyResetAuth("high", now.Add(100*time.Hour))
	high.Attributes = map[string]string{"priority": "5"}
	auths := []*Auth{high, claudeWeeklyResetAuth("low", now.Add(time.Hour))}
	if got := pickSoonestReset(t, now, auths); got != "high" {
		t.Fatalf("Pick() = %q, want high", got)
	}
}

func TestSoonestResetSelectorPick_SkipsCooledDownSoonestAccount(t *testing.T) {
	t.Parallel()

	now := time.Now()
	soonest := claudeWeeklyResetAuth("soonest", now.Add(time.Hour))
	soonest.Quota.Exceeded = true
	soonest.Quota.Reason = "credential_quota"
	soonest.Quota.NextRecoverAt = now.Add(30 * time.Minute)
	auths := []*Auth{soonest, claudeWeeklyResetAuth("later", now.Add(48*time.Hour))}
	if got := pickSoonestReset(t, now, auths); got != "later" {
		t.Fatalf("Pick() = %q, want later", got)
	}
}

func TestWeeklyQuotaResetAt(t *testing.T) {
	t.Parallel()

	observedAt := time.Unix(1_793_000_000, 0)
	tests := []struct {
		name    string
		signals map[string]string
		want    time.Time
		wantOK  bool
	}{
		{
			name:    "claude unified 7d reset",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "1793462400"},
			want:    time.Unix(1793462400, 0), wantOK: true,
		},
		{
			name:    "claude header stored under non-canonical casing",
			signals: map[string]string{"anthropic-ratelimit-unified-7d-reset": "1793462400"},
			want:    time.Unix(1793462400, 0), wantOK: true,
		},
		{
			name: "codex weekly primary window",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes": "10080",
				"X-Codex-Primary-Reset-At":       "1793801411",
			},
			want: time.Unix(1793801411, 0), wantOK: true,
		},
		{
			name: "codex weekly secondary window behind a 5h primary",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes":   "300",
				"X-Codex-Primary-Reset-At":         "1793010000",
				"X-Codex-Secondary-Window-Minutes": "10080",
				"X-Codex-Secondary-Reset-At":       "1793500000",
			},
			want: time.Unix(1793500000, 0), wantOK: true,
		},
		{
			name: "codex weekly window with only reset-after",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes":      "10080",
				"X-Codex-Primary-Reset-After-Seconds": "3600",
			},
			want: observedAt.Add(time.Hour), wantOK: true,
		},
		{
			name: "codex non-weekly window is ignored",
			signals: map[string]string{
				"X-Codex-Primary-Window-Minutes": "300",
				"X-Codex-Primary-Reset-At":       "1793010000",
			},
		},
		{
			name:    "malformed reset",
			signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Reset": "soon"},
		},
		{name: "no signals"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := WeeklyQuotaResetAt(&Auth{Quota: QuotaState{ObservedAt: observedAt, Signals: tt.signals}})
			if ok != tt.wantOK || !got.Equal(tt.want) {
				t.Fatalf("WeeklyQuotaResetAt() = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
	if _, ok := WeeklyQuotaResetAt(nil); ok {
		t.Fatal("WeeklyQuotaResetAt(nil) reported a reset")
	}
}

func registerSoonestResetAuths(t *testing.T, manager *Manager, model string, auths ...*Auth) {
	t.Helper()
	for _, candidate := range auths {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: model}})
		id := candidate.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
}

// Mirrors the deployed config: fill-first replaced by soonest-reset behind session affinity.
func TestManagerSoonestResetWithSessionAffinityBindsNewSessionsToSoonestReset(t *testing.T) {
	const model = "soonest-reset-affinity-model"
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude"})
	now := time.Now()
	registerSoonestResetAuths(t, manager, model,
		claudeWeeklyResetAuth("claude-823-"+t.Name(), now.Add(56*time.Hour)),
		claudeWeeklyResetAuth("claude-8e9-"+t.Name(), now.Add(17*time.Hour)),
	)

	for _, session := range []string{"session-a", "session-b", "session-c"} {
		opts := cliproxyexecutor.Options{
			Headers:  http.Header{"X-Session-Id": []string{session}},
			Metadata: map[string]any{},
		}
		got, errSelect := manager.SelectAuth(context.Background(), "claude", model, opts)
		if errSelect != nil {
			t.Fatalf("SelectAuth(%s) error = %v", session, errSelect)
		}
		if got.ID != "claude-8e9-"+t.Name() {
			t.Fatalf("SelectAuth(%s) = %q, want the soonest-reset account", session, got.ID)
		}
	}
}

// Starts with no observations and lets upstream response headers teach the reset times.
func TestManagerSoonestResetLearnsResetTimesFromResponses(t *testing.T) {
	const model = "soonest-reset-learn-model"
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	later, sooner := "a-later-"+t.Name(), "b-sooner-"+t.Name()
	registerSoonestResetAuths(t, manager, model,
		&Auth{ID: later, Provider: "claude", Status: StatusActive},
		&Auth{ID: sooner, Provider: "claude", Status: StatusActive},
	)
	now := time.Now()
	resets := map[string]time.Time{later: now.Add(56 * time.Hour), sooner: now.Add(17 * time.Hour)}
	manager.RegisterExecutor(&mockCustomErrorExecutor{
		identifier: "claude",
		executeFn: func(ctx context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			internallogging.SetResponseHeaders(ctx, http.Header{
				"Anthropic-Ratelimit-Unified-7d-Reset":  []string{strconv.FormatInt(resets[auth.ID].Unix(), 10)},
				"Anthropic-Ratelimit-Unified-7d-Status": []string{"allowed"},
			})
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})

	// Neither reset is known: ID order probes a-later, then the still-unknown b-sooner,
	// after which every request stays on b-sooner.
	want := []string{later, sooner, sooner, sooner}
	for i, wantID := range want {
		resp, errExecute := manager.Execute(context.Background(), []string{"claude"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("Execute #%d error = %v", i, errExecute)
		}
		if got := string(resp.Payload); got != wantID {
			t.Fatalf("Execute #%d routed to %q, want %q", i, got, wantID)
		}
	}
}

// Without session affinity the selector must rank the candidates the manager validated per
// resolved upstream model, not re-check them against a stale cooldown on the route alias.
func TestManagerSoonestResetIgnoresCooldownOnRemappedAlias(t *testing.T) {
	withQuotaCooldownEnabled(t)
	const routeModel, targetModel = "soonest-reset-alias", "soonest-reset-healthy-target"
	manager := NewManager(nil, &SoonestResetSelector{}, nil)
	manager.RegisterExecutor(&mockCustomErrorExecutor{identifier: "claude"})
	now := time.Now()
	later, sooner := "a-later-"+t.Name(), "b-sooner-"+t.Name()
	for _, candidate := range []*Auth{
		claudeWeeklyResetAuth(later, now.Add(56*time.Hour)),
		claudeWeeklyResetAuth(sooner, now.Add(17*time.Hour)),
	} {
		registry.GetGlobalRegistry().RegisterClient(candidate.ID, "claude", []*registry.ModelInfo{{ID: routeModel}, {ID: targetModel}})
		id := candidate.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, errRegister := manager.Register(context.Background(), candidate); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	retryAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID: sooner, Provider: "claude", Model: routeModel, RetryAfter: &retryAfter,
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
	})
	manager.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"claude": {{Name: targetModel, Alias: routeModel, Fork: true}},
	})

	got, errSelect := manager.SelectAuth(context.Background(), "claude", routeModel, cliproxyexecutor.Options{})
	if errSelect != nil {
		t.Fatalf("SelectAuth() error = %v", errSelect)
	}
	if got.ID != sooner {
		t.Fatalf("SelectAuth() = %q, want %q", got.ID, sooner)
	}
}
