package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/smartroute"
)

// 真实请求失败要进智能路由的状态机：页面上的「冷却中」只认 store，而
// 流式/主路径的失败全走 restAfterMarked——以前那里从不写 smartroute，
// 于是模型失败多少次都还挂在候选里。
func TestFailedRequestCoolsTheSmartEntry(t *testing.T) {
	const pid, model = "probeco", "m1"
	store := smartroute.Open()
	store.Get(pid, model, time.Now()) // 组员本来就在 store 里（ensureSmartGroup 建）
	store.ClearRest(pid, model)
	defer store.Forget(pid, model)

	p := provider.Provider{ID: pid, Chat: "http://127.0.0.1:1/v1", Key: "k"}
	c := candidate{p: p, model: model, rest: pid}
	body := []byte(`{"error":{"message":"Too many requests"}}`)
	s := &Server{}
	restAfter := s.restAfterMarked(c, 429, http.Header{}, body, false)
	if restAfter.Why == "" {
		t.Fatalf("429 was not marked as a rest: %+v", restAfter)
	}

	snap := store.Snapshot(time.Now())
	for _, e := range snap {
		if e.Provider != pid || e.Model != model {
			continue
		}
		if e.State != smartroute.Cooling {
			t.Fatalf("state %v after a 429, want cooling (lastError %q)", e.State, e.LastError)
		}
		if !e.CoolingUntil.After(time.Now()) {
			t.Fatalf("coolingUntil %v is not in the future", e.CoolingUntil)
		}
		return
	}
	t.Fatalf("no snapshot entry for %s/%s", pid, model)
}
