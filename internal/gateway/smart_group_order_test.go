package gateway

import (
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// 智能路由页把候选按分数排，网关也必须照这个顺序试。providers.json 里
// group/smart 的 Routing 是空的（=Smart），group 级再按额度重置重排一遍
// 就会把页面第一名的免费模型永远挤到后面。
func TestSmartGroupKeepsListedOrder(t *testing.T) {
	forgetSinks(t)
	old := allowances
	defer func() { allowances = old }()
	// a 的额度二十天后才重置，b 的一小时后重置：Smart 会把 b 提前
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

	// the group's own level, before planGroup has a say: Smart reweighs it
	var pl planned
	var asides []candidate
	var wAsides []Weighed
	leveled := planLevel(provider.Group{ID: "smart", Name: "智能路由"}, ms, 0, provider.Chat, &pl, &asides, &wAsides, nil)
	if got := ids(leveled); len(got) != 2 || got[0] != "b" {
		t.Fatalf("planLevel order %v, want b first (b renews sooner)", got)
	}

	// group/smart as providers.json has it (Routing empty): the page's order stands
	s := &Server{}
	out, _ := s.planGroup(provider.Group{ID: "smart", Name: "智能路由"}, ms, provider.Chat)
	if got := ids(out); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("group/smart order %v, want a b as listed", got)
	}
}
