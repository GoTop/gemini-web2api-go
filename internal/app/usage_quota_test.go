package app

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

// usageBody 拼一个 batchexecute 响应信封，形状跟真响应一致。
//
// 合成数据只防得住逻辑错，防不住「Google 换了字段顺序」——所以另有
// TestParseUsageBucketsRealResponse 读 testdata/usage_jSf9Qc.txt（2026-10-28 抓的真响应）。
func usageBody(payload any) string {
	inner, _ := json.Marshal(payload)
	env, _ := json.Marshal([]any{[]any{"wrb.fr", "jSf9Qc", string(inner)}})
	return ")]}'\n\n1\n" + string(env) + "\n"
}

// 真响应回归：2026-10-28 20:21 (+08) 从春川抓的，账号 oliviataylor198306。
// 原始 body：
//
//	[["wrb.fr","jSf9Qc","[2,[[0,1,1,[[1793200028,905962000]]],
//	                        [24067,0.50257355,2,[[1793293628,906026000]]]],false]", ...]]
//
// 两个桶的语义已跟网页 /usage 对过：period=2 的 reset 1793293628 = 2026-10-30 01:07:08 (+08)，
// 正是截图里的「每周限额…重置 10 月 30 日 1:07」。
func TestParseUsageBucketsRealResponse(t *testing.T) {
	raw, err := os.ReadFile("testdata/usage_jSf9Qc.txt")
	if err != nil {
		t.Fatalf("读 fixture 失败: %v", err)
	}
	// 抓取时刻（SAPISIDHASH 的时间戳），用它当 now 才能复现 reset_in_sec
	now := time.Unix(1793190121, 0)

	bs := parseUsageBuckets(string(raw), now)
	if len(bs) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(bs), bs)
	}

	// 按 reset 升序：先 5 小时桶
	win := bs[0]
	if win.Period != "rolling_5h" || win.PeriodCode != 1 {
		t.Errorf("bucket[0] 应为 rolling_5h/code=1，实得 %q/%d", win.Period, win.PeriodCode)
	}
	if win.Percent != 100 || win.Remaining != 0 {
		t.Errorf("5h 桶应为 100%% 用满，实得 percent=%v remaining=%v", win.Percent, win.Remaining)
	}
	if win.LimitKnown || win.Used != 0 {
		t.Errorf("5h 桶的 limit 恒为 0，不该标成 known：limit_known=%v used=%v",
			win.LimitKnown, win.Used)
	}
	if win.ResetAt != 1793200028 {
		t.Errorf("5h reset_at = %d, want 1793200028", win.ResetAt)
	}

	wk := bs[1]
	if wk.Period != "weekly" || wk.PeriodCode != 2 {
		t.Errorf("bucket[1] 应为 weekly/code=2，实得 %q/%d", wk.Period, wk.PeriodCode)
	}
	if wk.Limit != 24067 || !wk.LimitKnown {
		t.Errorf("每周 limit 应为 24067 且 known，实得 %v/%v", wk.Limit, wk.LimitKnown)
	}
	if wk.Percent != 50.3 {
		t.Errorf("每周 percent = %v, want 50.3", wk.Percent)
	}
	if wk.ResetAt != 1793293628 {
		t.Errorf("每周 reset_at = %d, want 1793293628", wk.ResetAt)
	}
	// 0.50257355 * 24067 ≈ 12095.4 compute 点
	if math.Abs(wk.Used-12095.4) > 0.5 {
		t.Errorf("每周 used = %v, want ≈12095.4", wk.Used)
	}
	if wk.ResetInSec != 1793293628-1793190121 {
		t.Errorf("reset_in_sec = %d, want %d", wk.ResetInSec, 1793293628-1793190121)
	}
}

func TestParseUsageBuckets(t *testing.T) {
	now := time.Unix(1792147200, 0)
	payload := []any{nil, []any{
		// 每周桶：3 天后重置
		[]any{500.0, 0.44, 2.0, []any{[]any{float64(now.Unix() + 3*86400), 0.0}}},
		// 5 小时桶：1 小时后重置，已用满
		[]any{500.0, 1.0, 1.0, []any{[]any{float64(now.Unix() + 3600), 0.0}}},
	}}

	bs := parseUsageBuckets(usageBody(payload), now)
	if len(bs) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(bs), bs)
	}
	// 按 reset_at 升序：先 5 小时桶
	if bs[0].Period != "rolling_5h" || bs[0].Percent != 100 || bs[0].Remaining != 0 {
		t.Errorf("bucket[0] = %+v, want rolling_5h / 100%% / 0 remaining", bs[0])
	}
	if bs[0].ResetInSec != 3600 {
		t.Errorf("bucket[0].ResetInSec = %d, want 3600", bs[0].ResetInSec)
	}
	if bs[1].Period != "weekly" || bs[1].Percent != 44 || bs[1].Remaining != 56 {
		t.Errorf("bucket[1] = %+v, want weekly / 44%% / 56 remaining", bs[1])
	}
	if bs[1].Used != 220 { // 0.44 * 500
		t.Errorf("bucket[1].Used = %v, want 220", bs[1].Used)
	}
	if bs[1].PeriodCode != 2 {
		t.Errorf("bucket[1].PeriodCode = %d, want 2 (原始枚举要留着校准)", bs[1].PeriodCode)
	}
}

// 形状不对的数组一律跳过，不能整体拒收，也不能 panic。
func TestParseUsageBucketsSkipsWrongShape(t *testing.T) {
	now := time.Unix(1792147200, 0)
	reset := []any{[]any{float64(now.Unix() + 3600), 0.0}}
	payload := []any{nil, []any{
		[]any{500.0, 7.0, 1.0, reset},               // fraction 越界
		[]any{500.0, 0.3, 1.0},                      // 缺 reset
		[]any{"x", 0.3, 1.0, reset},                 // limit 不是数字
		[]any{500.0, 0.3, 1.0, []any{[]any{5.0, 0}}}, // reset 不是合理 epoch
		[]any{500.0, 0.3, 1.0, reset},               // ✅ 唯一合法的
	}}
	bs := parseUsageBuckets(usageBody(payload), now)
	if len(bs) != 1 {
		t.Fatalf("want 1 valid bucket, got %d: %+v", len(bs), bs)
	}
	if bs[0].Percent != 30 {
		t.Errorf("Percent = %v, want 30", bs[0].Percent)
	}
}

// fraction 轻微越界要钳到 [0,1]，不能把 102% 原样吐给前端。
func TestUsageBucketClampsFraction(t *testing.T) {
	now := time.Unix(1792147200, 0)
	payload := []any{nil, []any{
		[]any{100.0, 1.02, 1.0, []any{[]any{float64(now.Unix() + 3600), 0.0}}},
	}}
	bs := parseUsageBuckets(usageBody(payload), now)
	if len(bs) != 1 {
		t.Fatalf("want 1 bucket, got %d", len(bs))
	}
	if bs[0].Fraction != 1.0 || bs[0].Percent != 100 || bs[0].Remaining != 0 {
		t.Errorf("clamp failed: %+v", bs[0])
	}
}

// RPC 被拒时 payload 是 null，解析要返回空而不是崩。
func TestParseUsageBucketsNullPayload(t *testing.T) {
	body := ")]}'\n\n" + `[["wrb.fr","jSf9Qc",null,null,null,null,"generic"]]` + "\n"
	if bs := parseUsageBuckets(body, time.Now()); len(bs) != 0 {
		t.Fatalf("want 0 buckets for null payload, got %+v", bs)
	}
	if bs := parseUsageBuckets("", time.Now()); len(bs) != 0 {
		t.Fatalf("want 0 buckets for empty body")
	}
}

// 枚举表没填时靠重置时间反推；两个都对不上就退回 period_<code>，别瞎猜。
func TestUsagePeriodNameFallback(t *testing.T) {
	now := time.Unix(1792147200, 0)
	cases := []struct {
		code  int
		reset int64
		want  string
	}{
		{7, now.Unix() + 2*3600, "rolling_5h"},
		{8, now.Unix() + 5*86400, "weekly"},
		{9, now.Unix() + 30*86400, "period_9"},
	}
	for _, c := range cases {
		if got := usagePeriodName(c.code, c.reset, now); got != c.want {
			t.Errorf("usagePeriodName(%d, +%ds) = %q, want %q",
				c.code, c.reset-now.Unix(), got, c.want)
		}
	}
}
