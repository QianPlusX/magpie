package gui

import (
	"encoding/json"
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
	for _, e := range provider.Catalog() {
		if e.Group != "" {
			continue // a routing group is not a model to measure
		}
		refs = append(refs, smartroute.Ref{Provider: e.Provider.ID, Model: e.ID,
			Context: e.Context, Free: e.Free, Inputs: e.Inputs, Outputs: e.Outputs})
	}
	store.Ensure(refs)
	return store.Snapshot(now)
}

func smartrouteRoutes(mux *http.ServeMux) {
	startSmartrouteDiagnoser()
	startSmartrouteBusy()
	mux.HandleFunc("GET /api/smartroute", func(rw http.ResponseWriter, r *http.Request) {
		now := time.Now()
		writeJSON(rw, map[string]any{
			"now":     now.UnixMilli(),
			"entries": smartrouteSnapshot(now),
		})
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
		for _, m := range p.Models {
			store.SetBusy(p.ID, m.ID, busy)
		}
	}
}
