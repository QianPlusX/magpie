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
	// Kind: why it failed (rate limit, no such model, upstream…), so the
	// page can say something more useful than "failed"
	Kind Kind `json:"kind,omitempty"`
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
	// Soon: the soonest renewal of this provider's account window, unix
	// seconds — quota about to renew is spent first (routes.Smart's Soon)
	Soon    int64    `json:"soon,omitempty"`
	Inputs  []string `json:"inputs,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
	// Rate: what a request costs of the subscription's credits, when the
	// vendor lists it (catalog's 0.08×)
	Rate float64 `json:"rate,omitempty"`
	// Answering: the gateway is serving a request on it right now
	Answering bool `json:"answering,omitempty"`
	// RestUntil: resting until then after a failure
	RestUntil time.Time `json:"restUntil,omitempty"`
	// Accounts: 多账号/多 Key 供应商里每个账号自己的冷却，键是网关的
	// candidate.rest（"provider@user" / "provider#keyid"）。一个账号失败只让
	// 它自己歇着，网关接着试下一个账号；都失败才走模型级的计数与禁用
	Accounts map[string]time.Time `json:"accounts,omitempty"`
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
	case e.TTFT <= 0 || e.Latency <= 0 || e.Context <= 0:
		// 首字、延迟、上下文缺一项就不算可用。速度不在这里：有的上游一次
		// 回完、不给分块，速度测不出来，但模型明明是通的——算进这儿会让它
		// 永远停在「检测中」，被诊断器每 5 秒重测一次（kiro/glm-5 卡住就是这个）
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
func (s *Store) Succeeded(provider, model, rest string, ttft, tps, latency float64) {
	s.Update(provider, model, func(e *Entry) {
		// 这个账号自己答上了：它先前的冷却不必等到期
		if rest != "" && rest != provider && e.Accounts != nil {
			delete(e.Accounts, rest)
		}
		e.Failures = 0
		e.LastError = ""
		e.DisabledUntil = time.Time{}
		e.RestUntil = time.Time{}
		e.LastOK = time.Now()
		e.LastTry = e.LastOK
		// 供应商页的「测试模型」只知道总耗时，量不出首字与 token 速度：
		// 那些数留 0 时保留上一次诊断出来的值，别把它清成「—」。
		if ttft > 0 {
			e.TTFT = ttft
		}
		if tps > 0 {
			e.TPS = tps
		}
		if latency > 0 {
			e.Latency = latency
		}
	})
}

// Failed records a failure. A counted one rests CoolFor, and
// FailuresBeforeDisabled in a row take the model out for DisabledFor; an
// uncounted one (a key missing, a model the provider doesn't serve) keeps
// the reason and the time only, so a misconfigured magpie never disables
// every model in a row.
func (s *Store) Failed(provider, model, rest string, f Failure) {
	s.Update(provider, model, func(e *Entry) {
		now := time.Now()
		if f.Reason == "" {
			f.Reason = string(f.Kind)
		}
		// 中断不是模型不可用（上游说完一半断开，或客户端自己取消）：
		// 记下这次错，但不动状态、不清指标、不冷却
		if f.Kind == KindInterrupted {
			e.LastError = f.Reason
			e.LastTry = now
			e.Kind = f.Kind
			return
		}
		e.LastError = f.Reason
		e.LastTry = now
		e.TTFT, e.TPS = 0, 0
		e.Kind = f.Kind
		// 多账号供应商：一个账号失败不该把整个模型踢出候选池，只让它自己歇一会儿，
		// 网关接着试下一个账号；都失败时这一轮才真的没有候选，连续失败照常累计
		// （成功一次就清零）。单账号时照旧，模型自己就是那个账号。
		multi := rest != "" && rest != provider
		if multi {
			if e.Accounts == nil {
				e.Accounts = map[string]time.Time{}
			}
			e.Accounts[rest] = now.Add(f.Rest())
		}
		// 只有真·抖动（上游故障/超时/未知）累计连续失败；其余按自己的时长
		// 歇一会儿就回候选，不该把一次配置错误算成模型坏了
		if f.Counts() {
			e.Failures++
			if e.Failures >= FailuresBeforeDisabled {
				e.DisabledUntil = now.Add(DisabledFor)
				return
			}
		}
		if !multi {
			e.RestUntil = now.Add(f.Rest())
		}
	})
}

// SetExcluded takes a model out of the running, or puts it back.
func (s *Store) SetExcluded(provider, model string, excluded bool) {
	s.Update(provider, model, func(e *Entry) { e.Excluded = excluded })
}

// ClearRest 结束一次冷却/禁用并把连续失败清零：手动按钮用，让一个
// 已经确定可用的模型不用干等 30 秒（或 24 小时）。
func (s *Store) ClearRest(provider, model string) {
	s.Update(provider, model, func(e *Entry) {
		e.RestUntil = time.Time{}
		e.DisabledUntil = time.Time{}
		e.Failures = 0
		e.LastError = ""
	})
}

// SetSoon records when this model's allowance window renews next.
func (s *Store) SetSoon(provider, model string, unix int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(provider, model)]
	if !ok {
		return
	}
	if e.Soon != unix {
		e.Soon = unix
	}
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

// SetAnswering marks the model the gateway is serving right now. Kept in
// memory only: nothing to remember across a restart.
func (s *Store) SetAnswering(provider, model string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(provider, model)]
	if !ok {
		if !on {
			return
		}
		e = s.get(provider, model, time.Now())
	}
	e.Answering = on
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
	Rate            float64
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
			// 新模型一律先算「不参与」：自动诊断只碰参与中的模型，
			// 用户手动加入之前不该有任何请求发出去。
			e = &Entry{Provider: r.Provider, Model: r.Model, Joined: time.Now(), Excluded: true}
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
		if r.Rate > 0 && e.Rate != r.Rate {
			e.Rate = r.Rate
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
			if sa, sb := Score(a), Score(b); sa != sb {
				return sa > sb
			}
		}
		return a.Joined.Before(b.Joined)
	})
}

// Score 是模型打分：倍率 30 + 首字 25 + 速度 30 + 上下文 15 + 整体延迟 10，
// 满分 110。页面上的列表顺序与 group/smart 的成员顺序都按它排。
func Score(e SnapshotEntry) int {
	s := 0
	switch {
	case e.Free && e.Rate <= 0:
		s += 30
	case e.Rate <= 0:
		// 倍率未知：不给分，免得白送
	case e.Rate <= 0.5:
		s += 20
	case e.Rate <= 1:
		s += 15
	case e.Rate <= 3:
		s += 10
	case e.Rate <= 5:
		s += 5
	}
	if t := e.TTFT; t > 0 {
		switch {
		case t <= 1000:
			s += 25
		case t <= 1500:
			s += 20
		case t <= 2000:
			s += 15
		case t <= 2500:
			s += 10
		case t <= 3000:
			s += 5
		}
	}
	switch {
	case e.TPS > 100:
		s += 30
	case e.TPS >= 50:
		s += 25
	case e.TPS >= 40:
		s += 20
	case e.TPS >= 30:
		s += 15
	case e.TPS >= 20:
		s += 10
	case e.TPS >= 15:
		s += 5
	}
	switch {
	case e.Context >= 900000:
		s += 15
	case e.Context >= 500000:
		s += 10
	case e.Context >= 100000:
		s += 5
	}
	if l := e.Latency; l > 0 {
		switch {
		case l <= 1500:
			s += 10
		case l <= 2000:
			s += 8
		case l <= 2500:
			s += 6
		case l <= 3000:
			s += 4
		case l <= 4000:
			s += 2
		}
	}
	return s
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

// AccountCooling: 多账号供应商里这个账号/Key 自己还在上一次失败的冷却里。
// rest 是网关的 candidate.rest；没有记录的账号不算（它没失败过，或者已经
// 过了自己的冷却）。模型级的 Excluded/Disabled/Cooling 由 ExcludedOrCooling 管。
func (s *Store) AccountCooling(provider, model, rest string, now time.Time) bool {
	if rest == "" || rest == provider {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(provider, model)]
	if !ok || e.Accounts == nil {
		return false
	}
	until, ok := e.Accounts[rest]
	return ok && now.Before(until)
}
