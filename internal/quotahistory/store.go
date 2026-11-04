// Package quotahistory keeps a bounded, persisted time series of credential
// quota readings so the management UI can chart remaining capacity over time.
//
// Each sample is one credential's reading of a single quota window (normally
// the weekly one) at one instant. Samples arrive from two places: the
// management frontend, which parses provider usage endpoints and reports what
// it read, and the passive observer, which derives readings from upstream
// response headers on proxied traffic.
package quotahistory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// Retention is how long samples are kept.
	Retention = 14 * 24 * time.Hour
	// MaxSamples bounds the total number of stored samples; the oldest are evicted first.
	MaxSamples = 200_000
	// MaxFutureSkew is how far ahead of the server clock a sample may be stamped.
	MaxFutureSkew = 5 * time.Minute

	// throttleInterval bounds each series to one sample per interval, which
	// keeps 14 days of history small enough to chart. A reset always gets
	// through: either reset_at changed, or remaining jumped up by at least
	// resetJump (for sources that do not report reset_at).
	throttleInterval = 5 * time.Minute
	resetJump        = 5.0

	maxNameLength     = 512
	maxProviderLength = 64
	maxWindowIDLength = 128

	// SourceManagement marks samples reported by a management client.
	SourceManagement = "management"
	// SourceObserved marks samples derived from upstream response headers.
	SourceObserved = "observed"
)

// Sample is one credential's reading of a quota window at one instant.
type Sample struct {
	Name             string     `json:"name"`
	AuthIndex        string     `json:"auth_index,omitempty"`
	Provider         string     `json:"provider"`
	ObservedAt       time.Time  `json:"observed_at"`
	WindowID         string     `json:"window_id,omitempty"`
	PeriodHours      *float64   `json:"period_hours,omitempty"`
	RemainingPercent float64    `json:"remaining_percent"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	Source           string     `json:"source"`
}

type seriesKey struct {
	name, authIndex, provider string
	periodHours               float64
}

func keyOf(s Sample) seriesKey {
	period := 0.0
	if s.PeriodHours != nil {
		period = *s.PeriodHours
	}
	return seriesKey{name: s.Name, authIndex: s.AuthIndex, provider: s.Provider, periodHours: period}
}

// Store holds samples in memory, ordered by ObservedAt, and mirrors them to an
// append-only JSON lines file when a path is configured.
type Store struct {
	mu      sync.Mutex
	path    string
	now     func() time.Time
	samples []Sample
	last    map[seriesKey]Sample
	// fileLines counts lines in the backing file, live or not, to decide when to compact.
	fileLines int
}

// New creates a store backed by path. An empty path keeps history in memory
// only. Existing history is loaded; a missing or unreadable file starts empty.
func New(path string) *Store {
	s := &Store{path: path, now: time.Now, last: make(map[seriesKey]Sample)}
	s.load()
	return s
}

func (s *Store) load() {
	if s.path == "" {
		return
	}
	file, errOpen := os.Open(s.path)
	if errOpen != nil {
		if !errors.Is(errOpen, os.ErrNotExist) {
			log.WithError(errOpen).Warn("quota history: failed to open history file")
		}
		return
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("quota history: failed to close history file")
		}
	}()

	cutoff := s.now().Add(-Retention)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		s.fileLines++
		var sample Sample
		if errDecode := json.Unmarshal(scanner.Bytes(), &sample); errDecode != nil {
			continue
		}
		if sample.ObservedAt.Before(cutoff) {
			continue
		}
		s.samples = append(s.samples, sample)
	}
	if errScan := scanner.Err(); errScan != nil {
		log.WithError(errScan).Warn("quota history: failed to read history file")
	}
	sort.SliceStable(s.samples, func(i, j int) bool {
		return s.samples[i].ObservedAt.Before(s.samples[j].ObservedAt)
	})
	if len(s.samples) > MaxSamples {
		s.samples = append([]Sample(nil), s.samples[len(s.samples)-MaxSamples:]...)
	}
	for _, sample := range s.samples {
		s.last[keyOf(sample)] = sample
	}
}

// Normalize validates and cleans a sample. It reports false when the sample
// must be rejected.
func Normalize(sample Sample, now time.Time) (Sample, bool) {
	sample.Name = strings.TrimSpace(sample.Name)
	sample.AuthIndex = strings.TrimSpace(sample.AuthIndex)
	sample.Provider = strings.ToLower(strings.TrimSpace(sample.Provider))
	sample.WindowID = strings.TrimSpace(sample.WindowID)
	if sample.Name == "" || len(sample.Name) > maxNameLength || len(sample.AuthIndex) > maxNameLength {
		return Sample{}, false
	}
	if sample.Provider == "" || len(sample.Provider) > maxProviderLength {
		return Sample{}, false
	}
	if len(sample.WindowID) > maxWindowIDLength {
		return Sample{}, false
	}
	if math.IsNaN(sample.RemainingPercent) || math.IsInf(sample.RemainingPercent, 0) {
		return Sample{}, false
	}
	sample.RemainingPercent = math.Min(100, math.Max(0, sample.RemainingPercent))
	if sample.PeriodHours != nil {
		period := *sample.PeriodHours
		if math.IsNaN(period) || math.IsInf(period, 0) || period <= 0 {
			sample.PeriodHours = nil
		}
	}
	if sample.ResetAt != nil && sample.ResetAt.IsZero() {
		sample.ResetAt = nil
	}
	if sample.ObservedAt.IsZero() {
		sample.ObservedAt = now
	}
	if sample.ObservedAt.After(now.Add(MaxFutureSkew)) || sample.ObservedAt.Before(now.Add(-Retention)) {
		return Sample{}, false
	}
	sample.ObservedAt = sample.ObservedAt.UTC()
	if sample.ResetAt != nil {
		reset := sample.ResetAt.UTC()
		sample.ResetAt = &reset
	}
	return sample, true
}

func sameReset(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// isDuplicate reports whether next adds nothing over the series' last sample.
func isDuplicate(previous, next Sample) bool {
	if !next.ObservedAt.After(previous.ObservedAt) {
		// Same instant or older than the series head: an echo or a late
		// arrival. Series only grow forward, which keeps the throttle honest.
		return true
	}
	return next.ObservedAt.Sub(previous.ObservedAt) < throttleInterval &&
		next.RemainingPercent-previous.RemainingPercent < resetJump &&
		sameReset(previous.ResetAt, next.ResetAt)
}

// Add validates, deduplicates and stores samples, stamping them with source.
// It returns how many samples were accepted.
func (s *Store) Add(source string, samples ...Sample) int {
	if s == nil || len(samples) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	accepted := make([]Sample, 0, len(samples))
	for _, raw := range samples {
		sample, ok := Normalize(raw, now)
		if !ok {
			continue
		}
		sample.Source = source
		key := keyOf(sample)
		if previous, exists := s.last[key]; exists && isDuplicate(previous, sample) {
			continue
		}
		s.last[key] = sample
		accepted = append(accepted, sample)
	}
	if len(accepted) == 0 {
		return 0
	}

	s.insertLocked(accepted)
	s.pruneLocked(now)
	s.persistLocked(accepted)
	return len(accepted)
}

// insertLocked merges samples into the ordered slice. Most samples are "now",
// so the common case is a plain append.
func (s *Store) insertLocked(accepted []Sample) {
	for _, sample := range accepted {
		n := len(s.samples)
		if n == 0 || !sample.ObservedAt.Before(s.samples[n-1].ObservedAt) {
			s.samples = append(s.samples, sample)
			continue
		}
		index := sort.Search(n, func(i int) bool { return s.samples[i].ObservedAt.After(sample.ObservedAt) })
		s.samples = slices.Insert(s.samples, index, sample)
	}
}

func (s *Store) pruneLocked(now time.Time) {
	cutoff := now.Add(-Retention)
	drop := sort.Search(len(s.samples), func(i int) bool { return !s.samples[i].ObservedAt.Before(cutoff) })
	if overflow := len(s.samples) - drop - MaxSamples; overflow > 0 {
		drop += overflow
	}
	if drop == 0 {
		return
	}
	s.samples = append([]Sample(nil), s.samples[drop:]...)
	// Forget series whose last sample was evicted, so the map stays bounded too.
	for key, sample := range s.last {
		if sample.ObservedAt.Before(cutoff) {
			delete(s.last, key)
		}
	}
}

func (s *Store) persistLocked(accepted []Sample) {
	if s.path == "" {
		return
	}
	// Rewrite instead of appending once the file is mostly dead lines.
	if s.fileLines+len(accepted) > 2*len(s.samples)+1000 {
		if errCompact := s.compactLocked(); errCompact != nil {
			log.WithError(errCompact).Warn("quota history: failed to compact history file")
		}
		return
	}
	if errAppend := s.appendLocked(accepted); errAppend != nil {
		log.WithError(errAppend).Warn("quota history: failed to append history file")
	}
}

func (s *Store) appendLocked(accepted []Sample) (err error) {
	if errDir := os.MkdirAll(filepath.Dir(s.path), 0o700); errDir != nil {
		return errDir
	}
	file, errOpen := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := file.Close(); errClose != nil && err == nil {
			err = errClose
		}
	}()
	writer := bufio.NewWriter(file)
	if errWrite := writeLines(writer, accepted); errWrite != nil {
		return errWrite
	}
	if errFlush := writer.Flush(); errFlush != nil {
		return errFlush
	}
	s.fileLines += len(accepted)
	return nil
}

func (s *Store) compactLocked() (err error) {
	if errDir := os.MkdirAll(filepath.Dir(s.path), 0o700); errDir != nil {
		return errDir
	}
	tmp := s.path + ".tmp"
	file, errOpen := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if errOpen != nil {
		return errOpen
	}
	writer := bufio.NewWriter(file)
	errWrite := writeLines(writer, s.samples)
	if errWrite == nil {
		errWrite = writer.Flush()
	}
	if errClose := file.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		_ = os.Remove(tmp)
		return errWrite
	}
	if errRename := os.Rename(tmp, s.path); errRename != nil {
		return fmt.Errorf("replace history file: %w", errRename)
	}
	s.fileLines = len(s.samples)
	return nil
}

func writeLines(writer *bufio.Writer, samples []Sample) error {
	encoder := json.NewEncoder(writer)
	for _, sample := range samples {
		if errEncode := encoder.Encode(sample); errEncode != nil {
			return errEncode
		}
	}
	return nil
}

// Query returns samples observed at or after since, optionally for one
// provider, oldest first.
func (s *Store) Query(since time.Time, provider string) []Sample {
	if s == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	s.mu.Lock()
	defer s.mu.Unlock()
	start := sort.Search(len(s.samples), func(i int) bool { return !s.samples[i].ObservedAt.Before(since) })
	out := make([]Sample, 0, len(s.samples)-start)
	for _, sample := range s.samples[start:] {
		if provider != "" && sample.Provider != provider {
			continue
		}
		out = append(out, sample)
	}
	return out
}

// Len reports the number of stored samples.
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.samples)
}
