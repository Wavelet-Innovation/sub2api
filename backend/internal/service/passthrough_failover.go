package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 透传层的账号健康标记与失败切换。
//
// 设计取向：**反应式，不预测**。
//
// 一个直觉的做法是在本地统计"这把 key 本分钟用了多少额度"，撞到阈值就换。但那要求
// 我们准确复刻上游的窗口语义——滑动还是固定？按 UTC 还是本地时区重置？并发请求
// 之间怎么计数？猜错的结果是要么保守浪费额度，要么仍然超限。
//
// 上游才是权威。所以改为：拿到限流信号时把该账号标记为临时不可调度，请求立刻改投
// 下一个账号；冷却到期自动恢复。不需要知道上游任何窗口细节。
//
// 关键安全依据：**被限流的请求没有被执行**，因此换账号重试不会产生重复副作用——
// 即使对非幂等的任务提交接口也安全。这与 5xx 截然不同（5xx 可能已执行了一半），
// 所以下面严格区分这两类。

const (
	// passthroughRateLimitCooldown 是收到限流信号后的默认冷却时长。
	//
	// 取 65 秒是因为常见的上游限额以"每分钟"为窗口（如 MinerU 的 50 文件/分钟），
	// 略超一分钟可以确保窗口已经滚过，同时不会让账号闲置过久——目标是跑满吞吐。
	passthroughRateLimitCooldown = 65 * time.Second

	// passthroughQuotaCooldown 是额度耗尽（非速率）时的冷却时长。
	//
	// 取 30 分钟：日额度耗尽要等到次日，但我们不猜上游的重置时刻，而是每半小时
	// 放一个请求去试探。代价是每半小时可能有一次失败，换来的是重置后能自动恢复
	// 而不必人工干预。
	passthroughQuotaCooldown = 30 * time.Minute
)

// passthroughQuotaExhaustedKeywords 用于把"额度耗尽"从普通的 4xx 里区分出来。
// 只在响应体命中这些词时才按长冷却处理，避免把参数错误之类误判成额度问题。
var passthroughQuotaExhaustedKeywords = []string{
	"insufficient",
	"quota",
	"credit",
	"balance",
	"exhaust",
	"out of",
	"额度",
	"余额",
	"用完",
	"不足",
}

// PassthroughAccountHealth 描述一次上游响应对账号健康状态的判定。
type PassthroughAccountHealth struct {
	// Cooldown 大于零表示应把账号标记为临时不可调度这么久。
	Cooldown time.Duration
	// Reason 写入账号的熔断原因，便于后台排查。
	Reason string
	// Retryable 表示可以立即换一个账号重试本次请求。
	//
	// 仅在"请求确定未被执行"时为 true（限流、额度耗尽），因此对非幂等接口
	// 同样安全。5xx 一律为 false。
	Retryable bool
}

// classifyPassthroughUpstreamHealth 根据上游响应判断账号是否应被暂时摘除。
//
// 返回 nil 表示这次响应不影响账号健康（正常响应，或客户端自身的参数错误）。
func classifyPassthroughUpstreamHealth(statusCode int, header http.Header, body []byte) *PassthroughAccountHealth {
	switch statusCode {
	case http.StatusTooManyRequests:
		// 速率限制。优先采纳上游给出的 Retry-After，它比我们的默认值准确。
		cooldown := passthroughRateLimitCooldown
		if d, ok := parseRetryAfter(header.Get("Retry-After")); ok {
			// 加一秒余量，避免边界上再撞一次。
			cooldown = d + time.Second
		}
		return &PassthroughAccountHealth{
			Cooldown:  cooldown,
			Reason:    "passthrough_429_rate_limited",
			Retryable: true,
		}

	case http.StatusPaymentRequired:
		// 402 语义明确就是余额/额度不足。
		return &PassthroughAccountHealth{
			Cooldown:  passthroughQuotaCooldown,
			Reason:    "passthrough_402_quota_exhausted",
			Retryable: true,
		}

	case http.StatusForbidden:
		// 403 含义太宽：可能是额度耗尽，也可能是这个 key 无权访问某个资源。
		// 只有响应体明确提到额度/余额时才摘除账号——否则把"权限不足"误判成
		// "额度耗尽"，会把一个完全健康的 key 摘掉半小时。
		if !containsPassthroughQuotaSignal(body) {
			return nil
		}
		return &PassthroughAccountHealth{
			Cooldown:  passthroughQuotaCooldown,
			Reason:    "passthrough_403_quota_exhausted",
			Retryable: true,
		}
	}

	// 其余状态码（含 5xx）不摘除账号、不重试。
	//
	// 5xx 尤其不能重试：请求可能已在上游执行了一部分，换账号重试会产生重复
	// 副作用（比如重复提交一次解析任务、重复扣费）。
	return nil
}

// containsPassthroughQuotaSignal 判断响应体是否包含额度耗尽的语义。
func containsPassthroughQuotaSignal(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	// 只看前若干字节：额度类错误信息一定在响应开头，而完整读取大响应没有必要。
	const maxScan = 4 << 10
	if len(body) > maxScan {
		body = body[:maxScan]
	}
	lower := strings.ToLower(string(body))
	for _, kw := range passthroughQuotaExhaustedKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// parseRetryAfter 解析 Retry-After 头，支持秒数与 HTTP 日期两种形式。
func parseRetryAfter(value string) (time.Duration, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(trimmed); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(trimmed); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// PassthroughAccountMarker 把账号标记为临时不可调度。
// 由 accountRepo 实现（SetTempUnschedulable）。
type PassthroughAccountMarker interface {
	SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error
}

// markPassthroughAccountUnhealthy 摘除账号，使调度器在冷却期内跳过它。
//
// 用独立的后台 ctx：客户端连接随时可能断开，而摘除动作必须完成——否则下一个
// 请求还会打到同一个已知不可用的账号上。
func markPassthroughAccountUnhealthy(
	marker PassthroughAccountMarker,
	accountID int64,
	health *PassthroughAccountHealth,
) error {
	if marker == nil || health == nil || health.Cooldown <= 0 {
		return nil
	}
	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return marker.SetTempUnschedulable(bgCtx, accountID, time.Now().Add(health.Cooldown), health.Reason)
}

// MarkPassthroughAccountUnhealthy 把账号标记为临时不可调度，供 handler 在透传
// 失败切换时调用。
//
// 单独在这里暴露而不改 gateway_service.go：后者是上游高频改动文件，加方法会带来
// 长期合并成本。Go 允许同包任意文件声明方法，所以这个薄封装放在本文件即可。
func (s *GatewayService) MarkPassthroughAccountUnhealthy(accountID int64, health *PassthroughAccountHealth) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	return markPassthroughAccountUnhealthy(s.accountRepo, accountID, health)
}
