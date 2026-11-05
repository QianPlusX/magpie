# 智能路由：错误分类与差异化处理方案

现状：所有失败一视同仁——冷却 30 秒，连续 10 次禁用 24 小时。
后果是三类错误被误伤：

- 模型根本不存在（`400 model not found`）：被当成「坏了」，试满 10 次白烧额度，
  每 30 秒还去撞一次墙。
- 账号问题（401 余额/权限）：记在**模型**头上，同账号下其它模型照样被继续派发。
- 429 限流：本来十几秒就恢复，却累计进「连续失败」，极端情况被禁 24 小时。

目标：**错误分类型，处理方式和计时各按类型走。**

---

## 一、分类表

| # | 类别 | 判定 | 计数 | 冷却 | 解除方式 | 页面文案 |
|---|---|---|---|---|---|---|
| 1 | `rate_limited` | 429，或 503 带 `Retry-After` | 否 | `Retry-After` 或 30s | 到时自动 | 限流中（Ns 后重试） |
| 2 | `auth` | 401 / 402 / 403 | 否 | 10 分钟 | 到时自动 | 账号异常（Nm 后重试） |
| 3 | `no_model` | 400/404 且 body 匹配 `service info not found` / `model .* not found` / `invalid model` | 否 | 24 小时 | 手动「结束冷却」，或供应商配置变化 | 模型未提供 |
| 4 | `bad_request` | 400 其他（参数、格式、上下文超长） | 否 | 5 分钟 | 到时自动 | 请求被拒 |
| 5 | `content` | 400 且 body 含 `content_filter` / `safety` / `moderation` | 否 | 1 分钟 | 到时自动 | 内容被拦 |
| 6 | `upstream` | 5xx（500/502/503/504） | 是 | 30s | 到时自动 | 上游故障（Ns 后重试） |
| 7 | `timeout` | 上下文超时 / 连接失败 / DNS | 是 | 30s | 到时自动 | 超时（Ns 后重试） |
| 8 | `unknown` | 其余（4xx 未知、协议异常） | 是 | 30s | 到时自动 | 异常 |

只有 6/7/8 走原来的「连续 10 次 → 禁用 24 小时」；1–5 不累计，只各自计时。

### 为什么是这些时长

| 类别 | 时长的依据 |
|---|---|
| 429 | 上游最清楚什么时候能再来，`Retry-After` 优先；没有就 30s（原值） |
| 401/402/403 | 人要去改配置（充值、换 key、开权限），10 分钟给一个反馈周期，不骚扰 |
| no_model | 模型不在这个供应商的列表里，重试一次都不会变——24 小时等用户自己改 |
| bad_request | 多半是这次请求的锅（上下文超长、参数不对），5 分钟足够跨过一批坏请求 |
| content | 同样是一次性的，1 分钟 |
| upstream / timeout | 真·抖动，30 秒重试是合理的 |

---

## 二、账号级错误要落到「账号」

401/402/403 不是某个模型坏了。它应该：

1. 仍把该模型标成 `cooling`（不让它继续被选）；
2. 同时给**同一供应商下的所有模型**打上同样的冷却（同一个 key 一起坏）；
3. 页面上标成「账号异常」，而不是「模型异常」。

实现时在 `Failed` 里识别类别后，遍历同 provider 的条目一起设置 `RestUntil`。
**ponytail 边界**：不做按账号（多 key）细分，一个 provider 一条 key 就先按 provider 处理；
真有多 key 再按 `accountID` 分流。

---

## 三、数据结构改动

```go
// 新：失败的类别
type Kind int
const (
    KindUnknown Kind = iota
    KindRateLimited
    KindAuth
    KindNoModel
    KindBadRequest
    KindContent
    KindUpstream
    KindTimeout
)

type Failure struct {
    Kind       Kind
    Reason     string
    RetryAfter time.Duration // 0 = 用该类别的默认值
}

func (s *Store) Failed(provider, model string, f Failure)   // 取代 counted bool
```

`Entry` 增加：

```go
Kind  Kind   `json:"kind,omitempty"`   // 最后一次失败的类别
```

`StateNow` 不变（仍是 cooling/disabled），页面按 `Kind` 决定文案与颜色。

---

## 四、分类函数

```go
// internal/smartroute/classify.go
func Classify(status int, header http.Header, body []byte) Failure
```

判定顺序：先看 status，再看 body 里的关键字（大小写不敏感，只看前 500 字节）：

```go
switch {
case status == 429:
    return Failure{Kind: KindRateLimited, RetryAfter: retryAfter(header)}
case status == 401 || status == 402 || status == 403:
    return Failure{Kind: KindAuth}
case status == 400 || status == 404:
    if hasAny(body, "service info not found", "model not found", "invalid model", "does not exist") {
        return Failure{Kind: KindNoModel}
    }
    if hasAny(body, "content_filter", "safety", "moderation") {
        return Failure{Kind: KindContent}
    }
    return Failure{Kind: KindBadRequest}
case status >= 500:
    return Failure{Kind: KindUpstream}
}
return Failure{Kind: KindUnknown}
```

超时/网络在调用点（`diagnoseOne`、gateway 的网络错误分支）直接构造 `Failure{Kind: KindTimeout}`。

---

## 五、接入点

| 位置 | 现在 | 改成 |
|---|---|---|
| `internal/gateway/routing.go:restAfter` | `Failed(p, m, reasonOf(status, body), status >= 500)` | `Failed(p, m, classify(status, header, body))` |
| `internal/gui/smartroute_diagnose.go:diagnoseOne` | 自己拼 `"HTTP %d: %s"` | 同上，用 `classify` |
| `internal/gateway/gateway.go` 网络错误 → 502 的映射 | 走 restAfter | 保留，但构造时带 `KindTimeout` |

`reasonOf` 降级为「生成给人看的字符串」的辅助，由 `Failure.Reason` 承载。

---

## 六、冷却时长表（一处常量，便于以后调）

```go
var restFor = map[Kind]time.Duration{
    KindRateLimited: 30 * time.Second,
    KindAuth:        10 * time.Minute,
    KindNoModel:     24 * time.Hour,
    KindBadRequest:  5 * time.Minute,
    KindContent:     time.Minute,
    KindUpstream:    30 * time.Second,
    KindTimeout:     30 * time.Second,
    KindUnknown:     30 * time.Second,
}
var counted = map[Kind]bool{   // 只有这些累计进「连续 10 次」
    KindUpstream: true, KindTimeout: true, KindUnknown: true,
}
```

---

## 七、页面表现

| 类别 | 球 | 文案 | 告警条 |
|---|---|---|---|
| 限流中 | 红 | 限流中 | `⚠ 429 限流 · 12s 后重试` |
| 账号异常 | 红 | 账号异常 | `⚠ 401 · 账号 key 或额度有问题 · 10m 后重试` |
| 模型未提供 | 红 | 模型未提供 | `⚠ 该供应商列表里没有这个模型 · 24h 后重试` |
| 请求被拒 | 红 | 请求被拒 | `⚠ 400 · 上下文或参数不对 · 5m 后重试` |
| 内容被拦 | 红 | 内容被拦 | `⚠ 内容过滤 · 1m 后重试` |
| 上游故障 | 红 | 上游故障 | `⚠ 500 · Ns 后重试` |
| 超时 | 红 | 超时 | `⚠ 请求超时 · Ns 后重试` |

「模型未提供」和「账号异常」额外给一个 `重新检测` 按钮：改完配置立刻验一次，不必等计时。

---

## 八、验证清单

单测（`internal/smartroute/classify_test.go`，表驱动，不需要网络）：

1. `429 + Retry-After: 7` → `KindRateLimited`，`RetryAfter=7s`
2. `401` / `402` / `403` → `KindAuth`
3. `400 {"message":"model [x] service info not found"}` → `KindNoModel`
4. `404 model not found` → `KindNoModel`
5. `400 context length exceeded` → `KindBadRequest`
6. `400 content_filter` → `KindContent`
7. `500` / `502` → `KindUpstream`
8. `Failed(KindNoModel)` 后状态是 cooling 且 `Failures` 不变
9. `Failed(KindUpstream)` 十次后状态是 disabled

真机（各造一条）：

- 关网 → 看「超时」
- 把 key 改错 → 看「账号异常」，同供应商另一个模型也一起变红
- 请求一个供应商没有的模型 → 看「模型未提供」，且 24 小时内不再发请求
- 正常聊天触发 429（连续猛发）→ 看「限流中」且倒计时跟着 `Retry-After`

---

## 九、落地顺序

1. `classify.go` + `classify_test.go`（纯函数，先测通）
2. `Store.Failed` 换签名，两家调用点跟着改
3. 页面按 `Kind` 出文案与按钮
4. 账号级联动（同 provider 一起冷却）
5. 真机验证

第一步与第二步可以一起提交；第三步可以晚一版。

---

## 十、不做的部分

- 不做按 API key 的精细账号路由（一个供应商多条 key 的场景还没出现）。
- 不做自动改配置（充值、换 key 得人来）。
- 不把错误类别写进 `usage.jsonl`（现有的用量日志已经够复盘，加字段要动格式）。
