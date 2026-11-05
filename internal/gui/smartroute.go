package gui

import (
	"encoding/json"
	"sync"
	"net/http"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/smartroute"
)

// The smart routing page's own state: what each model answered last time,
// how often it failed in a row, and whether the user kept it out. The
// gateway writes it as it routes (internal/smartroute); this is the page's
// way in.
// smartrouteSnapshot is the store, with every model the providers page
// lists ensured an entry (so the page lists them all, untried).
func smartrouteSnapshot(now time.Time) []smartroute.SnapshotEntry {
	store := smartroute.Open()
	refs := make([]smartroute.Ref, 0, 64)
	// 只列供应商那页勾上的模型：catalog 里还有没被选中的，
	// 它们既不参与路由，页面上也不该占地方
	on, onOk := checkedModels()
	for _, e := range provider.Catalog() {
		if e.Group != "" {
			continue // a routing group is not a model to measure
		}
		if onOk && !on[e.ID] {
			continue
		}
		refs = append(refs, smartroute.Ref{Provider: e.Provider.ID, Model: e.Model,
			Context: e.Context, Free: e.Free, Rate: e.Rate,
			Inputs: e.Inputs, Outputs: e.Outputs})
	}
	store.Ensure(refs)
	// 早先版本拿 catalog 的 "provider/model" 当裸模型名存过一批，
	// 拼出来是 "provider/provider/model"：模型名里不会有 "/"，丢掉它们
	out := store.Snapshot(now)
	keep := out[:0]
	for _, e := range out {
		if strings.Contains(e.Model, "/") {
			continue // the old double-prefixed leftovers
		}
		if onOk && !on[e.Provider+"/"+e.Model] {
			continue // not ticked on the providers page: it waits, unlisted
		}
		keep = append(keep, e)
	}
	return keep
}

// smartGroupID is the routing group the smart router serves as one model:
// an agent calls "group/smart" like any model, and the gateway routes it
// over every model there is, fastest measured first. The models the page
// kept out are dropped by the gateway itself (dropSmartExcluded), so
// "不参与" holds for outside apps too.
const smartGroupID = "smart"

// checkedModels are the "provider/model" the providers page has ticked,
// read off its state (the only place the pick lives). Kept for a few
// seconds: the page asks four times a minute.
var (
	checkedMu  sync.Mutex
	checkedAt  time.Time
	checkedSet map[string]bool
)

func checkedModels() (map[string]bool, bool) {
	checkedMu.Lock()
	defer checkedMu.Unlock()
	if checkedSet != nil && time.Since(checkedAt) < 10*time.Second {
		return checkedSet, true
	}
	set := provider.PickedByUser()
	checkedSet, checkedAt = set, time.Now()
	return set, true
}


// ensureSmartGroup keeps that group in step with the catalog: members are
// every model, in the order the smart page ranks them. Saved only when the
// set of models changes, so the file isn't rewritten as latencies drift.
func ensureSmartGroup(now time.Time) {
	entries := smartrouteSnapshot(now)
	members := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.State == smartroute.Excluded {
			continue // kept out of the smart router means out of the group too
		}
		// Entry.ID is already "provider/model"; prefixing the provider
		// again made members that name no model ("no usable member")
		members = append(members, e.Provider+"/"+e.Model)
	}
	if len(members) == 0 {
		return
	}
	// FindGroup takes the catalog id ("group/<id>"), not the bare group id
	if g, _, ok := provider.FindGroup(provider.GroupPrefix + smartGroupID); ok {
		if sameOrder(g.Members, members) {
			return
		}
		// 成员没变、只是分数把顺序改了：60 秒内不重复写盘，
		// 免得每轮诊断都把 providers.json 重写一次
		if sameMembers(g.Members, members) && time.Since(lastGroupWrite) < time.Minute {
			return
		}
	}
	_ = provider.SaveGroup(provider.Group{ID: smartGroupID, Name: "智能路由", Members: members})
	lastGroupWrite = time.Now()
}

// lastGroupWrite: when the smart group was last rewritten, so a reordering
// from a fresh latency reading doesn't write the file every few seconds.
var lastGroupWrite time.Time

// sameOrder: same models in the same order — the request asks them in this
// order, so a reordering has to reach the file.
func sameOrder(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range have {
		if have[i] != want[i] {
			return false
		}
	}
	return true
}

// sameMembers: same set of models, order ignored.
func sameMembers(have, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(have))
	for _, m := range have {
		seen[m] = true
	}
	for _, m := range want {
		if !seen[m] {
			return false
		}
	}
	return true
}

func smartrouteRoutes(mux *http.ServeMux) {
	// 供应商页勾了哪些，网关就只服务哪些
	provider.SetCheckedModels(checkedModels)
	startSmartrouteDiagnoser()
	startSmartrouteBusy()
	go func() {
		// the smart group has to exist before anyone opens the page:
		// the gateway page lists it, and outside apps call it
		ensureSmartGroup(time.Now())
		for range time.Tick(30 * time.Second) {
			ensureSmartGroup(time.Now())
		}
	}()
	mux.HandleFunc("GET /api/smartroute", func(rw http.ResponseWriter, r *http.Request) {
		now := time.Now()
		ensureSmartGroup(now)
		writeJSON(rw, map[string]any{
			"now":     now.UnixMilli(),
			"entries": smartrouteSnapshot(now),
		})
	})

	// 结束冷却：把 RestUntil/DisabledUntil 清掉，模型立刻回到诊断中
	mux.HandleFunc("POST /api/smartroute/reset", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Provider == "" || in.Model == "" {
			http.Error(rw, "provider and model are needed", http.StatusBadRequest)
			return
		}
		smartroute.Open().ClearRest(in.Provider, in.Model)
		now := time.Now()
		writeJSON(rw, map[string]any{"now": now.UnixMilli(), "entries": smartrouteSnapshot(now)})
	})
	mux.HandleFunc("POST /api/smartroute/toggle", func(rw http.ResponseWriter, r *http.Request) {
		var in struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Excluded bool   `json:"excluded"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Provider == "" || in.Model == "" {
			http.Error(rw, "provider and model are needed", http.StatusBadRequest)
			return
		}
		smartroute.Open().SetExcluded(in.Provider, in.Model, in.Excluded)
		now := time.Now()
		writeJSON(rw, map[string]any{
			"now":     now.UnixMilli(),
			"entries": smartrouteSnapshot(now),
		})
	})
}

// startSmartrouteBusy keeps the page's 并发满 tab true: every few seconds it
// asks the gateway how many requests each provider has out, against the
// limit its keys or accounts carry, and tells the store.
func startSmartrouteBusy() {
	go func() {
		for {
			time.Sleep(3 * time.Second)
			syncSmartBusy()
		}
	}()
}

func syncSmartBusy() {
	gw := served.Load()
	if gw == nil {
		return
	}
	out := map[string]int{}
	for _, a := range gw.Active() {
		if a.Provider != "" {
			out[a.Provider]++
		}
	}
	// a provider's own lane (not one of its keys or accounts) carries the
	// limit its requests wait on
	limit := map[string]int{}
	for who, ln := range gw.Lanes() {
		if ln.Limit <= 0 || strings.ContainsAny(who, "#@") {
			continue
		}
		limit[who] = ln.Limit
	}
	store := smartroute.Open()
	for _, p := range providersState().Providers {
		busy := limit[p.ID] > 0 && out[p.ID] >= limit[p.ID]
		answering := out[p.ID] > 0
		for _, m := range p.Models {
			store.SetBusy(p.ID, m.ID, busy)
			store.SetAnswering(p.ID, m.ID, answering)
		}
	}
}
