package service

import (
	"net/http"
	"testing"
	"time"
)

// TestClassifyPassthroughUpstreamHealth 覆盖健康判定的全部分支。
//
// 最关键的两条断言：
//   - 5xx 既不摘除账号也不重试（请求可能已在上游执行了一部分，重试会重复扣费）
//   - 403 只在响应体明确提到额度时才摘除（否则会把"无权访问某资源"误判成
//     "额度耗尽"，把一个健康的 key 摘掉半小时）
func TestClassifyPassthroughUpstreamHealth(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		retryAfter   string
		body         string
		wantNil      bool
		wantRetry    bool
		wantCooldown time.Duration
	}{
		{
			name: "429 限流 → 摘除且可重试", status: http.StatusTooManyRequests,
			wantRetry: true, wantCooldown: passthroughRateLimitCooldown,
		},
		{
			name: "429 带 Retry-After 秒数 → 采纳上游值", status: http.StatusTooManyRequests,
			retryAfter: "30", wantRetry: true, wantCooldown: 31 * time.Second,
		},
		{
			name: "429 带非法 Retry-After → 退回默认值", status: http.StatusTooManyRequests,
			retryAfter: "soon", wantRetry: true, wantCooldown: passthroughRateLimitCooldown,
		},
		{
			name: "402 额度不足 → 长冷却", status: http.StatusPaymentRequired,
			wantRetry: true, wantCooldown: passthroughQuotaCooldown,
		},
		{
			name:   "403 提到 credits → 摘除",
			status: http.StatusForbidden, body: `{"error":"insufficient credits"}`,
			wantRetry: true, wantCooldown: passthroughQuotaCooldown,
		},
		{
			name:   "403 中文额度不足 → 摘除",
			status: http.StatusForbidden, body: `{"message":"账户余额不足"}`,
			wantRetry: true, wantCooldown: passthroughQuotaCooldown,
		},
		{
			// 这条最重要：无权访问某资源不能被当成额度耗尽。
			name:   "403 仅权限问题 → 不摘除",
			status: http.StatusForbidden, body: `{"error":"you do not have access to this resource"}`,
			wantNil: true,
		},
		{
			// 5xx 绝不重试：上游可能已执行了一部分。
			name: "500 → 不摘除不重试", status: http.StatusInternalServerError,
			body: "internal error", wantNil: true,
		},
		{
			name: "502 → 不摘除不重试", status: http.StatusBadGateway, wantNil: true,
		},
		{
			name: "400 参数错误 → 不摘除", status: http.StatusBadRequest,
			body: `{"error":"invalid url"}`, wantNil: true,
		},
		{
			name: "200 → 不摘除", status: http.StatusOK, wantNil: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.retryAfter != "" {
				h.Set("Retry-After", tc.retryAfter)
			}
			got := classifyPassthroughUpstreamHealth(tc.status, h, []byte(tc.body))
			if tc.wantNil {
				if got != nil {
					t.Fatalf("期望不影响账号健康，得到 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("期望产出健康判定，得到 nil")
			}
			if got.Retryable != tc.wantRetry {
				t.Fatalf("可重试标志不符: got %v want %v", got.Retryable, tc.wantRetry)
			}
			if got.Cooldown != tc.wantCooldown {
				t.Fatalf("冷却时长不符: got %v want %v", got.Cooldown, tc.wantCooldown)
			}
			if got.Reason == "" {
				t.Fatal("熔断原因不应为空——后台要靠它排查")
			}
		})
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d, ok := parseRetryAfter("45"); !ok || d != 45*time.Second {
		t.Fatalf("秒数形式解析失败: %v %v", d, ok)
	}
	if _, ok := parseRetryAfter("0"); ok {
		t.Fatal("0 秒应视为无效")
	}
	if _, ok := parseRetryAfter("-5"); ok {
		t.Fatal("负数应视为无效")
	}
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("空值应视为无效")
	}
	// HTTP 日期形式
	future := time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(future); !ok || d <= 0 {
		t.Fatalf("HTTP 日期形式解析失败: %v %v", d, ok)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if _, ok := parseRetryAfter(past); ok {
		t.Fatal("过去的时间应视为无效")
	}
}

func TestContainsPassthroughQuotaSignal(t *testing.T) {
	for _, body := range []string{
		`{"error":"Insufficient credits"}`, `{"msg":"quota exceeded"}`,
		`out of balance`, `额度已用完`, `余额不足`,
	} {
		if !containsPassthroughQuotaSignal([]byte(body)) {
			t.Fatalf("应识别为额度信号: %q", body)
		}
	}
	for _, body := range []string{
		``, `{"error":"invalid parameter"}`, `{"error":"forbidden"}`, `not found`,
	} {
		if containsPassthroughQuotaSignal([]byte(body)) {
			t.Fatalf("不应识别为额度信号: %q", body)
		}
	}
}
