package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// newPassthroughTestAccount 构造一个指向 baseURL 的透传账号。
func newPassthroughTestAccount(baseURL string, routes []any) *Account {
	return &Account{
		ID:       42,
		Platform: "testsvc",
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url": baseURL,
			"api_key":  "upstream-secret-key",
		},
		Extra: map[string]any{
			"passthrough": map[string]any{"routes": routes},
		},
	}
}

// newPassthroughTestService 构造允许私有地址的透传服务。
// 生产路径（NewPassthroughService）不开这个开关，httptest 跑在 127.0.0.1 上
// 才需要它——这也顺带证明了默认策略确实会拦下私有地址。
func newPassthroughTestService() *PassthroughService {
	svc := NewPassthroughService(nil)
	svc.allowPrivateHosts = true
	return svc
}

func passthroughTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return c, rec
}

// TestPassthroughForwardStripsClientCredentials 是本层最重要的安全断言：
// 客户端自己的凭证与 Host 绝不能被转发给上游，上游认证只能来自账号凭证。
//
// 若把客户端的 Authorization（即 sub2api 自己签发的 key）转发出去，等于把内部
// 凭证泄露给每一个上游供应商；若转发客户端伪造的 Host，可诱导上游路由到别处。
func TestPassthroughForwardStripsClientCredentials(t *testing.T) {
	var got http.Header
	var gotHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotHost = r.Host
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{"method": "POST", "path": "/echo", "sku": "test/echo"},
	})
	c, rec := passthroughTestContext()

	clientHeader := http.Header{}
	clientHeader.Set("Authorization", "Bearer sk-sub2api-internal-key")
	clientHeader.Set("X-Api-Key", "client-supplied-key")
	clientHeader.Set("Host", "evil.example.com")
	clientHeader.Set("Connection", "keep-alive")
	clientHeader.Set("Transfer-Encoding", "chunked")
	clientHeader.Set("X-Custom-Passthrough", "keep-me")

	out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account,
		Method:  http.MethodPost,
		Path:    "/echo",
		Body:    []byte(`{"a":1}`),
		Header:  clientHeader,
	})
	if err != nil {
		t.Fatalf("转发失败: %v", err)
	}
	if out.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，得到 %d", out.StatusCode)
	}

	if v := got.Get("Authorization"); v != "Bearer upstream-secret-key" {
		t.Fatalf("上游 Authorization 应来自账号凭证，得到 %q", v)
	}
	if strings.Contains(got.Get("Authorization"), "sub2api-internal") {
		t.Fatal("客户端 Authorization 被泄露给了上游")
	}
	if v := got.Get("X-Api-Key"); v != "" {
		t.Fatalf("客户端 X-Api-Key 不应转发，得到 %q", v)
	}
	if strings.Contains(gotHost, "evil.example.com") {
		t.Fatalf("客户端伪造的 Host 被转发，得到 %q", gotHost)
	}
	for _, h := range []string{"Connection", "Transfer-Encoding"} {
		if got.Get(h) != "" && got.Get(h) == clientHeader.Get(h) {
			t.Fatalf("逐跳头 %s 不应原样转发", h)
		}
	}
	if v := got.Get("X-Custom-Passthrough"); v != "keep-me" {
		t.Fatalf("普通自定义头应保留，得到 %q", v)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("响应体未回传，得到 %q", body)
	}
}

// TestPassthroughForwardDeniesUnlistedPath 断言默认拒绝，且【不发出任何上游请求】。
//
// 这一点是账号稳定性的关键：若放行让上游返回 4xx，某些上游的 403（如智谱的
// "无权访问某模型"）会被识别成账号级故障，把整个账号熔断十分钟。
func TestPassthroughForwardDeniesUnlistedPath(t *testing.T) {
	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{"method": "POST", "path": "/allowed", "sku": "test/allowed"},
	})
	c, _ := passthroughTestContext()

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/not-allowed"},
		{http.MethodGet, "/allowed"}, // 方法不符
		{http.MethodPost, "/allowed/deeper"},
	} {
		_, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
			Account: account, Method: tc.method, Path: tc.path, Header: http.Header{},
		})
		if !errors.Is(err, ErrPassthroughRouteNotAllowed) {
			t.Fatalf("%s %s 应被拒绝，得到 err=%v", tc.method, tc.path, err)
		}
	}
	if hits != 0 {
		t.Fatalf("被拒绝的请求不应触达上游，实际触达 %d 次", hits)
	}
}

// TestPassthroughForwardExtractsUnits 断言按 units_json_path 从响应体提取计费数量。
func TestPassthroughForwardExtractsUnits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"pages_crawled":7}}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{
			"method":          "POST",
			"path":            "/crawl",
			"sku":             "test/crawl",
			"units_json_path": "data.pages_crawled",
		},
	})
	c, rec := passthroughTestContext()

	out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account, Method: http.MethodPost, Path: "/crawl", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("转发失败: %v", err)
	}
	if out.Units != 7 {
		t.Fatalf("计费数量应为 7，得到 %d", out.Units)
	}
	// 提取数量的同时，响应体必须完整回传给客户端。
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应体不是完整 JSON: %v (%q)", err, rec.Body.String())
	}
}

// TestPassthroughForwardRelaysUpstreamErrorVerbatim 断言【不影响账号健康】的上游
// 错误原样立即回传，不改写成网关自己的格式——否则客户端无法按上游文档排错。
//
// 注意与 402/429 的区别：那两类会被暂缓（见
// TestPassthroughForwardDefersQuotaErrorForFailover），因为要留出换账号重试的机会。
// 这里用 400 参数错误，它不指示账号有问题，因此立即回传。
func TestPassthroughForwardRelaysUpstreamErrorVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"InvalidParameter","message":"url error"}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{"method": "POST", "path": "/x", "sku": "test/x"},
	})
	c, rec := passthroughTestContext()

	out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account, Method: http.MethodPost, Path: "/x", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("上游错误不应让 Forward 报错: %v", err)
	}
	if out.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，得到 %d", out.StatusCode)
	}
	if out.Health != nil {
		t.Fatalf("400 不应影响账号健康，得到 %+v", out.Health)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("回写给客户端的状态码应为 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "InvalidParameter") {
		t.Fatalf("上游错误体应原样回传，得到 %q", rec.Body.String())
	}
}

// TestPassthroughForwardDefersQuotaErrorForFailover 断言限流/额度类错误被暂缓：
// 响应【不能】写给客户端，否则一旦写了响应头就再没有换账号重试的机会。
func TestPassthroughForwardDefersQuotaErrorForFailover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"429 限流", http.StatusTooManyRequests, `{"error":"rate limit exceeded"}`},
		{"402 额度不足", http.StatusPaymentRequired, `{"code":"InsufficientBalance"}`},
		{"403 额度耗尽", http.StatusForbidden, `{"error":"insufficient credits"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			account := newPassthroughTestAccount(upstream.URL, []any{
				map[string]any{"method": "POST", "path": "/x", "sku": "test/x"},
			})
			c, rec := passthroughTestContext()
			out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
				Account: account, Method: http.MethodPost, Path: "/x", Header: http.Header{},
			})
			if err != nil {
				t.Fatalf("转发不应报错: %v", err)
			}
			if out.Health == nil {
				t.Fatal("应产出健康判定")
			}
			if !out.Health.Retryable {
				t.Fatal("限流/额度类错误应标记为可重试——请求未被上游执行")
			}
			// 核心断言：响应体尚未写给客户端。
			if rec.Body.Len() != 0 {
				t.Fatalf("响应不应写给客户端（否则无法重试），已写入 %q", rec.Body.String())
			}
			if string(out.DeferredBody) != tc.body {
				t.Fatalf("暂缓的响应体应完整保留: got %q want %q", out.DeferredBody, tc.body)
			}
		})
	}
}

func TestPassthroughForwardRequiresBaseURL(t *testing.T) {
	account := newPassthroughTestAccount("", []any{
		map[string]any{"method": "POST", "path": "/x", "sku": "test/x"},
	})
	c, _ := passthroughTestContext()
	_, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account, Method: http.MethodPost, Path: "/x", Header: http.Header{},
	})
	if !errors.Is(err, ErrPassthroughNoBaseURL) {
		t.Fatalf("缺少 base_url 应报 ErrPassthroughNoBaseURL，得到 %v", err)
	}
}

// TestPassthroughUnitsFromRequestBody 断言异步任务型上游能在【提交时】就按请求体
// 里的计量参数算出用量——这是避免为"提交挂账/完成结算"引入新表与幂等状态机的关键。
func TestPassthroughUnitsFromRequestBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// 异步上游只回 task_id，响应里没有任何用量信息。
		_, _ = w.Write([]byte(`{"output":{"task_id":"t-1","task_status":"PENDING"}}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{
			"method":                  "POST",
			"path":                    "/video",
			"sku":                     "svc/video",
			"units_request_json_path": "parameters.duration",
			"sku_suffix_json_path":    "parameters.resolution",
		},
	})
	c, _ := passthroughTestContext()

	out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account, Method: http.MethodPost, Path: "/video", Header: http.Header{},
		Body: []byte(`{"parameters":{"resolution":"1080P","duration":8}}`),
	})
	if err != nil {
		t.Fatalf("转发失败: %v", err)
	}
	if out.Units != 8 {
		t.Fatalf("用量应取自请求体 parameters.duration=8，得到 %d", out.Units)
	}
	if out.BillingSKU != "svc/video:1080P" {
		t.Fatalf("计费 SKU 应带分辨率档位，得到 %q", out.BillingSKU)
	}
}

// TestPassthroughSKUSuffixFallsBackSafely 断言取不到档位值时退回基础 SKU。
//
// 这一点很重要：若生成一个定价表里不存在的 SKU，计费会静默变成 0——
// 宁可用基础档的价，也不要悄悄不收费。
func TestPassthroughSKUSuffixFallsBackSafely(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, []any{
		map[string]any{
			"method":               "POST",
			"path":                 "/video",
			"sku":                  "svc/video",
			"sku_suffix_json_path": "parameters.resolution",
		},
	})
	for _, body := range []string{`{}`, `{"parameters":{}}`, `{"parameters":{"resolution":"  "}}`, ``} {
		c, _ := passthroughTestContext()
		out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
			Account: account, Method: http.MethodPost, Path: "/video",
			Header: http.Header{}, Body: []byte(body),
		})
		if err != nil {
			t.Fatalf("body=%q 转发失败: %v", body, err)
		}
		if out.BillingSKU != "svc/video" {
			t.Fatalf("body=%q 应退回基础 SKU，得到 %q", body, out.BillingSKU)
		}
		if out.Units != 1 {
			t.Fatalf("body=%q 未配请求体取量时用量应为 1，得到 %d", body, out.Units)
		}
	}
}

func crawlStatusRoutes(settleSKU string) []any {
	return []any{
		map[string]any{
			"method":                 "GET",
			"path":                   "/v2/crawl/*",
			"sku":                    "fc/crawl-status",
			"idempotent":             true,
			"settle_when_json_path":  "status",
			"settle_when_equals":     "completed",
			"settle_units_json_path": "creditsUsed",
			"settle_sku":             settleSKU,
		},
	}
}

// TestPassthroughSettlementOnTerminalState 断言轮询观测到终态时产出结算信息，
// 且幂等键稳定——重复轮询必须得到【同一个】 request_id，否则数据库那条
// (request_id, api_key_id) 唯一索引就拦不住重复计费。
func TestPassthroughSettlementOnTerminalState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"completed","completed":37,"total":37,"creditsUsed":37}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, crawlStatusRoutes("fc/crawl-usage"))

	var seen []string
	for i := 0; i < 3; i++ {
		c, _ := passthroughTestContext()
		out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
			Account: account, Method: http.MethodGet,
			Path: "/v2/crawl/01a00fe1-cfdd-75f8-8b78-d97235779b4a", Header: http.Header{},
		})
		if err != nil {
			t.Fatalf("第 %d 次轮询失败: %v", i+1, err)
		}
		if out.Settlement == nil {
			t.Fatalf("第 %d 次轮询应产出结算信息", i+1)
		}
		if out.Settlement.Units != 37 {
			t.Fatalf("结算用量应为 37，得到 %d", out.Settlement.Units)
		}
		if out.Settlement.SKU != "fc/crawl-usage" {
			t.Fatalf("结算 SKU 应为 fc/crawl-usage，得到 %q", out.Settlement.SKU)
		}
		seen = append(seen, out.Settlement.RequestID)
	}
	// 幂等的全部指望都在这个键上：三次轮询必须给出完全相同的 request_id。
	want := "px-settle:01a00fe1-cfdd-75f8-8b78-d97235779b4a"
	for i, got := range seen {
		if got != want {
			t.Fatalf("第 %d 次的幂等键不稳定: got %q want %q", i+1, got, want)
		}
	}
}

// TestPassthroughSettlementSkippedWhenNotTerminal 断言未达终态、用量为 0 或
// 缺字段时都不产生结算行。
//
// 用量为 0 时必须跳过尤其重要：若此时就落一条零额结算行，幂等键会被提前占掉，
// 等真实用量到达时反而会被去重掉，造成永久少收。
func TestPassthroughSettlementSkippedWhenNotTerminal(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"仍在进行", `{"status":"scraping","creditsUsed":5}`},
		{"用量为 0", `{"status":"completed","creditsUsed":0}`},
		{"缺用量字段", `{"status":"completed"}`},
		{"缺状态字段", `{"creditsUsed":5}`},
		{"状态值不符", `{"status":"failed","creditsUsed":5}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			account := newPassthroughTestAccount(upstream.URL, crawlStatusRoutes("fc/crawl-usage"))
			c, _ := passthroughTestContext()
			out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
				Account: account, Method: http.MethodGet, Path: "/v2/crawl/task-1", Header: http.Header{},
			})
			if err != nil {
				t.Fatalf("转发失败: %v", err)
			}
			if out.Settlement != nil {
				t.Fatalf("不应产生结算行，得到 %+v", out.Settlement)
			}
		})
	}
}

// TestPassthroughSettlementFallsBackToRouteSKU 断言未配 settle_sku 时复用路由 SKU。
func TestPassthroughSettlementFallsBackToRouteSKU(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"completed","creditsUsed":4}`))
	}))
	defer upstream.Close()

	account := newPassthroughTestAccount(upstream.URL, crawlStatusRoutes(""))
	c, _ := passthroughTestContext()
	out, err := newPassthroughTestService().Forward(context.Background(), c, &PassthroughForwardInput{
		Account: account, Method: http.MethodGet, Path: "/v2/crawl/t", Header: http.Header{},
	})
	if err != nil {
		t.Fatalf("转发失败: %v", err)
	}
	if out.Settlement == nil || out.Settlement.SKU != "fc/crawl-status" {
		t.Fatalf("应回退到路由 SKU，得到 %+v", out.Settlement)
	}
}
