package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// 通用 REST 透传的 HTTP 入口。
//
// 路由形如 ANY /px/:service/*upstream_path，其中 :service 即目标账号的 platform
// 字符串（由路由组的 ForcePlatform 中间件注入 ctx）。这样调度器、并发限制、
// 代理池、USD 计费与配额引擎全部零改动复用。
//
// 本文件刻意只新增，不修改 GatewayHandler 的结构体定义——gateway_handler.go 是
// 上游高频改动文件（半年 25 次提交），加字段会带来长期的合并成本。Go 允许在同
// 包的任意文件里声明方法，因此这里直接以 *GatewayHandler 的方法访问已有依赖。
//
// ============================================================================
// 配置透传 SKU 定价时必读：定价要挂在【分组】的 platform 下，不是透传服务名
// ============================================================================
//
// 直觉上 /px/bailian 的 SKU 定价该配成 platform='bailian'，但那样查不到。原因是
// 一条隐蔽的上下文传递链：
//
//  1. ChannelService.GetChannelModelPricing 经 channelLookupPlatform(ctx, groupPlatform)
//     决定用哪个 platform 去查定价缓存；该函数【优先】取 ctx 里的 ForcePlatform。
//  2. 但计费不在请求协程里做——submitUsageRecordTask 把它丢进 usage worker 池，
//     而 usageRecordContext（openai_gateway_handler.go）只把 ClientRequestID 与
//     RequestID 两个键搬到 worker 的 ctx 上，【ForcePlatform 不搬】。
//  3. 于是计费时 ForcePlatform 缺失，channelLookupPlatform 回退到 groupPlatform。
//
// 结论：channel_model_pricing.platform 必须等于该 key 所属【分组】的 platform
// （例如分组是 openai，就写 'openai'），哪怕账号的 platform 是 'bailian'。
//
// 另有两个排查时容易误判的点：
//   - 渠道定价缓存 TTL 是 10 分钟（channelCacheTTL）。改完定价若不重启，
//     最多要等 10 分钟才生效——期间现象与"平台配错"完全一样，极易误诊。
//   - accounts.extra / credentials 的改动同样被进程缓存，改完需重启才生效。
//
// 还有一条已知限制：Account.IsHeaderOverrideEligible 只对
// anthropic/openai/grok 平台放行，因此 platform 为自定义服务名（bailian 等）的
// 透传账号，其 credentials.header_overrides 【不会生效】。需要非 Bearer 认证
// （如 ElevenLabs 的 xi-api-key）时，当前只能把账号 platform 设为 openai 并靠
// 分组隔离，或扩展该函数的放行列表。

// passthroughMaxAccountSwitches 限制单次请求最多尝试几个账号。
//
// 上限存在的意义是防止在一组全部耗尽的账号上无限打转、把一次请求拖成几十秒。
// 取 3：两把 key 的常见场景下足够（第一把限流、第二把接手），更多账号时也能
// 在一次请求内覆盖多数情况，剩下的交给下一个请求——因为坏账号已被摘除。
const passthroughMaxAccountSwitches = 3

var (
	passthroughServiceOnce sync.Once
	passthroughServiceInst *service.PassthroughService
)

// passthroughService 惰性构造透传服务。
//
// v1 传入 nil 代理解析器：GatewayService 上没有暴露账号代理的解析入口，而为此
// 改动上游文件不值得。已知限制——绑定了出网代理的账号，其透传流量不走代理。
// 后续若需要，补一个 PassthroughProxyResolver 实现即可，无需改动本文件之外的代码。
func passthroughService() *service.PassthroughService {
	passthroughServiceOnce.Do(func() {
		passthroughServiceInst = service.NewPassthroughService(nil)
	})
	return passthroughServiceInst
}

// Passthrough 处理一次通用 REST 透传请求。
func (h *GatewayHandler) Passthrough(c *gin.Context) {
	requestStart := time.Now()

	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return
	}

	serviceName := strings.TrimSpace(c.Param("service"))
	if serviceName == "" {
		h.errorResponse(c, http.StatusNotFound, "not_found_error", "Passthrough service is required")
		return
	}
	upstreamPath := c.Param("upstream_path")

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return
	}

	reqLog := requestLogger(c, "handler.passthrough",
		zap.String("passthrough_service", serviceName),
		zap.String("upstream_path", upstreamPath),
	)

	subscription, _ := middleware2.GetSubscriptionFromContext(c)

	// 配额准入与后续计费都显式传空平台。
	//
	// 原因：user_platform_quotas.platform 同时受 ent Validate 与数据库 CHECK 约束，
	// 只接受 anthropic|openai|gemini|antigravity|grok。而透传把 ForcePlatform 设成了
	// 任意服务名（如 "bailian"），若原样传入，配额层会拿到一个非法平台值。目前
	// 恰好安全（下游有 platform != "" 与 HasUserPlatformQuotaLimit 双重保护），
	// 但离一次重构就会变成告警风暴或写入失败，因此在此显式截断。
	//
	// 语义上也更正确：透传的计费单元是 SKU（按次/按量），不属于任何 LLM 平台配额。
	const passthroughQuotaPlatform = ""

	if err := h.billingCacheService.CheckBillingEligibility(
		c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, passthroughQuotaPlatform,
	); err != nil {
		status, code, message, retryAfter := billingErrorDetails(err)
		if retryAfter > 0 {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
		}
		h.errorResponse(c, status, code, message)
		return
	}

	userRelease, acquired, acquireErr := h.concurrencyHelper.TryAcquireUserSlot(
		c.Request.Context(), subject.UserID, subject.Concurrency,
	)
	if acquireErr != nil {
		reqLog.Warn("passthrough.acquire_user_slot_failed", zap.Error(acquireErr))
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Failed to acquire concurrency slot")
		return
	}
	if !acquired {
		h.errorResponse(c, http.StatusTooManyRequests, "rate_limit_error", "User concurrency limit reached")
		return
	}
	if userRelease != nil {
		defer userRelease()
	}

	// 失败切换循环。
	//
	// 目标是跑满吞吐：一把 key 撞到限额就立刻改投下一把，而不是保守地在本地
	// 预估额度。上游的 429/402 才是权威信号——而且被限流的请求【没有被执行】，
	// 所以换账号重试不会产生重复副作用，对非幂等的任务提交同样安全。
	//
	// 每轮把已试过的账号加入排除集，避免在同一个坏账号上打转。
	excluded := make(map[int64]struct{}, passthroughMaxAccountSwitches)
	var (
		account *service.Account
		out     *service.PassthroughForwardOutput
	)
	for attempt := 0; attempt < passthroughMaxAccountSwitches; attempt++ {
		// ForcePlatform 已由路由组中间件写入 ctx，调度器据此只在该服务的账号里挑选。
		selected, selErr := h.gatewayService.SelectAccountForModelWithExclusions(
			c.Request.Context(), apiKey.GroupID, "", "", excluded,
		)
		if selErr != nil || selected == nil {
			if account != nil && out != nil && out.Health != nil {
				// 账号全部试尽：把最后一次的上游响应如实回传，而不是替换成
				// 网关自己的错误——客户端需要看到上游的原始状态码与错误体。
				reqLog.Warn("passthrough.all_accounts_exhausted",
					zap.Int("attempts", attempt),
					zap.Int("last_upstream_status", out.StatusCode),
				)
				service.WritePassthroughDeferredResponse(c, http.Header{}, out.StatusCode, out.DeferredBody)
				return
			}
			reqLog.Warn("passthrough.no_available_account", zap.Error(selErr))
			h.errorResponse(c, http.StatusServiceUnavailable, "api_error",
				"No available account for passthrough service \""+serviceName+"\"")
			return
		}
		account = selected
		excluded[account.ID] = struct{}{}

		forwardOut, fwdErr := passthroughService().Forward(c.Request.Context(), c, &service.PassthroughForwardInput{
			Account:  account,
			Method:   c.Request.Method,
			Path:     upstreamPath,
			RawQuery: c.Request.URL.RawQuery,
			Body:     body,
			Header:   c.Request.Header,
		})
		if fwdErr != nil {
			h.writePassthroughForwardError(c, reqLog, serviceName, upstreamPath, account, fwdErr)
			return
		}
		out = forwardOut

		if out.Health == nil {
			// 正常响应（含不影响账号健康的客户端错误）：响应已写给客户端。
			break
		}

		// 摘除该账号，使后续请求（以及本循环的下一轮）跳过它。
		if markErr := h.gatewayService.MarkPassthroughAccountUnhealthy(account.ID, out.Health); markErr != nil {
			reqLog.Warn("passthrough.mark_account_unhealthy_failed",
				zap.Int64("account_id", account.ID), zap.Error(markErr))
		}
		reqLog.Warn("passthrough.account_unhealthy",
			zap.Int64("account_id", account.ID),
			zap.Int("upstream_status", out.StatusCode),
			zap.String("reason", out.Health.Reason),
			zap.Duration("cooldown", out.Health.Cooldown),
			zap.Bool("retryable", out.Health.Retryable),
		)

		if !out.Health.Retryable {
			service.WritePassthroughDeferredResponse(c, http.Header{}, out.StatusCode, out.DeferredBody)
			return
		}
		// 可重试：继续下一轮，挑一个未试过的账号。
	}

	// 循环用尽仍未成功（例如账号数超过上限）：回传最后一次的上游响应。
	if out != nil && out.Health != nil {
		reqLog.Warn("passthrough.switch_limit_reached",
			zap.Int("limit", passthroughMaxAccountSwitches),
			zap.Int("last_upstream_status", out.StatusCode),
		)
		service.WritePassthroughDeferredResponse(c, http.Header{}, out.StatusCode, out.DeferredBody)
		return
	}
	if out == nil {
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error", "Passthrough failed to obtain a response")
		return
	}

	// 计费：SKU 作为 usage_logs.model 落库，因此现有按模型的仪表盘、渠道定价与
	// 后台筛选全部免费生效，无需改动 usagestats 聚合层。
	// RequestID 留空：交给 resolveUsageBillingRequestID 沿既有链路回退
	// （client_request_id → local request_id → 生成），与其它端点行为一致。
	billingSKU := out.BillingSKU
	if billingSKU == "" {
		billingSKU = out.Route.SKU
	}
	result := &service.ForwardResult{
		Model:    billingSKU,
		Duration: out.Duration,
	}
	if out.Units > 1 {
		// 多单位（N 页 / N 秒 / N 张）借用图片计费通道：calculateImageCost 会把
		// ImageCount 原样当作 RequestCount，金额精确。代价是行落在 image_count 上。
		result.ImageCount = out.Units
	}

	userAgent := c.Request.UserAgent()
	clientIP := c.ClientIP()
	sessionID := service.ExtractClientSessionID(c)
	// usage_logs 有 ON CONFLICT (request_id, api_key_id) DO NOTHING，而
	// request_id 可能取自客户端头。填入请求体语义哈希，降低复用 X-Request-Id
	// 时静默跳过计费的风险。
	requestPayloadHash := service.HashUsageRequestPayload(body)
	routeTemplate := "/px/" + serviceName + out.Route.RouteTemplate()
	h.submitUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
		if err := h.gatewayService.RecordUsage(ctx, &service.RecordUsageInput{
			Result:        result,
			QuotaPlatform: passthroughQuotaPlatform,
			APIKey:        apiKey,
			User:          apiKey.User,
			Account:       account,
			Subscription:  subscription,
			// 一律使用路由模板而非原始路径：原始路径可能含任务 ID 等高基数片段，
			// 会撑爆 EndpointStat 聚合，且该列是 VARCHAR(128) 有截断风险。
			InboundEndpoint:    routeTemplate,
			UpstreamEndpoint:   out.Route.RouteTemplate(),
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			SessionID:          sessionID,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
		}); err != nil {
			reqLog.Error("passthrough.record_usage_failed",
				zap.Int64("account_id", account.ID),
				zap.String("sku", billingSKU),
				zap.Error(err),
			)
		}
	})

	// 异步任务终态结算：本次轮询观测到任务完成，额外落一条带真实用量的用量行。
	//
	// 幂等不靠这里判重，而是靠 usage_logs 的 (request_id, api_key_id) 唯一索引：
	// 结算行的 request_id 固定为 px-settle:<资源id>，重复轮询写入时被
	// ON CONFLICT DO NOTHING 静默丢弃。因此无需状态表，也不怕并发轮询。
	if st := out.Settlement; st != nil {
		settleResult := &service.ForwardResult{
			RequestID: st.RequestID,
			Model:     st.SKU,
		}
		if st.Units > 1 {
			settleResult.ImageCount = st.Units
		}
		h.submitUsageRecordTask(c.Request.Context(), func(ctx context.Context) {
			// 结算必须用一个不带 client_request_id 的干净 ctx：
			// resolveUsageBillingRequestID 会优先取 ctx 里的 client/local request id，
			// 那样就盖掉了我们精心构造的幂等键，去重也就失效了。
			settleCtx := context.WithoutCancel(context.Background())
			if err := h.gatewayService.RecordUsage(settleCtx, &service.RecordUsageInput{
				Result:           settleResult,
				QuotaPlatform:    passthroughQuotaPlatform,
				APIKey:           apiKey,
				User:             apiKey.User,
				Account:          account,
				Subscription:     subscription,
				InboundEndpoint:  routeTemplate,
				UpstreamEndpoint: out.Route.RouteTemplate(),
				UserAgent:        userAgent,
				IPAddress:        clientIP,
				APIKeyService:    h.apiKeyService,
			}); err != nil {
				reqLog.Error("passthrough.settle_usage_failed",
					zap.String("settle_request_id", st.RequestID),
					zap.String("sku", st.SKU),
					zap.Int("units", st.Units),
					zap.Error(err),
				)
				return
			}
			reqLog.Info("passthrough.settled",
				zap.String("settle_request_id", st.RequestID),
				zap.String("sku", st.SKU),
				zap.Int("units", st.Units),
			)
			_ = ctx
		})
	}

	reqLog.Info("passthrough.completed",
		zap.Int64("account_id", account.ID),
		zap.String("sku", billingSKU),
		zap.Int("upstream_status", out.StatusCode),
		zap.Int("units", out.Units),
		zap.Int64("total_ms", time.Since(requestStart).Milliseconds()),
	)
}

// writePassthroughForwardError 把转发失败翻译成客户端可见的错误。
func (h *GatewayHandler) writePassthroughForwardError(
	c *gin.Context,
	reqLog *zap.Logger,
	serviceName, upstreamPath string,
	account *service.Account,
	err error,
) {
	switch {
	case errors.Is(err, service.ErrPassthroughRouteNotAllowed):
		// 默认拒绝：未在账号白名单里的路径一律本地 404，绝不打到上游。
		// 若放行让上游返回 4xx，某些上游的 403（如智谱的"无权访问某模型"）
		// 会被识别成账号级故障，把整个账号熔断十分钟、殃及其他正常路径。
		service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalFeatureGate)
		h.errorResponse(c, http.StatusNotFound, "not_found_error",
			"Path \""+upstreamPath+"\" is not allowed for passthrough service \""+serviceName+"\"")
	case errors.Is(err, service.ErrPassthroughNoBaseURL):
		reqLog.Error("passthrough.account_missing_base_url", zap.Int64("account_id", account.ID))
		h.errorResponse(c, http.StatusServiceUnavailable, "api_error",
			"Passthrough account is not configured with a base_url")
	default:
		reqLog.Warn("passthrough.forward_failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		// 到这里可能已经写过响应头（流式回传中途失败），此时不能再写一次。
		if !c.Writer.Written() {
			h.errorResponse(c, http.StatusBadGateway, "api_error", "Upstream request failed")
		}
	}
}
