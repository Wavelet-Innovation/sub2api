package service

import (
	"net/http"
	"testing"
)

func bailianRoutesExtra() map[string]any {
	return map[string]any{
		"passthrough": map[string]any{
			"routes": []any{
				map[string]any{
					"method": "POST",
					"path":   "/services/aigc/video-generation/video-synthesis",
					"sku":    "bailian/video-gen",
				},
				map[string]any{
					"method":     "GET",
					"path":       "/tasks/*",
					"sku":        "bailian/task-query",
					"idempotent": true,
				},
				map[string]any{
					"method": "POST",
					"path":   "/services/audio/asr/transcription",
					"sku":    "bailian/asr",
				},
			},
		},
	}
}

func TestParsePassthroughRoutes(t *testing.T) {
	routes, err := ParsePassthroughRoutes(bailianRoutesExtra())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(routes) != 3 {
		t.Fatalf("期望 3 条路由，得到 %d", len(routes))
	}
	// 长路径优先，避免短前缀抢先命中。
	if len(routes[0].Path) < len(routes[len(routes)-1].Path) {
		t.Fatalf("路由未按长度降序排列: %+v", routes)
	}
}

// TestParsePassthroughRoutesFailClosed 断言配置缺失或损坏时不会放行任何路径。
func TestParsePassthroughRoutesFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{"extra 为空", nil},
		{"没有 passthrough 段", map[string]any{"other": 1}},
		{"routes 不是数组", map[string]any{"passthrough": map[string]any{"routes": "nope"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes, err := ParsePassthroughRoutes(tc.extra)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if _, _, ok := MatchPassthroughRoute(routes, http.MethodPost, "/anything"); ok {
				t.Fatal("空白名单必须拒绝所有路径")
			}
		})
	}
}

func TestParsePassthroughRoutesRejectsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		route map[string]any
	}{
		{"缺 path", map[string]any{"sku": "x"}},
		{"缺 sku", map[string]any{"path": "/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{"passthrough": map[string]any{"routes": []any{tc.route}}}
			if _, err := ParsePassthroughRoutes(extra); err == nil {
				t.Fatal("期望报错")
			}
		})
	}
}

func TestMatchPassthroughRoute(t *testing.T) {
	routes, err := ParsePassthroughRoutes(bailianRoutesExtra())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	cases := []struct {
		name       string
		method     string
		path       string
		wantOK     bool
		wantSKU    string
		wantSuffix string
	}{
		{"精确命中", "POST", "/services/aigc/video-generation/video-synthesis", true, "bailian/video-gen", ""},
		{"精确命中带首尾斜杠", "POST", "/services/aigc/video-generation/video-synthesis/", true, "bailian/video-gen", ""},
		{"前缀命中并取出后缀", "GET", "/tasks/abc-123", true, "bailian/task-query", "abc-123"},
		{"前缀命中多级后缀", "GET", "/tasks/abc/status", true, "bailian/task-query", "abc/status"},
		{"前缀基准路径本身", "GET", "/tasks", true, "bailian/task-query", ""},
		{"方法不符", "DELETE", "/tasks/abc", false, "", ""},
		{"未在白名单", "POST", "/services/unknown", false, "", ""},
		{"相似但不相等的路径", "POST", "/services/aigc/video-generation", false, "", ""},
		{"前缀不能被部分匹配", "GET", "/tasksomething", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, suffix, ok := MatchPassthroughRoute(routes, tc.method, tc.path)
			if ok != tc.wantOK {
				t.Fatalf("命中与否不符: got %v want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if route.SKU != tc.wantSKU {
				t.Fatalf("SKU 不符: got %q want %q", route.SKU, tc.wantSKU)
			}
			if suffix != tc.wantSuffix {
				t.Fatalf("后缀不符: got %q want %q", suffix, tc.wantSuffix)
			}
		})
	}
}

// TestPassthroughRouteTemplateIsStable 断言用于 usage_logs.inbound_endpoint 的
// 是固定模板而非含任务 ID 的原始路径——后者会撑爆 EndpointStat 的聚合基数，
// 且该列是 VARCHAR(128) 存在截断风险。
func TestPassthroughRouteTemplateIsStable(t *testing.T) {
	routes, _ := ParsePassthroughRoutes(bailianRoutesExtra())
	route, _, ok := MatchPassthroughRoute(routes, "GET", "/tasks/9f8e7d6c-5b4a-3210-fedc-ba9876543210")
	if !ok {
		t.Fatal("应命中前缀路由")
	}
	if got := route.RouteTemplate(); got != "/tasks/*" {
		t.Fatalf("模板应为 /tasks/*，得到 %q", got)
	}
}
