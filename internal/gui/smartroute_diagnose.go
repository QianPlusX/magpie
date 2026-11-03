package gui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/smartroute"
)

// The smart routing page's diagnoser: every DiagnoseGap it takes the
// models that stand in 诊断中 (failed last time, or never tried) and asks
// the local gateway for one tiny streamed answer each, DiagnoseBatch at a
// time, measuring time to first token, output speed and total latency.
//
// A misconfigured magpie (no key, a model its provider doesn't serve)
// fails every model the same way: that batch doesn't count against the
// models, and the round stands off for a while instead.

var diagnoseOffUntil atomic.Int64

func startSmartrouteDiagnoser() {
	go func() {
		for {
			time.Sleep(smartroute.DiagnoseGap)
			diagnoseBatch()
		}
	}()
}

func diagnoseBatch() {
	now := time.Now()
	if until := diagnoseOffUntil.Load(); until > now.UnixMilli() {
		return
	}
	entries := smartroute.Open().Snapshot(now)
	batch := make([]smartroute.SnapshotEntry, 0, smartroute.DiagnoseBatch)
	for i := range entries {
		e := entries[i]
		if e.State != smartroute.Diagnosing {
			continue
		}
		// tried within the last two rounds: let it be
		if !e.LastTry.IsZero() && now.Sub(e.LastTry) < 2*smartroute.DiagnoseGap {
			continue
		}
		batch = append(batch, e)
		if len(batch) >= smartroute.DiagnoseBatch {
			break
		}
	}
	if len(batch) == 0 {
		return
	}

	var wg sync.WaitGroup
	var bad, total int32
	for _, e := range batch {
		wg.Add(1)
		go func(e smartroute.SnapshotEntry) {
			defer wg.Done()
			if !diagnoseOne(e.Provider, e.Model) {
				atomic.AddInt32(&bad, 1)
			}
			atomic.AddInt32(&total, 1)
		}(e)
	}
	wg.Wait()

	// a whole batch the same way, and not a model's own answer: hold off
	if total >= 3 && bad == total {
		diagnoseOffUntil.Store(time.Now().Add(10 * time.Minute).UnixMilli())
	}
}

// diagnoseOne asks the gateway for the smallest streamed answer and records
// what it took. It returns false when the whole gateway refused (the batch
// is then held off), true for anything the model itself answered.
func diagnoseOne(providerID, modelID string) bool {
	body, _ := json.Marshal(map[string]any{
		"model":      providerID + "/" + modelID,
		"messages":   []map[string]string{{"role": "user", "content": "ok"}},
		"max_tokens": 1,
		"stream":     true,
	})
	start := time.Now()
	req, err := http.NewRequest("POST", "http://"+gateway.DefaultAddr+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return true
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Magpie-Diagnose", "1")
	client := &http.Client{Timeout: 45 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		smartroute.Open().Failed(providerID, modelID, "无法连接网关："+err.Error(), true)
		return true
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
		// 4xx is about the configuration or the model, not this attempt:
		// keep the words, don't strike the model out
		smartroute.Open().Failed(providerID, modelID, msg, resp.StatusCode >= 500)
		return resp.StatusCode < 500
	}

	ttftMs := 0.0
	tokens := 0
	lines := 0
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			lines++
			if ttftMs == 0 && lines > 0 {
				ttftMs = float64(time.Since(start).Milliseconds())
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "" && payload != "[DONE]" {
				tokens++
			}
		}
		if readErr != nil {
			break
		}
	}
	totalMs := float64(time.Since(start).Milliseconds())
	if lines == 0 {
		smartroute.Open().Failed(providerID, modelID, "网关没有返回内容", false)
		return true
	}
	tps := 0.0
	if totalMs > ttftMs && tokens > 1 {
		tps = float64(tokens-1) / ((totalMs - ttftMs) / 1000)
	}
	smartroute.Open().Succeeded(providerID, modelID, ttftMs, tps, totalMs)
	return true
}
