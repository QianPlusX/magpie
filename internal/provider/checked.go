package provider

import "strings"

// checkedModels is what the Providers page has ticked, as "provider/model",
// handed in by the gui. The gateway refuses a model it doesn't show, so a
// request cannot reach a vendor the user turned off there — the same set
// the agent's model list is built from.
var checkedModels func() (map[string]bool, bool)

// SetCheckedModels installs that set. Without it every model is served, as
// a build without the page has always done.
func SetCheckedModels(f func() (map[string]bool, bool)) { checkedModels = f }

// Checked says whether ref is one the Providers page shows now.
//
// The second return of the setter says whether that reading is trustworthy.
// 没读到（页面还没起来）就放行，免得一次读失败把所有模型停掉；读到了但集合为空
// 是用户真的一个都没勾，那就一个都不放行 —— 这两种以前被同一个 len==0 兜底吃掉，
// 于是「全不选」的时候网关和诊断器照旧把模型当可用。
func Checked(ref string) bool {
	if checkedModels == nil {
		return true
	}
	set, ok := checkedModels()
	if !ok {
		return true
	}
	return set[strings.TrimSpace(strings.TrimSuffix(ref, "[1m]"))]
}

// PickedByUser are the "provider/model" the user added on the Providers
// page: a provider's entry there keeps the model ids they ticked.
//
// 这一份才是「用户勾了什么」。provider.Catalog() 不是：一个从没添加过的
// 插件供应商（providers.json 里没有它的条目）会被当成「未挑选过，全开」，
// 于是它的模型全被列出来 —— 智能路由照 Catalog 列的时候就把这些谁也没选
// 的模型摆了出来，还顺手写进了 group/smart 的成员。
func PickedByUser() map[string]bool {
	out := map[string]bool{}
	// 只认 providers.json 里真的有条目的那些：All() 还会带上插件注册的
	// 供应商，它们的 Models 是插件自带的全量清单（用户没添加过也一样），
	// 拿它当勾选就会把另一个供应商的同名模型也算进来。
	for _, p := range load().Providers {
		// 供应商关掉了（Providers 页那个开关）就不算勾选：它的模型在网关里
		// 也解析不到（providerEntries 按 Off 过滤过），留在智能路由里只是幽灵
		if p.Off {
			continue
		}
		for _, id := range p.Models {
			out[p.ID+"/"+id] = true
		}
	}
	return out
}
