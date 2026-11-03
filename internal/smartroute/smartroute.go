// Package smartroute keeps what the smart routing page shows about each
// model: how it answered last time, how many times in a row it failed,
// how long it rests after that, and whether the user took it out of the
// running. The gateway writes it as it routes; the page reads it.
//
// It is a file beside the provider list (smartroute.json), so a restart
// keeps what was learned: the gateway's own resting and failure counts
// live in memory only (internal/gateway).
package smartroute

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

// State is where a model stands in the smart routing page's tabs.
type State string

const (
	// Candidate: ready to be used, the page's 候选中
	Candidate State = "candidate"
	// Diagnosing: failed last time or never tried, the page's 诊断中
	Diagnosing State = "diagnosing"
	// Cooling: resting after a failure, the page's 冷却中
	Cooling State = "cooling"
	// Busy: at its concurrency limit, the page's 并发满
	Busy State = "busy"
	// Disabled: failed too often in a row, the page's 已禁用
	Disabled State = "disabled"
	// Excluded: the user took it out of the running, the page's 未参与
	Excluded State = "excluded"
)

const (
	// FailuresBeforeDisabled: this many failures in a row disable the model
	FailuresBeforeDisabled = 10
	// DisabledFor: how long a disabled model stays out
	DisabledFor = 24 * time.Hour
	// CoolFor: how long a model rests after one failure
	CoolFor = 30 * time.Second
	// DiagnoseBatch and DiagnoseGap: how many models a diagnosing round
	// checks at once, and how long it waits between rounds
	DiagnoseBatch = 5
	DiagnoseGap   = 5 * time.Second
)

// Entry is one model's standing.
type Entry struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Excluded: the user said it takes no part in routing
	Excluded bool `json:"excluded,omitempty"`
	// Failures: failures in a row, reset by an answer
	Failures int `json:"failures,omitempty"`
	// LastError: the last failure's words, what the copy button gives
	LastError string `json:"lastError,omitempty"`
	// LastOK, LastTry: when it last answered, and was last asked
	LastOK  time.Time `json:"lastOk,omitempty"`
	LastTry time.Time `json:"lastTry,omitempty"`
	// TTFT, TPS, Latency: how it answered last time, ms and tokens a second
	TTFT    float64 `json:"ttft,omitempty"`
	TPS     float64 `json:"tps,omitempty"`
	Latency float64 `json:"latency,omitempty"`
	// Context: the window it takes, from the provider list
	Context int `json:"context,omitempty"`
	// Free: its provider's list says it costs nothing; what it takes and
	// makes (text, image, audio…) the type filter and the list show
	Free    bool     `json:"free,omitempty"`
	Inputs  []string `json:"inputs,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
	// RestUntil: resting until then after a failure
	RestUntil time.Time `json:"restUntil,omitempty"`
	// DisabledUntil: out until then, having failed FailuresBeforeDisabled in a row
	DisabledUntil time.Time `json:"disabledUntil,omitempty"`
	// Busy: its concurrency limit is reached
	Busy bool `json:"busy,omitempty"`
	// Joined: when it first appeared, the order a diagnosing round goes in
	Joined time.Time `json:"joined,omitempty"`
}

// Key is the model's id in the store: "provider/model".
func Key(provider, model string) string { return provider + "/" + model }

// StateNow is where the entry stands at now.
func (e *Entry) StateNow(now time.Time) State {
	switch {
	case e.Excluded:
		return Excluded
	case !e.DisabledUntil.IsZero() && now.Before(e.DisabledUntil):
		return Disabled
	case !e.RestUntil.IsZero() && now.Before(e.RestUntil):
		return Cooling
	case e.Busy:
		return Busy
	case e.Failures > 0 || e.LastOK.IsZero():
		return Diagnosing
	}
	return Candidate
}

// Store is smartroute.json, read at start-up and written on every change.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
}

var (
	shared *Store
	once   sync.Once
)

// Open is the store beside the provider list; a failure leaves an empty
// in-memory store (the page still works, it just forgets).
func Open() *Store {
	once.Do(func() {
		shared = &Store{path: filepath.Join(appdir.Config(), "smartroute.json"), entries: map[string]*Entry{}}
		shared.load()
	})
	return shared
}

// OpenAt reads a store at path, for tests.
func OpenAt(path string) *Store {
	s := &Store{path: path, entries: map[string]*Entry{}}
	s.load()
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var entries []*Entry
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	for _, e := range entries {
		if e != nil && e.Provider != "" && e.Model != "" {
			s.entries[Key(e.Provider, e.Model)] = e
		}
	}
}

func (s *Store) save() {
	entries := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	// ponytail: write straight to the file; a torn write loses the page's
	// memory, not anything the gateway needs. Rename if that matters.
	_ = os.WriteFile(s.path, data, 0o644)
}

// Get returns the entry for provider/model, made when it wasn't there.
func (s *Store) Get(provider, model string, now time.Time) *Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(provider, model, now)
}

func (s *Store) get(provider, model string, now time.Time) *Entry {
	k := Key(provider, model)
	e, ok := s.entries[k]
	if !ok {
		e = &Entry{Provider: provider, Model: model, Joined: now}
		s.entries[k] = e
	}
	return e
}

// Update changes one entry and writes the store.
func (s *Store) Update(provider, model string, fn func(e *Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.get(provider, model, time.Now())
	fn(e)
	s.save()
}

// Succeeded records an answer: the failure count resets, the last error
// goes, and the numbers of the answer are kept.
func (s *Store) Succeeded(provider, model string, ttft, tps, latency float64) {
	s.Update(provider, model, func(e *Entry) {
		e.Failures = 0
		e.LastError = ""
		e.DisabledUntil = time.Time{}
		e.RestUntil = time.Time{}
		e.LastOK = time.Now()
		e.LastTry = e.LastOK
		e.TTFT, e.TPS, e.Latency = ttft, tps, latency
	})
}

// Failed records a failure. A counted one rests CoolFor, and
// FailuresBeforeDisabled in a row take the model out for DisabledFor; an
// uncounted one (a key missing, a model the provider doesn't serve) keeps
// the reason and the time only, so a misconfigured magpie never disables
// every model in a row.
func (s *Store) Failed(provider, model, reason string, counted bool) {
	s.Update(provider, model, func(e *Entry) {
		e.LastError = reason
		e.LastTry = time.Now()
		e.TTFT, e.TPS = 0, 0
		if !counted {
			return
		}
		e.Failures++
		if e.Failures >= FailuresBeforeDisabled {
			e.DisabledUntil = time.Now().Add(DisabledFor)
		} else {
			e.RestUntil = time.Now().Add(CoolFor)
		}
	})
}

// SetExcluded takes a model out of the running, or puts it back.
func (s *Store) SetExcluded(provider, model string, excluded bool) {
	s.Update(provider, model, func(e *Entry) { e.Excluded = excluded })
}

// SetBusy marks the model at its concurrency limit, or past it.
func (s *Store) SetBusy(provider, model string, busy bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(provider, model)]
	if !ok {
		if !busy {
			return
		}
		e = s.get(provider, model, time.Now())
	}
	if e.Busy == busy {
		return
	}
	e.Busy = busy
	s.save()
}

// SetContext records the window a model takes, from the provider list.
func (s *Store) SetContext(provider, model string, window int) {
	s.Update(provider, model, func(e *Entry) { e.Context = window })
}

// Ref is a model the provider list knows, with the window it takes.
type Ref struct {
	Provider, Model string
	Context         int
	Free            bool
	Inputs          []string
	Outputs         []string
}

// Ensure makes an entry for every ref not seen yet and keeps their windows,
// writing once: the page lists what the providers page lists.
func (s *Store) Ensure(refs []Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for _, r := range refs {
		if r.Provider == "" || r.Model == "" {
			continue
		}
		k := Key(r.Provider, r.Model)
		e, ok := s.entries[k]
		if !ok {
			e = &Entry{Provider: r.Provider, Model: r.Model, Joined: time.Now()}
			s.entries[k] = e
			changed = true
		}
		if r.Context > 0 && e.Context != r.Context {
			e.Context = r.Context
			changed = true
		}
		if e.Free != r.Free {
			e.Free = r.Free
			changed = true
		}
		if !equalStrings(e.Inputs, r.Inputs) {
			e.Inputs = r.Inputs
			changed = true
		}
		if !equalStrings(e.Outputs, r.Outputs) {
			e.Outputs = r.Outputs
			changed = true
		}
	}
	if changed {
		s.save()
	}
}

// equalStrings says whether two small lists of names are the same.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Snapshot is every entry, in Joined order, with the state at now.
type SnapshotEntry struct {
	*Entry
	State State `json:"state"`
}

func (s *Store) Snapshot(now time.Time) []SnapshotEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SnapshotEntry, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, SnapshotEntry{Entry: e, State: e.StateNow(now)})
	}
	sortEntries(out)
	return out
}

// sortEntries puts the tabs' models in the order the page shows them: the
// ready ones by what answers best (a low first token, a fast one, a wide
// window, a low latency — unknown last), the rest by when they joined.
func sortEntries(es []SnapshotEntry) {
	rank := map[State]int{Candidate: 0, Diagnosing: 1, Cooling: 2, Busy: 3, Disabled: 4, Excluded: 5}
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.State != b.State {
			return rank[a.State] < rank[b.State]
		}
		if a.State == Candidate {
			// a free model first, then the fast ones
			if a.Free != b.Free {
				return a.Free
			}
			// a model that has never answered (TTFT 0) goes last
			if (a.TTFT == 0) != (b.TTFT == 0) {
				return b.TTFT == 0
			}
			if a.TTFT != b.TTFT {
				return a.TTFT < b.TTFT
			}
			if a.TPS != b.TPS {
				return a.TPS > b.TPS
			}
			if a.Context != b.Context {
				return a.Context > b.Context
			}
			if a.Latency != b.Latency {
				return a.Latency < b.Latency
			}
		}
		return a.Joined.Before(b.Joined)
	})
}

// ExcludedOrCooling says whether the gateway should skip the model now.
func (s *Store) ExcludedOrCooling(provider, model string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(provider, model)]
	if !ok {
		return false
	}
	switch e.StateNow(now) {
	case Excluded, Disabled, Cooling:
		return true
	}
	return false
}
