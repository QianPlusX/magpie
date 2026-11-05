package smartroute

import (
	"net/http"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")

	cases := []struct {
		name   string
		status int
		head   http.Header
		body   string
		want   Kind
		rest   time.Duration
	}{
		{"限流带秒数", 429, h, "", KindRateLimited, 7 * time.Second},
		{"限流没给秒数", 429, nil, "", KindRateLimited, 30 * time.Second},
		{"未授权", 401, nil, `{"error":"invalid key"}`, KindAuth, 10 * time.Minute},
		{"余额不足", 402, nil, "", KindAuth, 10 * time.Minute},
		{"无权限", 403, nil, "", KindAuth, 10 * time.Minute},
		{"模型不存在", 400, nil, `{"message":"model [x] service info not found"}`, KindNoModel, 24 * time.Hour},
		{"模型不存在404", 404, nil, `{"error":{"message":"model not found"}}`, KindNoModel, 24 * time.Hour},
		{"上下文超长", 400, nil, `{"error":"context length exceeded"}`, KindBadRequest, 5 * time.Minute},
		{"内容被拦", 400, nil, `{"error":{"type":"content_filter"}}`, KindContent, time.Minute},
		{"上游500", 500, nil, "boom", KindUpstream, 30 * time.Second},
		{"上游502", 502, nil, "", KindUpstream, 30 * time.Second},
		{"未知4xx", 418, nil, "", KindUnknown, 30 * time.Second},
	}

	for _, c := range cases {
		got := Classify(c.status, c.head, []byte(c.body))
		if got.Kind != c.want {
			t.Errorf("%s: kind = %q, want %q", c.name, got.Kind, c.want)
		}
		if d := got.Rest(); d != c.rest {
			t.Errorf("%s: rest = %v, want %v", c.name, d, c.rest)
		}
		if got.Reason == "" {
			t.Errorf("%s: reason 不能为空", c.name)
		}
	}
}

// 只有真·抖动的三类算连续失败：配置错/限流/账号问题不该把模型送进禁用。
func TestOnlyJitterCounts(t *testing.T) {
	for _, k := range []Kind{KindUpstream, KindTimeout, KindUnknown} {
		if !(Failure{Kind: k}).Counts() {
			t.Errorf("%q 应该计数", k)
		}
	}
	for _, k := range []Kind{KindRateLimited, KindAuth, KindNoModel, KindBadRequest, KindContent} {
		if (Failure{Kind: k}).Counts() {
			t.Errorf("%q 不该计数", k)
		}
	}
}

// 模型不存在不进「已禁用」，只长冷却；上游故障连败 10 次才禁用。
func TestFailureKindsChangeState(t *testing.T) {
	s := OpenAt(t.TempDir())
	join(s, Ref{Provider: "p", Model: "m"})

	s.Failed("p", "m", "p", Classify(400, nil, []byte("model not found")))
	e := s.Snapshot(time.Now())[0]
	if e.State != Cooling || e.Failures != 0 {
		t.Fatalf("no_model: state=%s failures=%d，应为 cooling/0", e.State, e.Failures)
	}
	if d := time.Until(e.RestUntil); d < 23*time.Hour {
		t.Fatalf("no_model 应该歇近 24 小时，实际 %v", d)
	}

	for i := 0; i < FailuresBeforeDisabled; i++ {
		s.Failed("p", "m", "p", Classify(500, nil, []byte("boom")))
	}
	e = s.Snapshot(time.Now())[0]
	if e.State != Disabled {
		t.Fatalf("upstream 连败 %d 次后应是 disabled，实际 %s", FailuresBeforeDisabled, e.State)
	}
}

// 说到一半断掉是正常操作（上游流中断、客户端取消），不该把模型记为失败
func TestInterruptedIsNotAFailure(t *testing.T) {
	// 真实请求中断仍按状态码正常失败：检测路径才用 Interrupted
	real := Classify(502, nil, []byte(`{"error":"WorkBuddy: connection lost mid-response"}`))
	if real.Kind != KindUpstream || real.Rest() == 0 {
		t.Fatalf("真实请求中断应照常失败，实际 %s rest=%v", real.Kind, real.Rest())
	}
	f := Interrupted("WorkBuddy: connection lost mid-response")
	if f.Kind != KindInterrupted {
		t.Fatalf("应归为中断，实际 %s", f.Kind)
	}
	if f.Rest() != 0 || f.Counts() {
		t.Fatalf("中断不该冷却也不该计数，实际 rest=%v counts=%v", f.Rest(), f.Counts())
	}
	s := tempStore(t)
	join(s, Ref{Provider: "wb", Model: "flash"})
	s.Succeeded("wb", "flash", "wb", 300, 42, 1500)
	s.Failed("wb", "flash", "wb", f)
	if e := one(t, s); e.State != Candidate || e.Failures != 0 || e.TPS != 42 {
		t.Fatalf("中断后应仍是候选中并保留指标，实际 %+v", e)
	}
}
