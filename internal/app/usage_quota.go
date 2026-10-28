package app

// /admin/api/usage-quota —— 按账号读 Gemini 的 compute-based 用量（当前 5 小时窗口 + 每周）。
//
// 数据源是网页 /usage 页面自己发的那个 batchexecute RPC（rpcid jSf9Qc，参数空数组），
// 不是本地 requests 表。原因有三：
//  1. 本地表只看得见经过本代理的流量，网页/手机/Kelivo 直连的消耗全都看不见；
//  2. 图片请求的 prompt_tokens 记 0（vision 走独立上传通道），按 token 算必然低估；
//  3. Google 从 2026-05-20 起改成 compute-based，同一账号 111 万 token 没触顶、
//     31 万就满了，token 数跟额度不是线性关系。
//
// 跟 video-quota 的分工：视频没有 API，只能翻会话历史数次数；文本额度有 RPC 直出百分比，
// 一次请求就够，不用遍历会话。
//
// 返回桶的形状（实测 + Voyager 扩展的结构化解析约定）：
//
//	[limit, fractionUsed, periodEnum, [[resetEpochSec, nanos]]]
//
// periodEnum 的含义 Google 没公开，所以这里不硬编码枚举值，改用「重置时间距今多远」
// 反推是哪个桶——重置在 6 小时内必然是 5 小时窗口，1~8 天内是每周。
// 等 Phase 0 用真实响应校准后，再把确认的枚举填进 usagePeriodNames。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

const usageRPCID = "jSf9Qc"

// 用量变化以分钟计，90 秒够新鲜；再短就是白吃单 IP 预算（每号每次 1 个上游请求）。
const usageCacheTTL = 90 * time.Second

// periodEnum → 名字。2026-10-28 用真响应校准过：
//
//	period=1 → 滚动 5 小时窗口（reset 在 5 小时内）
//	period=2 → 每周限额
//
// 校准证据：period=2 的 reset 是 1793293628 = 2026-10-30 01:07:08 (+08)，
// 与网页 /usage 上「每周限额…重置 10 月 30 日 1:07」完全一致。
// 兜底逻辑（按 reset 距今天数反推）保留，防 Google 换枚举。
var usagePeriodNames = map[int]string{
	1: "rolling_5h",
	2: "weekly",
}

// usageBucket 是一个额度桶。Percent 是 Google 自己算好的，别二次加工。
//
// 实测（2026-10-28）：5 小时桶的 limit 恒为 0——Google 只给比例，不给绝对量；
// 每周桶才带 limit（当次实测 24067，量纲是 compute 点数，不是请求次数）。
// 所以 Used 只在 LimitKnown 为 true 时才有意义，别拿 limit=0 去做除法。
type usageBucket struct {
	Period     string  `json:"period"`      // rolling_5h | weekly | period_<code>
	PeriodCode int     `json:"period_code"` // RPC 原始枚举，保留给校准用
	Limit      float64 `json:"limit"`       // 总配额；5h 桶恒为 0
	LimitKnown bool    `json:"limit_known"` // limit 是否可信
	Used       float64 `json:"used"`        // fraction * limit，仅 limit_known 时有意义
	Fraction   float64 `json:"fraction"`    // 0..1
	Percent    float64 `json:"percent"`     // 0..100，保留一位小数
	Remaining  float64 `json:"remaining"`   // 100 - percent，前端直接用
	ResetAt    int64   `json:"reset_at"`    // epoch 秒
	ResetInSec int64   `json:"reset_in_sec"`
}

type accountUsage struct {
	ID        int64         `json:"id"`
	Label     string        `json:"label"`
	Status    string        `json:"status"` // ok | error
	Buckets   []usageBucket `json:"buckets"`
	FetchedAt int64         `json:"fetched_at"`
	Cached    bool          `json:"cached"`
	Error     string        `json:"error,omitempty"`
	RawHead   string        `json:"raw_head,omitempty"` // 只在解析失败时给，方便排查上游改协议
}

func handleAdminUsageQuota(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()
	fresh := q.Get("fresh") == "1"
	wantID := int64(atoiDefault(q.Get("account_id"), 0))
	now := time.Now()

	var out []accountUsage
	for _, a := range accountList() {
		// 点名要某个号时，停用的号也照查——调用方明确要看它。
		if wantID != 0 && a.ID != wantID {
			continue
		}
		if wantID == 0 && a.Status != "enabled" {
			continue
		}
		out = append(out, usageQuotaFor(a, now, fresh))
	}
	if wantID != 0 && len(out) == 0 {
		writeJSON(w, 404, map[string]any{"error": "account not found", "account_id": wantID})
		return
	}

	writeJSON(w, 200, map[string]any{
		"now":      now.Unix(),
		"accounts": out,
		"note": "百分比来自 Google 的用量 RPC（与网页 /usage 同源），不是本地估算。" +
			"limit 的量纲未确认，可能是 compute 点数而非请求次数；额度是账号级统一池子，" +
			"不按模型分账。数据缓存 90 秒，?fresh=1 强制刷新。",
	})
}

func usageQuotaFor(a CookieAccount, now time.Time, fresh bool) accountUsage {
	u := accountUsage{ID: a.ID, Label: accountDisplayName(&a), FetchedAt: now.Unix()}

	// 缓存键就是账号 id，cookie 摘要存在 entry 里：换了 cookie 视为失效。
	// 不把 hash 拼进 key 是为了让「按账号取水位」能 O(1) 查到——额度闸和心跳都按 id 找。
	if !fresh {
		usageCacheMu.Lock()
		if e, ok := usageCache[a.ID]; ok &&
			e.cookieHash == cookieKey(a.Cookie) &&
			now.Sub(e.fetchedAt) < usageCacheTTL {
			hit := e.usage
			hit.Cached = true
			usageCacheMu.Unlock()
			return hit
		}
		usageCacheMu.Unlock()
	}

	sapisid := extractSAPISID(a.Cookie)
	var xsrf string
	err := viaSlot(a, func(proxyURL string) (err error) {
		xsrf, err = getXSRF(a.Cookie, proxyURL)
		return err
	})
	var rl *RateLimitError
	if errors.As(err, &rl) {
		u.Status, u.Error = "error", err.Error()
		return u
	}
	if err != nil {
		u.Status, u.Error = "error", "not signed in: "+err.Error()
		return u
	}

	var body string
	err = viaSlot(a, func(proxyURL string) (err error) {
		body, err = fetchUsageRaw(a.Cookie, sapisid, xsrf, proxyURL)
		return err
	})
	if err != nil {
		u.Status, u.Error = "error", err.Error()
		return u
	}

	u.Buckets = parseUsageBuckets(body, now)
	if len(u.Buckets) == 0 {
		// 上游改了协议 / RPC 被拒。不缓存，下次照查；把响应头 300 字带出来给人看。
		u.Status, u.Error = "error", "no usage bucket in response (rpc shape changed?)"
		u.RawHead = headOf(body, 300)
		return u
	}
	u.Status = "ok"

	usageCacheMu.Lock()
	if len(usageCache) > 256 {
		usageCache = map[int64]usageCacheEntry{}
	}
	usageCache[a.ID] = usageCacheEntry{usage: u, cookieHash: cookieKey(a.Cookie), fetchedAt: now}
	usageCacheMu.Unlock()
	return u
}

// fetchUsageRaw 打一次 jSf9Qc。三处易错点，改之前先看：
//   - at（XSRF）必须带，不带直接 400，响应体里带 "xsrf" 字样；
//   - source-path 是账号作用域标记，缺了可能拿到空数据；
//   - 不带模型 header（x-goog-ext-525001261-jspb），这个 RPC 跟模型无关。
func fetchUsageRaw(cookie, sapisid, xsrf, proxyURL string) (string, error) {
	freq, _ := json.Marshal([]any{[]any{[]any{usageRPCID, "[]", nil, "generic"}}})
	form := url.Values{"f.req": {string(freq)}, "at": {xsrf}}
	ep := fmt.Sprintf(
		"https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=%s&bl=%s&hl=en&_reqid=%d&rt=c&source-path=%%2Fu%%2F0%%2Fusage",
		usageRPCID, currentBL(proxyURL), time.Now().UnixNano()%1000000)
	h := buildGeminiHeaders(cookie, sapisid, "")
	delete(h, "x-goog-ext-525001261-jspb")
	st, _, body, err := uploadPost(ep, h, []byte(form.Encode()), proxyURL)
	if err != nil {
		return "", err
	}
	if st != 200 {
		return "", fmt.Errorf("%s HTTP %d", usageRPCID, st)
	}
	return string(body), nil
}

// parseUsageBuckets 按形状认桶，不按下标取：Google 会轮换 rpcid，也可能加新桶，
// 认不出的节点跳过，别整体拒收。
func parseUsageBuckets(body string, now time.Time) []usageBucket {
	var out []usageBucket
	for _, p := range wrbPayloads(body) {
		walkUsageBuckets(p, now, &out)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResetAt < out[j].ResetAt })
	return out
}

func walkUsageBuckets(node any, now time.Time, out *[]usageBucket) {
	arr, ok := node.([]any)
	if !ok {
		return
	}
	if b, ok := asUsageBucket(arr, now); ok {
		*out = append(*out, b)
		return
	}
	for _, v := range arr {
		walkUsageBuckets(v, now, out)
	}
}

func asUsageBucket(a []any, now time.Time) (usageBucket, bool) {
	if len(a) < 4 {
		return usageBucket{}, false
	}
	limit, ok1 := usageNum(a[0])
	frac, ok2 := usageNum(a[1])
	code, ok3 := usageNum(a[2])
	if !ok1 || !ok2 || !ok3 {
		return usageBucket{}, false
	}
	// fraction 允许轻微越界（Google 自己抖过），但离谱的就是别的数组，不是额度桶。
	if frac < -0.05 || frac > 1.05 {
		return usageBucket{}, false
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	reset := usageResetAt(a[3])
	if reset <= 0 {
		return usageBucket{}, false
	}
	b := usageBucket{
		PeriodCode: int(code),
		Limit:      limit,
		LimitKnown: limit > 0,
		Fraction:   frac,
		Percent:    round1(frac * 100),
		ResetAt:    reset,
		ResetInSec: reset - now.Unix(),
	}
	if limit > 0 {
		b.Used = frac * limit
	}
	b.Remaining = round1(100 - b.Percent)
	b.Period = usagePeriodName(b.PeriodCode, reset, now)
	return b, true
}

// usagePeriodName 优先用校准过的枚举表；没有就按重置时间反推。
// 5 小时窗口重置必在 6 小时内；每周重置在 1~8 天内。
func usagePeriodName(code int, resetAt int64, now time.Time) string {
	if n, ok := usagePeriodNames[code]; ok {
		return n
	}
	d := time.Duration(resetAt-now.Unix()) * time.Second
	switch {
	case d > 0 && d <= 6*time.Hour:
		return "rolling_5h"
	case d > 6*time.Hour && d <= 8*24*time.Hour:
		return "weekly"
	}
	return fmt.Sprintf("period_%d", code)
}

// usageResetAt 从 [[sec, nanos], ...] 里取 epoch 秒。
func usageResetAt(v any) int64 {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return 0
	}
	first, ok := arr[0].([]any)
	if !ok || len(first) == 0 {
		return 0
	}
	n, _ := usageNum(first[0])
	if n < 1e9 || n > 4e9 { // 不是合理的 epoch 秒
		return 0
	}
	return int64(n)
}

// usageNum 把 json 解出来的数字统一成 float64（int 也会出现）。
func usageNum(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func round1(f float64) float64 {
	return float64(int64(f*10+0.5)) / 10
}

func headOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// 缓存：内存即可，别落库——这是易失数据，重启丢光无所谓。
type usageCacheEntry struct {
	usage      accountUsage
	cookieHash string // 换 cookie 视为失效，不用去 cookie_pool 加钩子
	fetchedAt  time.Time
}

var (
	usageCacheMu sync.Mutex
	usageCache   = map[int64]usageCacheEntry{}
)

