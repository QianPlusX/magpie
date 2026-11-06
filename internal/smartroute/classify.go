package smartroute

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Kind 是失败的类别：同一个「失败」按原因走不同的冷却。
// 只有真·抖动的三类（上游故障、超时、未知）累计连续失败；其余各自计时，
// 免得一次配置错误把模型送进 24 小时小黑屋。
type Kind string

const (
	KindUnknown     Kind = ""
	KindRateLimited Kind = "rate_limited"
	KindAuth        Kind = "auth"
	KindNoModel     Kind = "no_model"
	KindBadRequest  Kind = "bad_request"
	KindContent     Kind = "content"
	KindUpstream    Kind = "upstream"
	KindTimeout     Kind = "timeout"
	// KindInterrupted 是「说到一半断了」：上游流中途断开，或客户端自己取消。
	// 它不说明模型不可用，所以既不冷却也不累计连续失败。
	KindInterrupted Kind = "interrupted"
)

// Failure 是一次失败的描述：什么类别、给人看的一句话、要不要听上游的 Retry-After。
type Failure struct {
	Kind       Kind
	Reason     string
	RetryAfter time.Duration
}

// restFor 是每类失败冷却多久。改时长只改这张表。
var restFor = map[Kind]time.Duration{
	KindRateLimited: 30 * time.Second,
	KindAuth:        10 * time.Minute,
	KindNoModel:     24 * time.Hour,
	KindBadRequest:  5 * time.Minute,
	KindContent:     time.Minute,
	KindUpstream:    30 * time.Second,
	// 超时给两分钟：探针自己最长等 45 秒，30 秒冷却等于刚超时就又戳一次，
	// 慢模型会一直在「诊断中」来回测
	KindTimeout: 2 * time.Minute,
	KindUnknown: 30 * time.Second,
	KindInterrupted: 0,
}

// counted 决定哪些类别累计进「连续 FailuresBeforeDisabled 次 → 禁用 24 小时」。
var counted = map[Kind]bool{
	KindUpstream: true,
	KindTimeout:  true,
	KindUnknown:  true,
}

// Classify 按状态码和响应体判断失败类别。body 只看前 500 字节。
func Classify(status int, header http.Header, body []byte) Failure {
	text := strings.ToLower(string(headline(body)))
	f := Failure{Kind: KindUnknown, Reason: reasonOf(status, body)}

	switch {
	case status == 429:
		f.Kind = KindRateLimited
		f.RetryAfter = retryAfter(header)
	case status == 401 || status == 402 || status == 403:
		f.Kind = KindAuth
	case status == 400 || status == 404 || status == 422:
		switch {
		case hasAny(text, "service info not found", "model not found", "invalid model", "model_not_found", "does not exist", "no such model", "unsupported model"):
			f.Kind = KindNoModel
		case hasAny(text, "content_filter", "content filter", "safety", "moderation", "blocked by"):
			f.Kind = KindContent
		default:
			f.Kind = KindBadRequest
		}
	case status >= 500:
		f.Kind = KindUpstream
		f.RetryAfter = retryAfter(header)
	}
	return f
}

// Timeout 是连接层面的失败：连不上、读超时。调用点自己造，没有状态码。
func Timeout(reason string) Failure {
	return Failure{Kind: KindTimeout, Reason: reason}
}

// Interrupted 是「说到一半断了」：只给检测用。真实请求半路断开仍按
// Classify 的结论正常失败（用户：中断不报失败只针对智能路由的检测行为）。
func Interrupted(reason string) Failure {
	return Failure{Kind: KindInterrupted, Reason: reason}
}

// FromError 把检测请求的错误分成两类：连接被切断/上下文取消算中断
// （探针断开是正常操作，不改变模型状态）；连不上、超时才是真失败。
func FromError(err error) Failure {
	if err == nil {
		return Failure{}
	}
	msg := err.Error()
	if errors.Is(err, context.Canceled) || hasAny(strings.ToLower(msg), "connection lost", "connection reset", "unexpected eof", "broken pipe", "stream closed", "client disconnected", "mid-response") {
		return Interrupted(msg)
	}
	return Timeout(msg)
}

// Rest 是这类失败该歇多久：RetryAfter 优先，其次表里的值。
func (f Failure) Rest() time.Duration {
	if f.Kind == KindInterrupted {
		return 0
	}
	if f.RetryAfter > 0 {
		return f.RetryAfter
	}
	if d := restFor[f.Kind]; d > 0 {
		return d
	}
	return CoolFor
}

// Counts 表示这次失败要不要累计进连续失败次数。
func (f Failure) Counts() bool { return counted[f.Kind] }

func hasAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// headline 取响应体的第一行（够判断，也够显示），最多 500 字节。
func headline(body []byte) []byte {
	if i := bytes.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	if len(body) > 500 {
		body = body[:500]
	}
	return body
}

func reasonOf(status int, body []byte) string {
	line := strings.TrimSpace(string(headline(body)))
	if len(line) > 200 {
		line = line[:200]
	}
	if line == "" {
		if s := strings.TrimSpace(http.StatusText(status)); s != "" {
			return "HTTP " + strconv.Itoa(status) + ": " + s
		}
		return "HTTP " + strconv.Itoa(status)
	}
	return "HTTP " + strconv.Itoa(status) + ": " + line
}

// retryAfter 读上游给的秒数；没有或读不懂就交给表里的默认值。
func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 3600 {
		return time.Duration(n) * time.Second
	}
	return 0
}
