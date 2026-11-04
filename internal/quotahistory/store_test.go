package quotahistory

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var baseTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newTestStore(t *testing.T, path string, now *time.Time) *Store {
	t.Helper()
	s := &Store{path: path, now: func() time.Time { return *now }, last: make(map[seriesKey]Sample)}
	s.load()
	return s
}

func ptrFloat(v float64) *float64 { return &v }
func ptrTime(v time.Time) *time.Time {
	return &v
}

func sample(name string, at time.Time, remaining float64) Sample {
	return Sample{Name: name, Provider: "claude", ObservedAt: at, RemainingPercent: remaining, PeriodHours: ptrFloat(168)}
}

func TestNormalizeValidatesAndClamps(t *testing.T) {
	now := baseTime
	cases := []struct {
		name string
		in   Sample
		ok   bool
		want float64
	}{
		{"clamps high", Sample{Name: "a", Provider: "Claude", RemainingPercent: 140}, true, 100},
		{"clamps low", Sample{Name: "a", Provider: "codex", RemainingPercent: -3}, true, 0},
		{"missing name", Sample{Provider: "codex", RemainingPercent: 3}, false, 0},
		{"missing provider", Sample{Name: "a", RemainingPercent: 3}, false, 0},
		{"nan", Sample{Name: "a", Provider: "codex", RemainingPercent: math.NaN()}, false, 0},
		{"future", Sample{Name: "a", Provider: "codex", ObservedAt: now.Add(10 * time.Minute)}, false, 0},
		{"too old", Sample{Name: "a", Provider: "codex", ObservedAt: now.Add(-Retention - time.Hour)}, false, 0},
	}
	for _, tc := range cases {
		got, ok := Normalize(tc.in, now)
		if ok != tc.ok {
			t.Fatalf("%s: ok=%v want %v", tc.name, ok, tc.ok)
		}
		if ok && got.RemainingPercent != tc.want {
			t.Fatalf("%s: remaining=%v want %v", tc.name, got.RemainingPercent, tc.want)
		}
	}
	got, _ := Normalize(Sample{Name: " a ", Provider: " Claude ", RemainingPercent: 5}, now)
	if got.Name != "a" || got.Provider != "claude" || !got.ObservedAt.Equal(now) {
		t.Fatalf("normalize did not trim/default: %+v", got)
	}
}

func TestAddThrottlesDuplicatesButKeepsResets(t *testing.T) {
	now := baseTime
	s := newTestStore(t, "", &now)

	if n := s.Add(SourceManagement, sample("a", now, 80)); n != 1 {
		t.Fatalf("first sample accepted=%d", n)
	}
	now = now.Add(time.Minute)
	if n := s.Add(SourceManagement, sample("a", now, 79.8)); n != 0 {
		t.Fatalf("near-duplicate accepted=%d", n)
	}
	if n := s.Add(SourceManagement, sample("a", now, 60)); n != 0 {
		t.Fatalf("usage inside the throttle interval accepted=%d", n)
	}
	// Remaining jumping up without a reset_at is still a reset.
	if n := s.Add(SourceManagement, sample("a", now, 100)); n != 1 {
		t.Fatalf("upward jump accepted=%d", n)
	}
	// A reset: changed reset_at must always be kept, even moments later.
	now = now.Add(time.Second)
	reset := sample("a", now, 99.5)
	reset.ResetAt = ptrTime(now.Add(7 * 24 * time.Hour))
	if n := s.Add(SourceObserved, reset); n != 1 {
		t.Fatalf("reset change accepted=%d", n)
	}
	// Same instant echoed from another source is not new information.
	if n := s.Add(SourceManagement, reset); n != 0 {
		t.Fatalf("echo accepted=%d", n)
	}
	now = now.Add(6 * time.Minute)
	steady := sample("a", now, 99.5)
	steady.ResetAt = reset.ResetAt
	if n := s.Add(SourceManagement, steady); n != 1 {
		t.Fatalf("sample after throttle interval accepted=%d", n)
	}
	got := s.Query(time.Time{}, "")
	if len(got) != 4 || got[2].Source != SourceObserved || got[3].Source != SourceManagement {
		t.Fatalf("unexpected samples: %+v", got)
	}
}

func TestQueryFiltersBySinceAndProvider(t *testing.T) {
	now := baseTime
	s := newTestStore(t, "", &now)
	codex := sample("b", now.Add(-time.Hour), 50)
	codex.Provider = "codex"
	s.Add(SourceManagement, sample("a", now.Add(-2*time.Hour), 90), codex, sample("a", now, 80))

	if got := s.Query(now.Add(-90*time.Minute), ""); len(got) != 2 {
		t.Fatalf("since filter: %d", len(got))
	}
	got := s.Query(time.Time{}, "CODEX")
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("provider filter: %+v", got)
	}
	all := s.Query(time.Time{}, "")
	for i := 1; i < len(all); i++ {
		if all[i].ObservedAt.Before(all[i-1].ObservedAt) {
			t.Fatalf("query not ordered: %+v", all)
		}
	}
}

func TestRetentionPrunesOldSamples(t *testing.T) {
	now := baseTime
	s := newTestStore(t, "", &now)
	s.Add(SourceManagement, sample("a", now, 90))
	now = now.Add(Retention + time.Hour)
	s.Add(SourceManagement, sample("b", now, 90))
	got := s.Query(time.Time{}, "")
	if len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("retention: %+v", got)
	}
}

func TestCapEvictsOldestFirst(t *testing.T) {
	now := baseTime
	s := newTestStore(t, "", &now)
	batch := make([]Sample, 0, MaxSamples+5)
	start := now.Add(-time.Hour)
	for i := 0; i < MaxSamples+5; i++ {
		batch = append(batch, sample("cred-"+strconv.Itoa(i), start.Add(time.Duration(i)*time.Millisecond), 50))
	}
	s.Add(SourceManagement, batch...)
	got := s.Query(time.Time{}, "")
	if len(got) != MaxSamples {
		t.Fatalf("cap: %d", len(got))
	}
	if got[0].Name != "cred-5" {
		t.Fatalf("oldest not evicted first: %s", got[0].Name)
	}
}

func TestPersistenceRoundTripAndCompaction(t *testing.T) {
	now := baseTime
	path := filepath.Join(t.TempDir(), "nested", "quota-history.jsonl")
	s := newTestStore(t, path, &now)
	first := sample("a", now.Add(-time.Hour), 90)
	first.AuthIndex = "idx"
	first.ResetAt = ptrTime(now.Add(24 * time.Hour))
	s.Add(SourceManagement, first, sample("b", now, 40))

	reloaded := newTestStore(t, path, &now)
	got := reloaded.Query(time.Time{}, "")
	if len(got) != 2 || got[0].AuthIndex != "idx" || got[0].ResetAt == nil || got[1].Name != "b" {
		t.Fatalf("round trip: %+v", got)
	}
	// Throttle state survives a restart too.
	if n := reloaded.Add(SourceManagement, sample("b", now.Add(time.Minute), 40)); n != 0 {
		t.Fatalf("duplicate after reload accepted=%d", n)
	}

	// Expire everything but one sample, then force a compaction.
	now = now.Add(Retention - 30*time.Minute)
	reloaded.fileLines = 5000
	reloaded.Add(SourceManagement, sample("c", now, 10))
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("compacted file has %d lines, want 2", lines)
	}
}
