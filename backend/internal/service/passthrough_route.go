package service

import (
	"fmt"
	"sort"
	"strings"
)

// 通用透传的路由定义与匹配。
//
// 路由白名单存放在 accounts.extra.passthrough.routes（自由 JSONB），形如：
//
//	{
//	  "passthrough": {
//	    "routes": [
//	      {"method":"POST","path":"/api/v1/services/aigc/video-generation/video-synthesis",
//	       "sku":"bailian/video-gen"},
//	      {"method":"GET","path":"/api/v1/tasks/*","sku":"bailian/task-query","idempotent":true}
//	    ]
//	  }
//	}
//
// 设计上刻意不用正则：正则容易写出既难审计又可能回溯爆炸的规则，而透传要的
// 是"能一眼看懂放行了什么"。只支持精确匹配与以 /* 结尾的前缀匹配。
//
// sku 会作为 usage_logs.model 落库，因此现有的按模型统计、渠道定价、后台筛选
// 全部免费复用——这是整个透传层不需要动计费代码的关键。

const (
	passthroughExtraKey  = "passthrough"
	passthroughRoutesKey = "routes"
	passthroughWildcard  = "/*"
)

// PassthroughRoute 是一条被放行的上游路径规则。
type PassthroughRoute struct {
	// Method 是允许的 HTTP 方法（大写）。为空表示不限方法。
	Method string
	// Path 是上游路径。以 /* 结尾表示前缀匹配，其余为精确匹配。
	Path string
	// SKU 是计费单元标识，写入 usage_logs.model。
	SKU string
	// Idempotent 标记该路由可安全重试。默认 false：透传的 POST 多为
	// 非幂等的任务提交（如提交一次视频生成即产生一次扣费），跨账号重试
	// 会造成重复计费，因此必须显式开启。
	Idempotent bool
	// UnitsJSONPath 可选，从上游【响应体】里取计费数量的 gjson 路径。
	// 为空时按单次计费（数量 1）。
	UnitsJSONPath string
	// UnitsRequestJSONPath 可选，从【请求体】里取计费数量的 gjson 路径。
	//
	// 存在的意义：异步任务型上游（视频生成等）在提交时只返回 task_id，真实用量
	// 要等任务完成才知道。但计量参数往往在提交请求里就给全了——例如百炼视频的
	// parameters.duration 就是要生成的秒数。直接从请求体取，可以避免为"提交时
	// 挂账、完成时结算"引入新表与幂等状态机。
	//
	// 已知偏差：任务若最终失败，这笔已计的费用不会退。上游通常也不为失败任务
	// 计费，因此这里会略微高估。要完全精确需要在轮询到终态时补结算。
	//
	// 优先级高于 UnitsJSONPath——请求体在转发前就可得，无需缓冲响应。
	UnitsRequestJSONPath string
	// SKUSuffixJSONPath 可选，从【请求体】取一个值追加到 SKU 后面（以 : 分隔）。
	//
	// 用于按档位计价：视频按分辨率分档（480P/720P/1080P 单价差 4 倍），而路由是
	// 静态的、无法按请求体变化。把分辨率并进 SKU（如 bailian/video-gen:1080P）
	// 就能沿用现有的按模型定价，不必扩展计费引擎。
	//
	// ⚠️ 每个可能出现的档位都必须在 channel_model_pricing 里有对应行，
	// 否则该档位查不到定价、按 0 计费。
	SKUSuffixJSONPath string

	// ---- 异步任务终态结算 ----
	//
	// 场景：爬取类任务的真实用量既不在提交请求里（页数取决于目标站点结构，
	// 请求体的 limit 只是上限），也不在提交响应里（只回一个任务 id）。只有轮询
	// 到终态时上游才报出实际消耗。
	//
	// 做法：在【轮询路由】上配置以下四项。当轮询响应命中终态条件时，除了本次
	// 轮询自身的计费行，再额外落一条结算行。
	//
	// 幂等靠数据库：结算行的 request_id 固定为 "px-settle:<资源id>"，而
	// usage_logs 上有 (request_id, api_key_id) 唯一索引 + ON CONFLICT DO NOTHING，
	// 因此重复轮询产生的结算行会被静默去重——不需要额外的状态表或状态机。

	// SettleWhenJSONPath 是响应体里表示任务状态的 gjson 路径（如 "status"）。
	SettleWhenJSONPath string
	// SettleWhenEquals 是触发结算的状态值（如 "completed"）。大小写不敏感。
	SettleWhenEquals string
	// SettleUnitsJSONPath 是响应体里真实用量的 gjson 路径（如 "creditsUsed"）。
	SettleUnitsJSONPath string
	// SettleSKU 是结算行使用的计费单元。为空时复用本路由的 SKU——但通常应配
	// 一个独立 SKU（如 firecrawl/crawl-usage），否则结算金额会和轮询本身的
	// 计费混在同一个 SKU 下、无法分辨。
	SettleSKU string
	// SettleIDJSONPath 是响应体里资源 id 的 gjson 路径，用于构造幂等键。
	// 为空时退回使用客户端请求路径的最后一段（前缀路由下即任务 id）。
	SettleIDJSONPath string
}

// HasSettlement 报告该路由是否配置了终态结算。
func (r PassthroughRoute) HasSettlement() bool {
	return r.SettleWhenJSONPath != "" && r.SettleWhenEquals != "" && r.SettleUnitsJSONPath != ""
}

// SettlementSKU 返回结算行应使用的 SKU。
func (r PassthroughRoute) SettlementSKU() string {
	if r.SettleSKU != "" {
		return r.SettleSKU
	}
	return r.SKU
}

// IsPrefix 报告该路由是否为前缀匹配。
func (r PassthroughRoute) IsPrefix() bool {
	return strings.HasSuffix(r.Path, passthroughWildcard)
}

// prefixBase 返回前缀路由去掉 /* 之后的基准路径。
func (r PassthroughRoute) prefixBase() string {
	return strings.TrimSuffix(r.Path, passthroughWildcard)
}

// RouteTemplate 返回用于 usage_logs.inbound_endpoint 的稳定模板。
//
// 必须使用模板而非原始路径：原始路径可能含任务 ID 等高基数片段，直接落库会
// 撑爆 EndpointStat 的聚合基数，且该列是 VARCHAR(128) 有截断风险。
func (r PassthroughRoute) RouteTemplate() string {
	return r.Path
}

// ParsePassthroughRoutes 从账号 extra 中解析路由白名单。
//
// 解析失败或未配置时返回空切片——调用方据此拒绝所有请求（默认拒绝）。
func ParsePassthroughRoutes(extra map[string]any) ([]PassthroughRoute, error) {
	if len(extra) == 0 {
		return nil, nil
	}
	section, ok := extra[passthroughExtraKey].(map[string]any)
	if !ok {
		return nil, nil
	}
	rawRoutes, ok := section[passthroughRoutesKey].([]any)
	if !ok {
		return nil, nil
	}

	routes := make([]PassthroughRoute, 0, len(rawRoutes))
	for i, raw := range rawRoutes {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("passthrough: route[%d] is not an object", i)
		}
		path, _ := item["path"].(string)
		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("passthrough: route[%d] missing path", i)
		}
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		sku, _ := item["sku"].(string)
		sku = strings.TrimSpace(sku)
		if sku == "" {
			return nil, fmt.Errorf("passthrough: route[%d] (%s) missing sku", i, path)
		}
		method, _ := item["method"].(string)
		idempotent, _ := item["idempotent"].(bool)
		unitsPath, _ := item["units_json_path"].(string)
		unitsReqPath, _ := item["units_request_json_path"].(string)
		skuSuffixPath, _ := item["sku_suffix_json_path"].(string)
		settleWhenPath, _ := item["settle_when_json_path"].(string)
		settleWhenEquals, _ := item["settle_when_equals"].(string)
		settleUnitsPath, _ := item["settle_units_json_path"].(string)
		settleSKU, _ := item["settle_sku"].(string)
		settleIDPath, _ := item["settle_id_json_path"].(string)

		routes = append(routes, PassthroughRoute{
			Method:               strings.ToUpper(strings.TrimSpace(method)),
			Path:                 path,
			SKU:                  sku,
			Idempotent:           idempotent,
			UnitsJSONPath:        strings.TrimSpace(unitsPath),
			UnitsRequestJSONPath: strings.TrimSpace(unitsReqPath),
			SKUSuffixJSONPath:    strings.TrimSpace(skuSuffixPath),
			SettleWhenJSONPath:   strings.TrimSpace(settleWhenPath),
			SettleWhenEquals:     strings.TrimSpace(settleWhenEquals),
			SettleUnitsJSONPath:  strings.TrimSpace(settleUnitsPath),
			SettleSKU:            strings.TrimSpace(settleSKU),
			SettleIDJSONPath:     strings.TrimSpace(settleIDPath),
		})
	}

	// 长路径优先，确保 /a/b 这样的精确规则不会被 /a/* 抢先命中。
	sort.SliceStable(routes, func(i, j int) bool {
		li, lj := len(routes[i].Path), len(routes[j].Path)
		if li != lj {
			return li > lj
		}
		// 同长度时精确规则优先于前缀规则。
		return !routes[i].IsPrefix() && routes[j].IsPrefix()
	})
	return routes, nil
}

// MatchPassthroughRoute 在白名单中查找匹配的路由。
//
// 返回命中的路由、客户端提供的剩余片段（仅前缀路由非空）以及是否命中。
// 未命中时调用方应返回 404 且不接触上游——这一点是把"没权限的模型/路径"
// 挡在本地的关键：一旦请求打到上游拿回 4xx，某些上游（如智谱的 403）会被
// 识别成账号级故障，进而把整个账号熔断。
func MatchPassthroughRoute(routes []PassthroughRoute, method, path string) (PassthroughRoute, string, bool) {
	normalizedMethod := strings.ToUpper(strings.TrimSpace(method))
	normalizedPath := "/" + strings.Trim(strings.TrimSpace(path), "/")

	for _, route := range routes {
		if route.Method != "" && route.Method != normalizedMethod {
			continue
		}
		if route.IsPrefix() {
			base := strings.TrimRight(route.prefixBase(), "/")
			if normalizedPath == base {
				return route, "", true
			}
			if strings.HasPrefix(normalizedPath, base+"/") {
				return route, strings.TrimPrefix(normalizedPath, base+"/"), true
			}
			continue
		}
		if normalizedPath == strings.TrimRight(route.Path, "/") {
			return route, "", true
		}
	}
	return PassthroughRoute{}, "", false
}
