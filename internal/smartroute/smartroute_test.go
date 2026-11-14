package smartroute

import (
	"path/filepath"
	"testing"
	"time"
)

func tempStore(t *testing.T) *Store {
	t.Helper()
	return OpenAt(filepath.Join(t.TempDir(), "smartroute.json"))
}

func one(t *testing.T, s *Store) SnapshotEntry {
	t.Helper()
	got := s.Snapshot(time.Now())
	if len(got) != 1 {
		t.Fatalf("想要 1 个条目，拿到 %d", len(got))
	}
	return got[0]
}

// join 建条目并让它们参与路由。新模型默认「不参与」（不自动检测），
// 排序和状态迁移的用例都要先手动加入才有意义。
func join(s *Store, refs ...Ref) {
	s.Ensure(refs)
	for _, r := range refs {
		s.SetExcluded(r.Provider, r.Model, false)
		// 上下文和首字/延迟/速度一样是「可用」的前提，测试里给个目录值
		s.SetContext(r.Provider, r.Model, 1_000_000)
	}
}

// 新模型先进「未参与」：自动诊断不该碰它，得用户手动加入。
func TestNewModelsStartExcluded(t *testing.T) {
	s := tempStore(t)
	s.Ensure([]Ref{{Provider: "p", Model: "fresh"}})
	if e := one(t, s); e.State != Excluded {
		t.Fatalf("新模型应默认未参与，实际 %s", e.State)
	}
}

// 400 不进冷却的话，检测器每 5 秒重试同一个错，页面永远停在「诊断中」。
func TestUncountedFailureStillCools(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "wb", Model: "flash"})
	s.Failed("wb", "flash", "wb", Classify(400, nil, []byte(`{"message":"model [x] service info not found"}`)))
	e := one(t, s)
	if e.State != Cooling {
		t.Fatalf("4xx 之后应在冷却中，实际 %s", e.State)
	}
	if e.Failures != 0 {
		t.Fatalf("4xx 不该累计连续失败，实际 %d", e.Failures)
	}
}

func TestFailuresInARowDisable(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "p", Model: "m"})
	for i := 0; i < FailuresBeforeDisabled; i++ {
		s.Failed("p", "m", "p", Classify(502, nil, []byte("bad gateway")))
	}
	if e := one(t, s); e.State != Disabled {
		t.Fatalf("连续 %d 次失败应禁用，实际 %s", FailuresBeforeDisabled, e.State)
	}
}

func TestSuccessClearsFailuresAndCooling(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "p", Model: "m"})
	s.Failed("p", "m", "p", Classify(500, nil, []byte("boom")))
	s.Succeeded("p", "m", "p", 300, 42, 1500)
	e := one(t, s)
	if e.State != Candidate || e.Failures != 0 || e.TTFT != 300 || e.TPS != 42 {
		t.Fatalf("成功应恢复成候选中并写入指标，实际 %+v", e)
	}
}

// 并发满也是页面的一格：网关把「这个供应商的槽位用完了」写进 store，
// 页面的「并发满」分组靠它才有内容，槽位一空出来就回候选。
func TestBusyIsItsOwnGroup(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "p", Model: "m"})
	s.Succeeded("p", "m", "p", 300, 42, 1500)
	s.SetBusy("p", "m", true)
	if e := one(t, s); e.State != Busy {
		t.Fatalf("槽位用完应在并发满，实际 %s", e.State)
	}
	s.SetBusy("p", "m", false)
	if e := one(t, s); e.State != Candidate {
		t.Fatalf("槽位空出应回候选，实际 %s", e.State)
	}
}

func TestExcludedStaysOut(t *testing.T) {
	s := tempStore(t)
	s.Ensure([]Ref{{Provider: "p", Model: "m"}})
	s.SetExcluded("p", "m", true)
	if e := one(t, s); e.State != Excluded {
		t.Fatalf("移出后应是未参与，实际 %s", e.State)
	}
}

func TestFreeSortsBeforeCheaperTTFT(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "a", Model: "paid"}, Ref{Provider: "b", Model: "free", Free: true})
	s.Succeeded("a", "paid", "a", 120, 30, 900)
	s.Succeeded("b", "free", "b", 400, 10, 1200)
	got := s.Snapshot(time.Now())
	if got[0].Model != "free" {
		t.Fatalf("免费模型应排第一，实际 %s", got[0].Model)
	}
}

// 打分高的排前面：倍率 30 + 首字 25 + 速度 30 + 上下文 15 + 延迟 10。
// 0.08 倍率 + 快首字的模型，即使续期时间未知，也应压过 0.7 倍率的慢模型。
func TestScoreSortsCandidates(t *testing.T) {
	s := tempStore(t)
	s.Ensure([]Ref{
		{Provider: "a", Model: "cheap", Rate: 0.08, Context: 1_000_000},
		{Provider: "b", Model: "dear", Rate: 0.7, Context: 1_000_000},
	})
	join(s, Ref{Provider: "a", Model: "cheap"}, Ref{Provider: "b", Model: "dear"})
	s.Succeeded("a", "cheap", "a", 900, 55, 1200)
	s.Succeeded("b", "dear", "b", 3200, 12, 5000)
	got := s.Snapshot(time.Now())
	if got[0].Model != "cheap" {
		t.Fatalf("高分模型应排第一，实际 %s", got[0].Model)
	}
	if Score(got[0]) <= Score(got[1]) {
		t.Fatalf("排序与分数不一致：%d vs %d", Score(got[0]), Score(got[1]))
	}
}

// 速度测不出来（上游一次回完、不给分块）不算没测好：模型是通的，
// 停在诊断中会被诊断器每 5 秒重测一次（kiro/glm-5 卡住就是这个）。
func TestMissingSpeedStaysCandidate(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "a", Model: "nospeed"})
	s.Succeeded("a", "nospeed", "a", 800, 0, 1000)
	if e := one(t, s); e.State != Candidate {
		t.Fatalf("测出首字与延迟就该算可用，实际 %s", e.State)
	}
}

// 首字、延迟、上下文缺一项仍算没测好。
func TestMissingFirstTokenStaysDiagnosing(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "a", Model: "half"})
	s.Succeeded("a", "half", "a", 0, 50, 1000)
	if e := one(t, s); e.State != Diagnosing {
		t.Fatalf("没有首字应算未测好，实际 %s", e.State)
	}
}

// 多账号供应商：一个账号失败只让它自己歇着，模型本身仍可路由，网关会接着
// 试下一个账号；只有单账号（rest 就是供应商自己）才按整个模型冷却。
func TestOneAccountFailingLeavesTheModelRoutable(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "wb", Model: "flash"})
	now := time.Now()
	s.Failed("wb", "flash", "wb@first", Classify(502, nil, []byte("bad gateway")))
	if e := one(t, s); e.State == Cooling {
		t.Fatalf("多账号时一个账号失败不该让整个模型进冷却，实际 %+v", e)
	}
	if !s.AccountCooling("wb", "flash", "wb@first", now) {
		t.Fatal("失败的那个账号应在自己的冷却里")
	}
	if s.AccountCooling("wb", "flash", "wb@second", now) {
		t.Fatal("没失败的账号不该被连坐")
	}
	// 另一个账号成功：连续失败清零，模型仍是候选中
	s.Succeeded("wb", "flash", "wb", 300, 42, 1500)
	if e := one(t, s); e.State != Candidate || e.Failures != 0 {
		t.Fatalf("另一个账号成功后应回候选中，实际 %+v", e)
	}
}

// 全部账号都在冷却才算模型整体冷却：只判「Accounts 里有条目」会让挂着一
// 个失败账号的可答模型一直显示冷却，而只判 RestUntil 又让多账号模型永远
// 停在「候选」——页面上的失败像没发生过（候选 56、冷却 0）。
func TestEveryAccountCoolingCoolsTheModel(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "trae", Model: "qwen"})
	s.Succeeded("trae", "qwen", "", 500, 50, 1200)
	s.FailedOf("trae", "qwen", "trae@a", 2, Failure{Kind: KindRateLimited, Reason: "429"})
	if e := one(t, s); e.State != Candidate {
		t.Fatalf("两个账号只歇了一个，模型仍能答，实际 %s", e.State)
	}
	now := time.Now()
	s.FailedOf("trae", "qwen", "trae@b", 2, Failure{Kind: KindRateLimited, Reason: "429"})
	e := one(t, s)
	if e.State != Cooling {
		t.Fatalf("两个账号都在冷却，模型应进冷却组，实际 %s", e.State)
	}
	if !e.CoolingUntil.After(now) {
		t.Fatalf("冷却中的模型要给出恢复时间，实际 %v", e.CoolingUntil)
	}
	if s.ExcludedOrCooling("trae", "qwen", now) != true {
		t.Fatal("全账号冷却的模型，网关也该跳过它")
	}
	// 冷却过期后回候选
	s.Update("trae", "qwen", func(e *Entry) {
		for k := range e.Accounts {
			e.Accounts[k] = now.Add(-time.Second)
		}
	})
	if e := one(t, s); e.State != Candidate {
		t.Fatalf("账号冷却过期后应回候选，实际 %s", e.State)
	}
}

func TestRecentAverageDropsTheOutliers(t *testing.T) {
	// 十条：最大 6000 与最小 100 各一条被丢掉
	e := &Entry{Recent: []Sample{
		{TTFT: 900}, {TTFT: 6000}, {TTFT: 100}, {TTFT: 1000}, {TTFT: 1100},
		{TTFT: 1200}, {TTFT: 1300}, {TTFT: 1400}, {TTFT: 1500}, {TTFT: 1600},
	}}
	got := e.average(func(s Sample) float64 { return s.TTFT })
	want := (900.0 + 1000 + 1100 + 1200 + 1300 + 1400 + 1500 + 1600) / 8
	if got != want {
		t.Fatalf("ttft average = %v, want %v", got, want)
	}
	// 不足十条直接平均
	short := &Entry{Recent: []Sample{{TTFT: 1000}, {TTFT: 3000}}}
	if got, want := short.average(func(s Sample) float64 { return s.TTFT }), 2000.0; got != want {
		t.Fatalf("short average = %v, want %v", got, want)
	}
}

// 检测成功后窗口太小的模型自己退到「未参与」，不必手动一个个关。
func TestSmallContextLeavesOnItsOwn(t *testing.T) {
	s := tempStore(t)
	join(s, Ref{Provider: "p", Model: "small"}, Ref{Provider: "p", Model: "big"})
	s.SetContext("p", "small", 200_000)
	s.SetContext("p", "big", 1_000_000)
	for _, m := range []string{"small", "big"} {
		s.Succeeded("p", m, "p", 900, 50, 1200)
	}
	states := map[string]State{}
	for _, e := range s.Snapshot(time.Now()) {
		states[e.Model] = e.State
	}
	if states["small"] != Excluded {
		t.Fatalf("200K 窗口应自动未参与，实际 %s", states["small"])
	}
	if states["big"] != Candidate {
		t.Fatalf("1M 窗口应留在候选，实际 %s", states["big"])
	}
}

// 一个模型同时只跑一个探针：45 秒的诊断期内，每 5 秒一轮的批次不该再戳它。
func TestOneProbeAtATime(t *testing.T) {
	s := tempStore(t)
	if !s.MarkTrying("p", "m") {
		t.Fatal("第一次应拿到槽位")
	}
	if s.MarkTrying("p", "m") {
		t.Fatal("探针未结束时不该再拿到")
	}
	s.DoneTrying("p", "m")
	if !s.MarkTrying("p", "m") {
		t.Fatal("释放后应能再拿")
	}
}

// 额度快重置的模型要排在同样指标但没有窗口的模型前面：不用掉就作废。
func TestSoonScoresHigher(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	mk := func(soon int64) SnapshotEntry {
		return SnapshotEntry{Entry: &Entry{
			Provider: "p", Model: "m", Rate: 0.5, Context: 1_000_000,
			TTFT: 800, TPS: 60, Latency: 900, Soon: soon,
		}}
	}
	base := ScoreAt(mk(0), now)
	far := ScoreAt(mk(now.Add(60*24*time.Hour).Unix()), now)
	near := ScoreAt(mk(now.Add(2*24*time.Hour).Unix()), now)
	if far != base {
		t.Fatalf("60 days out should not add points: %d vs %d", far, base)
	}
	if near != base+15 {
		t.Fatalf("2 days out should add 15: got %d, want %d", near, base+15)
	}
}
