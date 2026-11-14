package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/smartroute"
)

// 智能路由是页面自己的一套规则：候选顺序（页面分数序）就是请求序，网关
// 组这一层不掺任何原版路由的判断——不按额度重排、不参与 Sink、不把休息
// 中的成员挪到最后。原版各组照旧，两边互不影响。
func TestSmartGroupKeepsListedOrder(t *testing.T) {
	forgetSinks(t)
	old := allowances
	defer func() { allowances = old }()
	// a 的额度二十天后才重置，b 的一小时后重置：原版 Smart 会把 b 提前
	settled := map[string]provider.Allowance{
		"late@x.com":  {{Used: 5, Span: 30 * 24 * time.Hour, Resets: time.Now().Add(20 * 24 * time.Hour)}},
		"early@x.com": {{Used: 5, Span: 30 * 24 * time.Hour, Resets: time.Now().Add(time.Hour)}},
	}
	allowances = func(string) map[string]provider.Allowance { return settled }
	mem := func(id, user string) provider.Member {
		return provider.Member{ID: id + "/m", Path: []string{id + "/m"},
			Provider: provider.Provider{ID: id, Chat: "http://127.0.0.1:1/v1", Key: "k-" + id,
				Account: &provider.Account{Agent: "workbuddy", User: user}},
			Model: "m"}
	}
	ms := []provider.Member{mem("a", "late@x.com"), mem("b", "early@x.com")}
	ids := func(cs []candidate) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.p.ID)
		}
		return out
	}
	s := &Server{}
	// 智能路由的成员本来就来自页面自己的 store，状态机也只认 store 里列着的
	// 模型：这两个假的模型先放进去，上一次跑剩的冷却先清掉
	for _, id := range []string{"a", "b"} {
		smartroute.Open().Get(id, "m", time.Now())
		smartroute.Open().ClearRest(id, "m")
		defer smartroute.Open().Forget(id, "m")
	}

	// a group of the user's, on Smart: the original rule still weighs it
	var pl planned
	var asides []candidate
	var wAsides []Weighed
	leveled := planLevel(provider.Group{ID: "g", Name: "G"}, ms, 0, provider.Chat, &pl, &asides, &wAsides, nil)
	if got := ids(leveled); len(got) != 2 || got[0] != "b" {
		t.Fatalf("original group order %v, want b first (b renews sooner)", got)
	}

	// the smart page's pool: the page's order stands, whoever renews sooner
	out, _ := s.planGroup(provider.Group{ID: smartGroupID, Name: "智能路由"}, ms, provider.Chat)
	if got := ids(out); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("group/smart order %v, want a b as listed", got)
	}

	// Sink on and a member rate limited: the original rule sends it last and
	// keeps it in the list. The smart page's pool never sinks; a 429 on an
	// account is the shared rest table's business, not a routing rule, so that
	// account is skipped where it stands and the others keep their places.
	rate := []byte(`{"error":{"message":"Too many requests"}}`)
	if r := s.restAfter(out[0], 429, http.Header{}, rate); r.Why != failRate {
		t.Fatalf("rest: %s", r.Why)
	}
	rested(out[0].restKey())
	sunk, _ := s.planGroup(provider.Group{ID: smartGroupID, Name: "智能路由", Sink: true}, ms, provider.Chat)
	if got := ids(sunk); len(got) != 1 || got[0] != "b" {
		t.Fatalf("group/smart order %v after a 429, want just b", got)
	}
}
