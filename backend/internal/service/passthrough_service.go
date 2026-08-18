package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 通用透传转发器。
//
// 目标：把任意非 LLM 的 REST 上游（火山方舟视频、百炼 TTS/ASR、Firecrawl 等）
// 接进现有网关，同时完整复用账号调度、代理池、并发限制、USD 计费与配额引擎——
// 这些能力都不需要为透传单独实现，只要在 ctx 上设 ForcePlatform 并最终产出一个
// ForwardResult 交给 RecordUsage 即可。
//
// 与 LLM 转发路径的三点关键差异：
//  1. 不解析、不改写请求体与响应体：透传只换认证，语义完全交给上游。
//  2. 默认单次尝试：透传的 POST 多为非幂等的任务提交，跨账号重试会重复扣费。
//  3. 计费单元是路由定义里的 SKU 字符串，写进 usage_logs.model 复用全部按模型统计。

// passthroughMaxCaptureBytes 限制为提取计费数量而缓存的响应字节数。
// 超出部分直接流式转发，避免大响应把内存打满。
const passthroughMaxCaptureBytes = 64 << 10

// passthroughRequestTimeout 是单次透传请求的总超时。
// 取值偏大是因为抓取/渲染类上游常有数十秒的同步等待。
const passthroughRequestTimeout = 10 * time.Minute

// passthroughSettleRequestIDPrefix 是结算用量行的 request_id 前缀。
//
// 幂等完全依赖 usage_logs 上的 (request_id, api_key_id) 唯一索引：同一个异步
// 任务被轮询多少次，结算行只会成功写入一次，其余被 ON CONFLICT DO NOTHING
// 静默丢弃。这样就不必为"哪些任务已结算"再维护一张状态表。
const passthroughSettleRequestIDPrefix = "px-settle:"

// hopByHopHeaders 是逐跳头，按 RFC 7230 不得转发给上游。
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

var (
	// ErrPassthroughRouteNotAllowed 表示请求路径不在账号的白名单里。
	// 调用方必须返回 404 且不接触上游：若放行让上游返回 4xx，某些上游的
	// 403（如智谱的"无权访问某模型"）会被识别成账号级故障，导致整个账号
	// 被熔断十分钟，殃及其他正常模型。
	ErrPassthroughRouteNotAllowed = errors.New("passthrough: route not allowed")
	// ErrPassthroughNoBaseURL 表示账号未配置 base_url。
	ErrPassthroughNoBaseURL = errors.New("passthrough: account has no base_url")
)

// PassthroughProxyResolver 解析账号绑定的出网代理。
type PassthroughProxyResolver interface {
	ResolveAccountProxyURL(ctx context.Context, account *Account) string
}

// PassthroughService 执行通用 REST 透传。
type PassthroughService struct {
	proxies PassthroughProxyResolver
	// allowPrivateHosts 仅供包内测试打开（httptest 跑在 127.0.0.1 上）。
	// 生产构造路径永不设置它，因此透传流量始终拒绝解析到私有地址的目标——
	// 这是防 DNS Rebinding 与 169.254.169.254 云元数据端点的关键一环。
	// 刻意不做成配置项：全局 security.url_allowlist.allow_private_hosts 在本
	// 部署里是 true，而透传是 SSRF 风险最高的入口，不应跟随那个宽松默认值。
	allowPrivateHosts bool
}

// NewPassthroughService 构造透传服务。proxies 可为 nil（表示不使用代理）。
func NewPassthroughService(proxies PassthroughProxyResolver) *PassthroughService {
	return &PassthroughService{proxies: proxies}
}

// PassthroughForwardInput 是一次透传转发的输入。
type PassthroughForwardInput struct {
	Account  *Account
	Method   string
	Path     string // 客户端请求中 :service 之后的上游路径
	RawQuery string
	Body     []byte
	Header   http.Header
}

// PassthroughForwardOutput 汇总转发结果，供上层构造 ForwardResult 与计费。
type PassthroughForwardOutput struct {
	Route      PassthroughRoute
	StatusCode int
	Duration   time.Duration
	// Units 是计费数量，默认 1。来源优先级：
	// units_request_json_path（请求体）> units_json_path（响应体）> 1。
	Units int
	// BillingSKU 是最终用于计费的 SKU（写入 usage_logs.model）。
	// 配了 sku_suffix_json_path 时形如 "bailian/video-gen:1080P"，否则等于 Route.SKU。
	BillingSKU string
	// Health 非 nil 表示上游响应指示该账号应被暂时摘除（限流/额度耗尽）。
	// 此时响应体【尚未】写给客户端，调用方可以换一个账号重试。
	Health *PassthroughAccountHealth
	// DeferredBody 是被暂缓回传的响应体（仅当 Health 非 nil）。
	// 调用方若决定不再重试，必须把它连同 StatusCode 一起回写给客户端。
	DeferredBody []byte
	// Settlement 非 nil 表示本次轮询观测到了异步任务的终态，调用方应额外落一条
	// 结算用量行。重复轮询会重复产生该结构，靠 usage_logs 的
	// (request_id, api_key_id) 唯一索引去重，因此调用方无需自行判重。
	Settlement *PassthroughSettlement
}

// PassthroughSettlement 描述一次异步任务的终态结算。
type PassthroughSettlement struct {
	// SKU 是结算行的计费单元。
	SKU string
	// Units 是上游报出的真实用量。
	Units int
	// RequestID 是幂等键，形如 "px-settle:<资源id>"。
	RequestID string
}

// Forward 校验路由、转发请求并把响应流式回写给客户端。
//
// 无论上游成功与否都会把响应原样回传（状态码、响应头、响应体），因为透传的
// 语义是"中继而非建模"——把上游的错误改写成网关自己的格式反而会让客户端难以
// 按上游文档排错。
func (s *PassthroughService) Forward(
	ctx context.Context,
	c *gin.Context,
	in *PassthroughForwardInput,
) (*PassthroughForwardOutput, error) {
	if in == nil || in.Account == nil {
		return nil, errors.New("passthrough: account is required")
	}

	routes, err := ParsePassthroughRoutes(in.Account.Extra)
	if err != nil {
		return nil, fmt.Errorf("passthrough: invalid route config on account %d: %w", in.Account.ID, err)
	}
	route, clientSuffix, ok := MatchPassthroughRoute(routes, in.Method, in.Path)
	if !ok {
		return nil, ErrPassthroughRouteNotAllowed
	}

	baseURL := strings.TrimSpace(in.Account.GetCredential("base_url"))
	if baseURL == "" {
		return nil, ErrPassthroughNoBaseURL
	}
	upstreamPath := route.Path
	if route.IsPrefix() {
		upstreamPath = route.prefixBase()
	}
	targetURL, err := buildPassthroughURL(baseURL, upstreamPath, clientSuffix, in.RawQuery)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if s.proxies != nil {
		proxyURL = s.proxies.ResolveAccountProxyURL(ctx, in.Account)
	}
	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL: proxyURL,
		Timeout:  passthroughRequestTimeout,
		// 透传的目标由管理员配置、路径由客户端影响，是 SSRF 的高危面，
		// 因此强制校验解析后的 IP 并禁止私有地址（防 DNS Rebinding
		// 与 169.254.169.254 这类云元数据端点）。
		ValidateResolvedIP: true,
		AllowPrivateHosts:  s.allowPrivateHosts,
	})
	if err != nil {
		return nil, fmt.Errorf("passthrough: build http client: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, in.Method, targetURL, bytes.NewReader(in.Body))
	if err != nil {
		return nil, fmt.Errorf("passthrough: build request: %w", err)
	}
	applyPassthroughRequestHeaders(req, in.Header, in.Account)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("passthrough: upstream request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 先判账号健康：若是限流/额度耗尽，本次响应【不能】直接写给客户端——
	// 一旦写了响应头就再没有换账号重试的机会。
	//
	// 判定需要看响应体（403 要靠关键词区分"额度耗尽"与"无权访问"），而 body 是
	// 一次性的流，所以先把错误响应完整读出来再判。只对非 2xx 这么做：成功响应
	// 可能很大，必须保持零拷贝流式转发。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, passthroughMaxCaptureBytes))
		if health := classifyPassthroughUpstreamHealth(resp.StatusCode, resp.Header, body); health != nil {
			return &PassthroughForwardOutput{
				Route:        route,
				StatusCode:   resp.StatusCode,
				Duration:     time.Since(start),
				Units:        1,
				BillingSKU:   resolvePassthroughBillingSKU(route, in.Body),
				Health:       health,
				DeferredBody: body,
			}, nil
		}
		// 不影响账号健康的错误（如客户端参数错误）：原样回传，行为与之前一致。
		writePassthroughDeferredResponse(c, resp.Header, resp.StatusCode, body)
		return &PassthroughForwardOutput{
			Route:      route,
			StatusCode: resp.StatusCode,
			Duration:   time.Since(start),
			Units:      1,
			BillingSKU: resolvePassthroughBillingSKU(route, in.Body),
		}, nil
	}

	units, head, err := s.relayResponse(c, resp, route)
	if err != nil {
		return nil, err
	}
	// 请求体来源优先：异步任务上游在提交时只回 task_id，真实用量要等任务完成，
	// 但计量参数（如视频秒数）在提交请求里已经给全了。
	if route.UnitsRequestJSONPath != "" {
		if n := gjson.GetBytes(in.Body, route.UnitsRequestJSONPath); n.Exists() {
			if v := int(n.Int()); v > 0 {
				units = v
			}
		}
	}

	return &PassthroughForwardOutput{
		Route:      route,
		StatusCode: resp.StatusCode,
		Duration:   time.Since(start),
		Units:      units,
		BillingSKU: resolvePassthroughBillingSKU(route, in.Body),
		Settlement: resolvePassthroughSettlement(route, head, in.Path),
	}, nil
}

// resolvePassthroughSettlement 检查响应是否命中异步任务终态，若命中则构造结算信息。
//
// head 是已捕获的响应体前缀；未配置结算或未捕获响应体时返回 nil。
func resolvePassthroughSettlement(route PassthroughRoute, head []byte, clientPath string) *PassthroughSettlement {
	if !route.HasSettlement() || len(head) == 0 {
		return nil
	}
	state := gjson.GetBytes(head, route.SettleWhenJSONPath)
	if !state.Exists() || !strings.EqualFold(strings.TrimSpace(state.String()), route.SettleWhenEquals) {
		return nil
	}
	unitsValue := gjson.GetBytes(head, route.SettleUnitsJSONPath)
	if !unitsValue.Exists() {
		return nil
	}
	units := int(unitsValue.Int())
	if units <= 0 {
		// 用量为 0 或负数不产生结算行：既避免落一条无意义的零额记录，
		// 也避免把幂等键提前占掉、导致真实用量到达时被去重掉。
		return nil
	}

	resourceID := ""
	if route.SettleIDJSONPath != "" {
		resourceID = strings.TrimSpace(gjson.GetBytes(head, route.SettleIDJSONPath).String())
	}
	if resourceID == "" {
		// 退回使用客户端路径的最后一段：前缀路由下即任务 id。
		trimmed := strings.Trim(strings.TrimSpace(clientPath), "/")
		if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
			trimmed = trimmed[idx+1:]
		}
		resourceID = trimmed
	}
	if resourceID == "" {
		return nil
	}
	return &PassthroughSettlement{
		SKU:       route.SettlementSKU(),
		Units:     units,
		RequestID: passthroughSettleRequestIDPrefix + resourceID,
	}
}

// resolvePassthroughBillingSKU 计算最终计费 SKU。
//
// 配了 sku_suffix_json_path 时，从请求体取该值并以 : 追加到基础 SKU 后面，
// 用于按档位计价（如分辨率）。取不到值时退回基础 SKU——宁可用基础档的价，
// 也不要生成一个定价表里不存在的 SKU 而按 0 计费。
func resolvePassthroughBillingSKU(route PassthroughRoute, body []byte) string {
	if route.SKUSuffixJSONPath == "" {
		return route.SKU
	}
	v := gjson.GetBytes(body, route.SKUSuffixJSONPath)
	if !v.Exists() {
		return route.SKU
	}
	suffix := strings.TrimSpace(v.String())
	if suffix == "" {
		return route.SKU
	}
	return route.SKU + ":" + suffix
}

// relayResponse 把上游响应原样回写客户端，并在需要时提取计费数量。
func (s *PassthroughService) relayResponse(c *gin.Context, resp *http.Response, route PassthroughRoute) (int, []byte, error) {
	for name, values := range resp.Header {
		if _, blocked := hopByHopHeaders[strings.ToLower(name)]; blocked {
			continue
		}
		for _, v := range values {
			c.Writer.Header().Add(name, v)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)

	units := 1
	// 需要读响应体的两种情形：从响应取计费数量，或检测异步任务终态以结算。
	needBody := route.UnitsJSONPath != "" || route.HasSettlement()
	if !needBody || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 无需解析响应体：直接零拷贝流式转发，不缓冲。
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			return units, nil, fmt.Errorf("passthrough: relay body: %w", err)
		}
		return units, nil, nil
	}

	// 需要从响应体取数量：只缓存前 N 字节用于解析，其余继续流式转发，
	// 避免大响应把内存打满。
	head := make([]byte, 0, passthroughMaxCaptureBytes)
	limited := io.LimitReader(resp.Body, passthroughMaxCaptureBytes)
	buf, readErr := io.ReadAll(limited)
	head = append(head, buf...)
	if _, err := c.Writer.Write(head); err != nil {
		return units, nil, fmt.Errorf("passthrough: relay body head: %w", err)
	}
	if readErr == nil {
		if _, err := io.Copy(c.Writer, resp.Body); err != nil {
			return units, head, fmt.Errorf("passthrough: relay body tail: %w", err)
		}
	}
	if route.UnitsJSONPath != "" {
		if n := gjson.GetBytes(head, route.UnitsJSONPath); n.Exists() {
			if v := int(n.Int()); v > 0 {
				units = v
			}
		}
	}
	return units, head, nil
}

// applyPassthroughRequestHeaders 构造发往上游的请求头。
//
// 安全要点：
//   - 剥离逐跳头（RFC 7230 要求）。
//   - 绝不转发客户端的 Authorization——那是 sub2api 自己签发的 key，
//     泄露给上游没有意义且扩大凭证暴露面；上游认证一律由账号凭证提供。
//   - 不转发 Host：客户端伪造 Host 可诱导上游路由到别处。
//   - 账号的 header_overrides 最后应用，因此可以覆盖前面任何一项，
//     这是配置 x-api-key、xi-api-key 等非 Bearer 认证形态的入口。
//
// ⚠️ header_overrides 的生效前提：Account.IsHeaderOverrideEligible 只对
// anthropic/openai/grok 平台放行。platform 为自定义透传服务名（bailian、
// firecrawl 等）的账号，ApplyHeaderOverrides 会静默无操作——不会报错，只是
// 请求头没被改写，表现为上游 401。排查时容易误判成凭证配错。
func applyPassthroughRequestHeaders(req *http.Request, src http.Header, account *Account) {
	for name, values := range src {
		lower := strings.ToLower(name)
		if _, blocked := hopByHopHeaders[lower]; blocked {
			continue
		}
		switch lower {
		case "authorization", "host", "content-length", "x-api-key":
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey := strings.TrimSpace(account.GetCredential("api_key")); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	account.ApplyHeaderOverrides(req.Header)
}

// WritePassthroughDeferredResponse 把被暂缓的上游响应回写给客户端。
//
// 用于失败切换耗尽所有账号后，仍需把最后一次的上游响应如实返回——透传的语义是
// 中继而非建模，客户端应当看到上游的原始状态码与错误体，才能按上游文档排错。
func WritePassthroughDeferredResponse(c *gin.Context, header http.Header, statusCode int, body []byte) {
	writePassthroughDeferredResponse(c, header, statusCode, body)
}

func writePassthroughDeferredResponse(c *gin.Context, header http.Header, statusCode int, body []byte) {
	if c == nil || c.Writer.Written() {
		return
	}
	for name, values := range header {
		if _, blocked := hopByHopHeaders[strings.ToLower(name)]; blocked {
			continue
		}
		for _, v := range values {
			c.Writer.Header().Add(name, v)
		}
	}
	c.Writer.WriteHeader(statusCode)
	if len(body) > 0 {
		_, _ = c.Writer.Write(body)
	}
}
