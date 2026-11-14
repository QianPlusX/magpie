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
	// starts, and how long it waits between rounds.
	DiagnoseBatch = 12
	DiagnoseGap   = 5 * time.Second
	// DiagnoseInFlight: how many probes may be out at once across all rounds.
	// Rounds no longer wait for the batch before starting the next one, so a
	// slow model doesn't hold the queue back — this is the only ceiling.
	DiagnoseInFlight = 24
	// MinContext: 窗口比这更小的模型，检测成功后自动移出参与。小窗口撑不住
	// 正常对话，留在候选里只会让请求频繁改道；页面上仍可手动「重新参与」。
	MinContext = 800_000
)

// Entry is one model's standing.
// Sample is one measurement: what the model took to start answering, how
// fast it wrote, and how long the whole thing took.
type Sample struct {
	TTFT    float64 `json:"ttft,omitempty"`
	TPS     float64 `json:"tps,omitempty"`
	Latency float64 `json:"latency,omitempty"`
}

// recentSamples is how many measurements the average keeps.
const recentSamples = 10

// firstOf is the first of the two that was measured.
func firstOf(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}

// average answers one field's mean: all of them under ten measurements,
// the middle ones above (the largest and the smallest dropped), so one slow
// or fast answer does not move the ranking.
func (e *Entry) average(pick func(Sample) float64) float64 {
	vals := make([]float64, 0, len(e.Recent))
	for _, sm := range e.Recent {
		if v := pick(sm); v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	if len(vals) >= recentSamples {
		sort.Float64s(vals)
		vals = vals[1 : len(vals)-1]
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

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
	// Recent: 最近十次答上的首字/速度/延迟。排序用它们的平均值（满十条
	// 去掉最大最小再平均），免得一次抖动把排名带偏
	Recent []Sample `json:"recent,omitempty"`
	// Context: the window it takes, from the provider list
	Context int `json:"context,omitempty"`
	// Free: its provider's list says it costs nothing; what it takes and
	// makes (text, image, audio…) the type filter and the list show
	Free bool `json:"free,omitempty"`
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
	// Seats: 这个模型有几个能试的账号/Key（多账号供应商），网关记失败时
	// 写上；账号全在冷却时模型整体算「冷却中」。单账号是 0。
	Seats int `json:"seats,omitempty"`
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
	case e.allSeatsCooling(now):
		// 多账号/多 Key 供应商：每个账号各歇各的，全都歇着时模型整体
		// 没有可试的账号，页面就该把它放进「冷却中」
		return Cooling
	case e.Busy:
		return Busy
	case e.Failures > 0 || e.LastOK.IsZero():
		return Diagnosing
	case e.TTFT <= 0 || e.Latency <= 0:
		// 只认「答过、量出首字与延迟」。速度与上下文不在这里：有的上游一次
		// 回完、不给分块，速度测不出来；插件供应商的目录也可能没给窗口大小，
		// 两者缺一项就让模型永远停在「检测中」，被诊断器每 5 秒重测一次
		// （kiro/glm-5、Cline 的免费模型都是这么卡住的）
		return Diagnosing
	}
	return Candidate
}

// allSeatsCooling 说这个模型背后的账号是不是全在冷却：Seats 是网关记
// 失败时告诉的账号数（1 表示单账号，走 RestUntil），Accounts 里只有失
// 败过的账号，没失败过的不在里面——所以是「冷却的账号数 >= 账号总数」，
// 而不是「Accounts 里每个都在冷却」。
func (e *Entry) allSeatsCooling(now time.Time) bool {
	if e.Seats <= 0 || len(e.Accounts) == 0 {
		return false
	}
	live := 0
	for _, until := range e.Accounts {
		if now.Before(until) {
			live++
		}
	}
	return live >= e.Seats
}

// coolingUntil 是账号冷却里最晚的一个：模型整体进冷却时页面拿它倒计时。
// 模型级 RestUntil 只管单账号，多账号时它是零值。
func (e *Entry) coolingUntil(now time.Time) time.Time {
	last := e.RestUntil
	for _, until := range e.Accounts {
		if now.Before(until) && until.After(last) {
			last = until
		}
	}
	return last
}

// Store is smartroute.json, read at start-up and written on every change.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
	// trying 记着「这会儿正有探针在测」的模型。一次诊断最长 45 秒，而诊断
	// 轮次每 5 秒就起一批：不挡住的话同一个慢模型会被连着戳好几次（用户：
	// 「使用 key 的供应商出现了反复测试」）。只活在内存里，重启即清空。
	trying map[string]time.Time
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
		// 主文件被截断（App 被杀在写入中间）：退回上一次写完的 .tmp
		if data, err = os.ReadFile(s.path + ".tmp"); err != nil {
			return
		}
		if json.Unmarshal(data, &entries) != nil {
			return
		}
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
	// 先写临时文件再改名：App 被系统杀掉时常正好落在写入中间，直接覆盖会
	// 留下半截 JSON，下次启动解析失败，所有检测记录一起消失
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err == nil {
		_ = os.Rename(tmp, s.path)
	}
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
		if ttft > 0 || tps > 0 || latency > 0 {
			e.Recent = append(e.Recent, Sample{TTFT: ttft, TPS: tps, Latency: latency})
			if len(e.Recent) > recentSamples {
				e.Recent = e.Recent[len(e.Recent)-recentSamples:]
			}
		}
		// 窗口太小的模型检测完就挪进「未参与」：它答得上，但装不下正常对话
		if e.Context > 0 && e.Context < MinContext {
			e.Excluded = true
		}
	})
}

// Failed records a failure. A counted one rests CoolFor, and
// FailuresBeforeDisabled in a row take the model out for DisabledFor; an
// uncounted one (a key missing, a model the provider doesn't serve) keeps
// the reason and the time only, so a misconfigured magpie never disables
// every model in a row.
func (s *Store) Failed(provider, model, rest string, f Failure) {
	s.FailedOf(provider, model, rest, 0, f)
}

// FailedOf 同 Failed，seats 是这个模型背后能试的账号/Key 数（网关给的），
// 账号全在冷却时 StateNow 才算模型整体不可用。
func (s *Store) FailedOf(provider, model, rest string, seats int, f Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 只更新页面上真列着的模型：restAfterMarked 是所有路由共用的记账点，
	// 让一个没进过 store 的模型在这里凭空长条目，池子会被别的路由撑大
	e, ok := s.entries[Key(provider, model)]
	if !ok {
		return
	}
	defer s.save()
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
	e.Kind = f.Kind
	// 多账号供应商：一个账号失败不该把整个模型踢出候选池，只让它自己歇一会儿，
	// 网关接着试下一个账号；都失败时这一轮才真的没有候选，连续失败照常累计
	// （成功一次就清零）。单账号时照旧，模型自己就是那个账号。
	multi := rest != "" && rest != provider
	// 同理别把一个账号的失败算成整个模型没量出首字：另一个账号刚答过，
	// 清掉这个数会让模型退回「检测中」，页面上看着像模型坏了
	if !multi {
		e.TTFT, e.TPS = 0, 0
	}
	if multi {
		if e.Accounts == nil {
			e.Accounts = map[string]time.Time{}
		}
		e.Accounts[rest] = now.Add(f.Rest())
		if seats > 0 {
			e.Seats = seats
		}
	} else {
		// 单账号：模型级冷却就够，别留着上一个多账号时代的座位数
		e.Seats = 0
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
}

// Forget drops one entry: a model gone from the catalog, and tests that
// must not leave made-up models in a store they borrowed.
func (s *Store) Forget(provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := Key(provider, model)
	if _, ok := s.entries[k]; !ok {
		return
	}
	delete(s.entries, k)
	s.save()
}

// SetExcluded takes a model out of the running, or puts it back.
func (s *Store) SetExcluded(provider, model string, excluded bool) {
	s.Update(provider, model, func(e *Entry) { e.Excluded = excluded })
}

// ClearRest 结束一次冷却/禁用并把连续失败清零：手动按钮用，让一个
// 已经确定可用的模型不用干等 30 秒（或 24 小时）。多账号模型连各账号
// 自己的冷却一起清掉，否则页面写着「结束冷却」按了却没反应。
func (s *Store) ClearRest(provider, model string) {
	s.Update(provider, model, func(e *Entry) {
		e.RestUntil = time.Time{}
		e.DisabledUntil = time.Time{}
		e.Accounts = nil
		e.Seats = 0
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

// tryingFor: how long a probe's claim holds when nothing releases it. A probe
// gives up after 45 seconds, so anything older than this is a leftover.
const tryingFor = 2 * time.Minute

// MarkTrying claims the one probe slot a model has. It answers false while a
// probe is still in flight: the diagnoser starts a round every DiagnoseGap
// seconds, and a 45-second probe would otherwise be restarted eight times over
// ("使用 key 的供应商出现了反复测试").
func (s *Store) MarkTrying(provider, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trying == nil {
		s.trying = map[string]time.Time{}
	}
	key := provider + "/" + model
	if at, ok := s.trying[key]; ok && time.Since(at) < tryingFor {
		return false
	}
	s.trying[key] = time.Now()
	return true
}

// DoneTrying releases the slot, so the next round may measure it again.
func (s *Store) DoneTrying(provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.trying, provider+"/"+model)
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
	// Trying: 这会儿真有一个探针在跑。页面靠它决定画不画那颗「正在测」的
	// 闪烁球——「检测中」这个状态只说明上次失败或还没测过，不代表正在测。
	Trying bool `json:"trying,omitempty"`
	// CoolingUntil: 多账号时模型整体进了冷却，账号冷却里最晚的一个；
	// RestUntil 在那种情况是零值，页面拿这个倒计时
	CoolingUntil time.Time `json:"coolingUntil,omitempty"`
	// 排序用的平均值（见 Entry.average），卡片上仍显示最近一次的数
	AvgTTFT    float64 `json:"avgTtft,omitempty"`
	AvgTPS     float64 `json:"avgTps,omitempty"`
	AvgLatency float64 `json:"avgLatency,omitempty"`
}

func (s *Store) Snapshot(now time.Time) []SnapshotEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SnapshotEntry, 0, len(s.entries))
	for _, e := range s.entries {
		at, held := s.trying[e.Provider+"/"+e.Model]
		out = append(out, SnapshotEntry{Entry: e, State: e.StateNow(now),
			Trying:       held && time.Since(at) < tryingFor,
			CoolingUntil: e.coolingUntil(now),
			AvgTTFT:      e.average(func(s Sample) float64 { return s.TTFT }),
			AvgTPS:       e.average(func(s Sample) float64 { return s.TPS }),
			AvgLatency:   e.average(func(s Sample) float64 { return s.Latency })})
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

// Score 是模型打分：倍率 30 + 首字 15 + 速度 30 + 上下文 15 + 整体延迟 10
// + 额度即将重置 15，满分 115。页面上的列表顺序与 group/smart 的成员顺序都按它排。
//
// 「快到到期」这一项是给额度快作废的账号加分：窗口还剩几天就重置，
// 现在不用掉就浪费了。没有额度窗口的（Soon 为零）不加不减。
func Score(e SnapshotEntry) int {
	return ScoreAt(e, time.Now())
}

// ScoreAt 同 Score，只是把「现在」显式传进来，方便测试与复现。
func ScoreAt(e SnapshotEntry, now time.Time) int {
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
	if t := firstOf(e.AvgTTFT, e.TTFT); t > 0 {
		switch {
		case t <= 1000:
			s += 15
		case t <= 1500:
			s += 12
		case t <= 2000:
			s += 9
		case t <= 2500:
			s += 6
		case t <= 3000:
			s += 3
		}
	}
	tps := firstOf(e.AvgTPS, e.TPS)
	switch {
	case tps > 100:
		s += 30
	case tps >= 50:
		s += 25
	case tps >= 40:
		s += 20
	case tps >= 30:
		s += 15
	case tps >= 20:
		s += 10
	case tps >= 15:
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
	if l := firstOf(e.AvgLatency, e.Latency); l > 0 {
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
	if e.Soon > 0 {
		switch days := time.Unix(e.Soon, 0).Sub(now).Hours() / 24; {
		case days <= 3:
			s += 15
		case days <= 7:
			s += 12
		case days <= 14:
			s += 8
		case days <= 30:
			s += 4
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
