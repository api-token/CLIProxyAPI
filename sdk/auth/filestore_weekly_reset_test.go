package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// TestFileTokenStoreWeeklyResetSurvivesRestart verifies that a weekly reset observed in a
// response is written to the auth file and ranks credentials after a fresh load.
func TestFileTokenStoreWeeklyResetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"claude-a-later.json", "claude-b-sooner.json"} {
		if errWrite := os.WriteFile(filepath.Join(dir, name), []byte(`{"type":"claude","email":"`+name+`"}`), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil || len(auths) != 2 {
		t.Fatalf("List() = %d auths, err %v", len(auths), errList)
	}

	now := time.Now()
	resets := map[string]time.Time{
		"claude-a-later.json":  now.Add(56 * time.Hour).Truncate(time.Second),
		"claude-b-sooner.json": now.Add(17 * time.Hour).Truncate(time.Second),
	}
	manager := cliproxyauth.NewManager(store, nil, nil)
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		ctx := internallogging.WithResponseHeadersHolder(context.Background())
		internallogging.SetResponseHeaders(ctx, http.Header{
			"Anthropic-Ratelimit-Unified-7d-Reset": []string{strconv.FormatInt(resets[auth.ID].Unix(), 10)},
		})
		manager.MarkResult(ctx, cliproxyauth.Result{AuthID: auth.ID, Provider: "claude", Model: "claude-test", Success: true})
	}

	for name, resetAt := range resets {
		raw, errRead := os.ReadFile(filepath.Join(dir, name))
		if errRead != nil {
			t.Fatal(errRead)
		}
		var onDisk map[string]any
		if errUnmarshal := json.Unmarshal(raw, &onDisk); errUnmarshal != nil {
			t.Fatal(errUnmarshal)
		}
		if got, want := onDisk[cliproxyauth.WeeklyQuotaResetMetadataKey], resetAt.UTC().Format(time.RFC3339); got != want {
			t.Fatalf("%s %s = %v, want %s", name, cliproxyauth.WeeklyQuotaResetMetadataKey, got, want)
		}
		if onDisk["email"] != name {
			t.Fatalf("%s lost existing fields: %v", name, onDisk)
		}
	}

	restarted := NewFileTokenStore()
	restarted.SetBaseDir(dir)
	reloaded, errReload := restarted.List(context.Background())
	if errReload != nil {
		t.Fatal(errReload)
	}
	selector := &cliproxyauth.SoonestResetSelector{}
	picked, errPick := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, reloaded)
	if errPick != nil {
		t.Fatal(errPick)
	}
	if picked.ID != "claude-b-sooner.json" {
		t.Fatalf("Pick() after restart = %q, want claude-b-sooner.json", picked.ID)
	}
}

// TestFileTokenStoreWeeklyResetPersistsCodexAuth covers the Codex-specific save path, which
// merges credentials from disk instead of writing metadata wholesale.
func TestFileTokenStoreWeeklyResetPersistsCodexAuth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-user.json")
	content := `{"type":"codex","email":"user@example.com","access_token":"at","refresh_token":"rt","account_id":"acct"}`
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, errList := store.List(context.Background())
	if errList != nil || len(auths) != 1 {
		t.Fatalf("List() = %d auths, err %v", len(auths), errList)
	}
	manager := cliproxyauth.NewManager(store, nil, nil)
	if _, errRegister := manager.Register(context.Background(), auths[0]); errRegister != nil {
		t.Fatal(errRegister)
	}
	resetAt := time.Now().Add(90 * time.Hour).Truncate(time.Second)
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	internallogging.SetResponseHeaders(ctx, http.Header{
		"X-Codex-Primary-Window-Minutes": []string{"10080"},
		"X-Codex-Primary-Reset-At":       []string{strconv.FormatInt(resetAt.Unix(), 10)},
	})
	manager.MarkResult(ctx, cliproxyauth.Result{AuthID: auths[0].ID, Provider: "codex", Model: "gpt-test", Success: true})

	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var onDisk map[string]any
	if errUnmarshal := json.Unmarshal(raw, &onDisk); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if got, want := onDisk[cliproxyauth.WeeklyQuotaResetMetadataKey], resetAt.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("%s = %v, want %s", cliproxyauth.WeeklyQuotaResetMetadataKey, got, want)
	}
	for key, want := range map[string]string{"access_token": "at", "refresh_token": "rt", "account_id": "acct", "email": "user@example.com"} {
		if onDisk[key] != want {
			t.Fatalf("%s = %v, want %s; file %s", key, onDisk[key], want, raw)
		}
	}
}
