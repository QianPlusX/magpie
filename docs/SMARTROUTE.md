# 智能路由（smartroute）逻辑说明

面向 magpie 的增强：在**不替换**原版路由页的前提下，多给一个「智能路由」视图，
按实测数据给模型排序、把坏掉的模型自动剔除、并把结果做成一个可被外部应用调用的路由组。

当前随 App 发布的版本：**0.1.44**。

---

## 1. 它由四块拼成

| 组件 | 位置 | 职责 |
|---|---|---|
| 状态存储 | `internal/smartroute/smartroute.go` | 每个模型一条记录 + 状态机 + 持久化 |
| 诊断器 | `internal/gui/smartroute_diagnose.go` | 后台批量试模型，写成功/失败 |
| 页面与接口 | `internal/gui/smartroute.go` + `assets/plugins/routing/client.js` | `/api/smartroute`、`/api/smartroute/toggle`、`/api/smartroute/reset`，以及 UI |
| 路由层接线 | `internal/gateway/{gateway,routing,fallback}.go` | 真实调用写结果、候选池里剔除坏模型、白名单拦截 |

数据来源只有一个文件：

```
~/.config/magpie/smartroute.json
```

每条记录以 `"<provider>/<model>"` 为键，只改内存的字段（`Soon`、`Busy`、`Answering`、`Context`）不落盘。

---

## 2. 状态机

`Entry.StateNow(now)` 按下面顺序判定，**先命中先返回**：

| 序 | 条件 | 状态 | 页面文案 |
|---|---|---|---|
| 1 | `Excluded == true` | excluded | 未参与（灰） |
| 2 | `now < DisabledUntil` | disabled | 禁用中（红，显示 N 小时后恢复） |
| 3 | `now < RestUntil` | cooling | 冷却中（红，显示 Ns 后重试） |
| 4 | `Busy == true` | busy | 并发满（红） |
| 5 | `Failures > 0` 或 `LastOK` 为空 | diagnosing | 未检测 / 检测中（灰 / 蓝） |
| 6 | 其余 | candidate | 等待请求中 / 回答中（绿） |

关键点：

- **candidate 的充要条件是「成功过一次且当下没有失败计数」**。`Succeeded()` 会清零
  `Failures`、`RestUntil`、`DisabledUntil`，所以一次成功就立刻从冷却/禁用里出来。
- **diagnosing 分两种**：`LastTry`、`LastOK`、`Failures` 全为空 = 从没测过 → 「未检测」（灰球，静止）；
  其余 = 测过但还没结论 → 「检测中」（蓝球，呼吸动画）。
- 冷却与禁用都是**时间到自动恢复**，不需要人点。页面上的「结束冷却」是提前放行。

### 三个常量

```go
FailuresBeforeDisabled = 10              // 连续失败 10 次
DisabledFor            = 24 * time.Hour  // 禁用 24 小时
CoolFor                = 30 * time.Second// 单次失败冷却 30 秒
```

### 写入规则

```go
// 成功（诊断或真实调用）
Failures = 0; LastError = ""; DisabledUntil = zero; RestUntil = zero
LastOK = now; LastTry = now; TTFT/TPS/Latency = 实测

// 失败，counted = true（现在所有路径都传 true）
Failures++
if Failures >= 10 → DisabledUntil = now + 24h
else              → RestUntil    = now + 30s

// 失败，counted = false（保留给「配置性错误」的逃生门）
只写 LastError / LastTry，仍给 30s 冷却
```

> 历史坑：`counted=false` 早期写成「直接 return、不设 RestUntil」，导致 4xx 每 5 秒重试一次、
> 永远停在「连续失败 1」。现已修：即便不计数，也必须进冷却。

---

## 3. 诊断器（是谁在自动发请求）

```go
func diagnoseBatch() {
    entries := store.Snapshot(now)
    for _, e := range entries {
        if e.State != Diagnosing { continue }        // 只碰「诊断中」
        if !provider.Checked(e.Provider+"/"+e.Model) { continue } // 只碰勾选的
        if !e.LastTry.IsZero() && now.Sub(e.LastTry) < 2*DiagnoseGap { continue }
        batch = append(batch, e)                     // 每批最多 5 个
    }
    // 并发跑，每 5 秒一轮
}
```

- **只有「诊断中」的模型会被测**。冷却中、禁用中、并发满、未参与、候选中——一律不碰。
- **新加入目录的模型默认是「未参与」**：自动诊断不会碰它，也就不会有任何请求发出去。
  要它参与，得在页面上手动点「加入」。供应商页的「测试模型」按钮是另一条路：
  它只测一次、只回填指标，**不会**把模型从「未参与」改成参与。
- **只有供应商页勾选的模型会被测**。没勾的既不显示也不检测，勾回来才进队列。
- 每轮最多 5 个，间隔 `DiagnoseGap = 5s`，同一模型两轮内不重复测。

### 测试请求怎么发

诊断走 magpie 自己的入口，模拟一次 Agent 调用，而不是直连上游：

```
POST http://127.0.0.1:3425/v1/chat/completions
model: <provider>/<model>        # 与 Agent 请求时完全一致的写法
stream: true, max_tokens: 1
X-Magpie-Diagnose: 1
```

读到第一条 SSE `data:` 就算出首字时间，收流算速度。

> 历史坑：早期用 `provider.Find(pid).TestModels(OwnModelID(model))`，社区插件供应商剥不干净前缀，
> 上游报 `service info not found`。改成走网关入口后这个类错误消失。

### 诊断只拿得到延迟

`max_tokens=1` 的探测只能得到「首字」与「延迟」，**速度（tok/s）无法从探测里得到**，
所以速度栏显示 `—`，直到该模型被真实调用过一次。这是刻意的：为了测速度去生成更多 token 是浪费额度。

---

## 4. 真实调用的写入点

| 事件 | 位置 | 写入 |
|---|---|---|
| 请求成功（`status < 400`） | `gateway.go` 的 `appendUsage` 之后 | `Succeeded(provider, model, TTFT, tps, latency)`，`tps = 输出token / (Millis - TTFT)` |
| 上游返回 ≥400 | `routing.go` 的 `restAfter` | `Failed(provider, model, "HTTP 400: ...", true)` |
| 网络错误 | 已被 `gateway.go` 映射成 502 | 同样走 `restAfter` |

也就是说：**用着用着，列表会自己变准**。真实调用的首字/速度/延迟覆盖诊断估出来的值。

---

## 5. 路由层剔除

`fallback.go` 的 `restLast` 开头：

```go
out, pl = dropSmartExcluded(out, pl)
```

把 `excluded / disabled / cooling` 的候选从池子里拿掉，再走原版的重试链。

**fail open**：如果剔完一个都不剩，按原样试——宁可试一次，也不让请求无路可走。

这一步对**所有**调用生效，包括外部应用直接点名某个模型；所以「不参与」不只是页面效果。

---

## 6. 排序

`sortEntries` 先按状态分组：

```
候选(0) → 诊断中(1) → 冷却中(2) → 并发满(3) → 已禁用(4) → 未参与(5)
```

候选中再按这个优先级排（全部是「好」在前，未知在后）：

1. **免费**优先
2. **额度快重置**优先（`Soon` 小的在前，没数据排最后）
3. **首字低**（`TTFT` 升序，0 视为未知排最后）
4. **速度快**（`TPS` 降序）
5. **上下文大**（`Context` 降序）
6. **延迟低**（`Latency` 升序）

其余状态按加入时间（`Joined`）排。

`Soon`（额度到期）不是自己探测出来的，而是从路由层顺手抄的：`weighRouted` 里
算每个模型的额度窗口时，把最早的非零 `Renewal()` 时间写进 store（`SetSoon`，只改内存）。

---

## 7. 与 magpie 配置的耦合：勾选白名单

「供应商页勾选的模型」是三件事的唯一真源：

```go
func checkedModels() map[string]bool   // providersState().Providers[].Models[].On
```

| 用途 | 行为 |
|---|---|
| 页面列表 | 只显示勾选的 |
| 诊断器 | 只测勾选的 |
| 路由组 `group/smart` | 成员只有勾选的、且未标记「不参与」的 |
| 网关入口 | 没勾的模型直接 404 `model not selected`，不发上游 |

缓存 10 秒；供应商状态还没就绪（拿到空集合）时**保留上一次结果**，避免启动瞬间清空列表。

网关拦截在 `provider.Resolve(asked)` 之前：

```go
if !provider.IsGroupRef(asked) && !provider.Checked(asked) {
    → 404 "model not selected"
}
```

这条是为了解决「Agent 点名一个供应商没勾/供应商不提供的模型，请求一路发到上游换回 400」——
现在在本地就挡掉，不出网、不浪费一次往返。

---

## 8. 路由组 `group/smart`

`ensureSmartGroup` 把智能路由做成一个 magpie 原生路由组：

```go
provider.SaveGroup(provider.Group{
    ID:      "smart",              // 组 ID 只能小写字母数字点横线
    Name:    "智能路由",            // 中文只放这里
    Members: ["workbuddy-ai/glm-5.3-flash", ...], // 按页面排序，跳过「不参与」
})
```

- 外部应用 / DSH 直接把模型写成 `group/smart`，就能用上这套排序与剔除。
- 组只认 `group/smart` 这个 catalog id；`provider.FindGroup("smart")` 是找不到的（曾踩过：
  每 30 秒把组重建一遍）。
- 启动时建一次，之后每 30 秒核对；**成员集合没变就不写文件**，避免延迟漂移导致频繁落盘。

---

## 9. 页面

两个一级视图（保留原版路由）：

```
原版路由 | 智能路由
```

- 切换记忆在 `localStorage["mgsr.show"]`，切页面再回来保持。
- 智能路由视图**不隐藏实时请求舞台**，只隐藏「路由组 / 新建组」那一块
  （`#rtMore.smart-on #rtGroups{display:none}`）。
- 六个状态胶囊平铺、可换行：候选中 / 诊断中 / 冷却中 / 并发满 / 已禁用 / 未参与。

卡片结构（用户定稿，别合并回一行）：

```
● 模型名（可换行，占满剩余宽度）          状态（右对齐）
首字 1298ms
速度 —
延迟 1401ms
类型：文本
上下文：1M
倍率 0.08×
27 天后重置
连续失败 1
[复制错误]                        [结束冷却] [不参与]
```

---

## 10. 互动接口

| 方法 | 路径 | 作用 |
|---|---|---|
| GET | `/api/smartroute` | `{now, entries:[...]}`，已按勾选过滤 |
| POST | `/api/smartroute/toggle` | `{provider, model, excluded}`，加入/移出智能路由 |
| POST | `/api/smartroute/reset` | 清冷却/禁用/失败计数（页面的「结束冷却」） |

页面每 3 秒拉一次列表，另有 1 秒一次的本地重绘让倒计时每秒走。

---

## 11. 已知限制

1. **速度（tok/s）只有真实调用后才有**；诊断不做内容生成。
2. **额度到期时间**需要该模型被路由层算过一次额度（即真实请求碰过它），否则没有。
3. **冷却/禁用的节奏是全局常量**，没有做成设置项。
4. **跨供应商同名模型**按 `provider/model` 分开记，不会互相污染。
5. 诊断器只在 magpie 进程内跑；App 被杀就停，重启后按 store 里的状态继续。

---

## 12. 版本演进（给后来者看为什么长这样）

| 版本 | 那次解决了什么 |
|---|---|
| 0.1.6–0.1.9 | 骨架：store、接口、页面、真实调用写入 |
| 0.1.10–0.1.11 | 类型/免费/并发满 |
| 0.1.12–0.1.15 | 升级不换二进制的坑、tabs 位置、模型来源换成 Catalog |
| 0.1.22–0.1.24 | 4xx 也在冷却、上下文单位、版本戳、快到期优先 |
| 0.1.27–0.1.30 | 状态球动画、诊断改走网关入口、4xx 计入失败 |
| 0.1.38–0.1.39 | 视图记忆、倍率、组查找修正、成员双前缀 |
| 0.1.40 | 列表按供应商勾选过滤 |
| 0.1.42–0.1.44 | 网关白名单、诊断只测勾选、未检测文案 |
