package gui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
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
	entries := smartroute.Open().Snapshot(now)
	batch := make([]smartroute.SnapshotEntry, 0, smartroute.DiagnoseBatch)
	for i := range entries {
		e := entries[i]
		if e.State != smartroute.Diagnosing {
			continue
		}
		// 在飞的探针总数封顶：轮次不再等整批回来，慢的那一个不该拖住队列
		if inFlight.Load() >= int32(smartroute.DiagnoseInFlight) {
			break
		}
		// 供应商页没勾的不测：它不会进路由组，测了只是白花额度
		if !provider.Checked(e.Provider + "/" + e.Model) {
			continue
		}
		// tried within the last two rounds: let it be
		if !e.LastTry.IsZero() && now.Sub(e.LastTry) < 2*smartroute.DiagnoseGap {
			continue
		}
		// 45 秒的探针还没回来时别再起一个：不然同一个慢模型会同时挂好几个请求
		if !smartroute.Open().MarkTrying(e.Provider, e.Model) {
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

	// 不等整批：每批只负责起头，探针跑完自己释放槽位。以前 wg.Wait 让一个
	// 45 秒的慢模型把整轮拖住，75 个模型的供应商要测十分钟。
	for _, e := range batch {
		inFlight.Add(1)
		go func(e smartroute.SnapshotEntry) {
			defer inFlight.Add(-1)
			defer smartroute.Open().DoneTrying(e.Provider, e.Model)
			diagnoseOne(context.Background(), e.Provider, e.Model)
		}(e)
	}
}

// inFlight counts the probes out now, across rounds.
var inFlight atomic.Int32

// diagnoseOne 模拟一次 agent 请求：打 magpie 自己的 /v1/chat/completions，
// model 就是列表里那个全名（workbuddy-ai/xxx），前缀由 magpie 分派给供应商。
// 不自己拼 "provider/model"（会变成双前缀），也不调供应商的测试接口。
// base 是这次探针挂在谁的取消上：诊断器给 Background（自己 45 秒超时），
// 供应商页给请求的 ctx —— 这样「停止」按钮真的能停，不必等 45 秒。
func diagnoseOne(base context.Context, providerID, modelID string) (bool, int, int64, float64, string) {
	ctx, cancel := context.WithTimeout(base, 45*time.Second)
	defer cancel()
	// max_tokens 给到 64：太少算不出速度（只有 1 个块就没有"每秒多少块"）。
	// 诊断要的是「这个模型现在能不能用、多快」，多花几十个 token 是值得的。
	// 用全名问网关：裸名会让它按名字随便挑一个供应商，两个供应商有同名模型时
	// 测的就不是这一家了
	full := providerID + "/" + modelID
	// stream_options 让守规矩的上游在最后一块报 usage（真实 token 数）：
	// 有的免费上游只回一块，数块数算不出速度
	body := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":64,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"ping"}]}`, full)
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+gateway.DefaultAddr+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		smartroute.Open().Failed(providerID, modelID, providerID, smartroute.Timeout("构造请求失败："+err.Error()))
		return false, 0, 0, 0, "构造请求失败：" + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Magpie-Diagnose", "1")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 探针被切断（连接中断、上下文取消）不算模型失败；连不上/超时才算
		smartroute.Open().Failed(providerID, modelID, providerID, smartroute.FromError(err))
		return false, 0, 0, 0, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		smartroute.Open().Failed(providerID, modelID, providerID, smartroute.Classify(resp.StatusCode, resp.Header, b))
		return false, resp.StatusCode, time.Since(start).Milliseconds(), 0, strings.TrimSpace(string(b))
	}
	// 第一条 data: 是首字；之后每块算一次输出，块数 / 生成耗时 = 速度
	br := bufio.NewReader(resp.Body)
	var ttft time.Time
	var last time.Time
	chunks := 0
	tokensSeen := 0
	bodyBytes := 0
	var readErr error
	for {
		line, err := br.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			now := time.Now()
			if ttft.IsZero() {
				ttft = now
			}
			last = now
			chunks++
		}
		// 非流式上游一次回一段 JSON，没有 data: 行，token 数在那里面
		if n := completionTokens(line); n > 0 {
			tokensSeen = n
		}
		bodyBytes += len(line)
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
	}
	// 一个字都没读到就断了：这次检测做废，既不记失败也不假装成功
	if readErr != nil && chunks == 0 {
		f := smartroute.FromError(readErr)
		smartroute.Open().Failed(providerID, modelID, providerID, f)
		return false, resp.StatusCode, time.Since(start).Milliseconds(), 0, readErr.Error()
	}
	total := time.Since(start).Milliseconds()
	ttftMs := int64(0)
	if !ttft.IsZero() {
		ttftMs = ttft.Sub(start).Milliseconds()
	}
	if chunks == 0 {
		// 一个 data: 都没有：这不是 SSE，整段一次回来的。首字就是等它
		// 回来的时间，速度用它自报的 token 数算（kiro 走 anthropic 协议，
		// 回的是整段 JSON，之前这里空着，模型就永远停在「检测中」）
		ttftMs = total
	}
	// 首块之后才算 token；块间隔太短（一次性回来）就不报速度，免得写成离谱的几千。
	// 单块最多算 512 token：有的上游把整段答案拆成上千个小块一瞬间吐完，
	// 那样算出来的「tok/s」是假的（历史数据里出现过 9000）。
	// 分块的块数就是个近似：有的上游把整段答案拆成上千个小块，有的只回
	// 一块。上游报了 usage 的 token 数就用它，没有再用块数；连块都没分时
	// 退到总耗时，至少给个数（用户：测试要能测出速度）。
	count := chunks - 1
	if tokensSeen > count {
		count = tokensSeen
	}
	if count <= 0 && bodyBytes > 0 {
		// 一块都没分、也不报 usage：按 4 字节≈1 token 估一个，总比「—」强，
		// 反正只会用来给速度分档（ponytail: 粗略估算，接得上真实 usage 就别用）
		count = bodyBytes / 4
	}
	span := last.Sub(ttft)
	if last.IsZero() || span < 100*time.Millisecond {
		span = time.Duration(total) * time.Millisecond
	}
	tps := 0.0
	if count > 0 && span > 0 {
		n := float64(count)
		if n > 512 {
			n = 512
		}
		tps = n / span.Seconds()
	}
	smartroute.Open().Succeeded(providerID, modelID, providerID, float64(ttftMs), tps, float64(total))
	return true, resp.StatusCode, total, tps, ""
}

// completionTokens reads the token count an upstream puts in its stream's
// usage block: {"usage":{"completion_tokens":64,...}}. 0 when it says none.
func completionTokens(line string) int {
	const key = `"completion_tokens":`
	i := strings.Index(line, key)
	if i < 0 {
		return 0
	}
	rest := strings.TrimSpace(line[i+len(key):])
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:j])
	if err != nil {
		return 0
	}
	return n
}
