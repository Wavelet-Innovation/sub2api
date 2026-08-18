package routes

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// 通用 REST 透传路由组。
//
// 形如 ANY /px/:service/*upstream_path，其中 :service 是目标账号的 platform
// 字符串（如 bailian / firecrawl / kling），也就是全部的"注册表"——没有新表、
// 没有枚举常量、没有迁移。
//
// 刻意不挂在 /v1 之下：上游持续在 /v1 增加新端点，独立前缀可以永久避免撞车。
// 注意 Cloudflare 边缘规则目前只放行 /v1/*，上线前需要同步放行 /px/*。

// RegisterPassthroughRoutes 注册通用透传路由。
func RegisterPassthroughRoutes(
	r *gin.Engine,
	h *handler.Handlers,
	apiKeyAuth middleware.APIKeyAuthMiddleware,
	settingService *service.SettingService,
	cfg *config.Config,
) {
	group := r.Group("/px")
	group.Use(middleware.RequestBodyLimit(cfg.Gateway.MaxBodySize))
	group.Use(middleware.ClientRequestID())
	group.Use(handler.OpsErrorLoggerMiddleware(nil))
	// 刻意不挂 handler.InboundEndpointMiddleware()：它对未知路径原样返回
	// 请求路径，而透传路径常含任务 ID 等高基数片段，写进 ctx 会流入 ops 统计
	// （usage_logs.inbound_endpoint 是 VARCHAR(128)，既有截断风险又会撑爆
	// EndpointStat 聚合）。计费所需的入站端点由 handler 显式传入路由模板。
	group.Use(gin.HandlerFunc(apiKeyAuth))
	group.Use(forcePlatformFromServiceParam())
	group.Use(middleware.RequireGroupAssignment(settingService, middleware.AnthropicErrorWriter))

	// ANY 覆盖全部方法：具体放行哪些方法由账号白名单里每条路由的 method 决定，
	// 未命中一律本地 404，不接触上游。
	group.Any("/:service/*upstream_path", h.Gateway.Passthrough)
	// 允许 /px/:service（无尾随路径）命中账号白名单里的根路由。
	group.Any("/:service", h.Gateway.Passthrough)
}

// forcePlatformFromServiceParam 把 URL 里的 :service 段作为强制平台注入 ctx。
//
// middleware.ForcePlatform 只接受静态字符串，而透传需要按请求取值，因此这里
// 复制其行为（同时写 request.Context 供 Service 层读取、写 gin.Context 供
// Handler 快速检查），以保证 SelectAccount 的 ForcePlatform 分支能正常命中。
func forcePlatformFromServiceParam() gin.HandlerFunc {
	return func(c *gin.Context) {
		platform := strings.TrimSpace(c.Param("service"))
		if platform == "" {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": gin.H{
					"type":    "not_found_error",
					"message": "Passthrough service is required",
				},
			})
			return
		}
		ctx := c.Request.Context()
		c.Request = c.Request.WithContext(context.WithValue(ctx, ctxkey.ForcePlatform, platform))
		c.Set(string(middleware.ContextKeyForcePlatform), platform)
		c.Next()
	}
}
