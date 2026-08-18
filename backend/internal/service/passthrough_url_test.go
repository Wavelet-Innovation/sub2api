package service

import "testing"

const passthroughTestBase = "https://dashscope.aliyuncs.com/api/v1"

// TestBuildPassthroughURLRejectsHostile 覆盖计划中列为"必须逐条过"的攻击构造。
// 每一条都对应一种把网关变成 SSRF 跳板、并把账号里的真实上游凭证转发给
// 攻击者主机的具体手法。
func TestBuildPassthroughURLRejectsHostile(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
	}{
		{"父目录穿越", "../../v1/chat/completions"},
		{"单点段", "./secret"},
		{"中间穿越", "tasks/../../admin"},
		{"百分号编码穿越", "%2e%2e/%2e%2e/admin"},
		{"双重编码穿越", "%252e%252e/admin"},
		{"大写编码穿越", "%2E%2E/admin"},
		{"绝对 URL 改写主机", "http://169.254.169.254/latest/meta-data/"},
		{"https 绝对 URL", "https://evil.example.com/steal"},
		{"协议相对地址", "//169.254.169.254/latest/meta-data/"},
		{"反斜杠穿越", "..\\..\\admin"},
		{"反斜杠分隔", "tasks\\..\\admin"},
		{"换行请求走私", "tasks\r\nHost: evil.com"},
		{"空字节", "tasks\x00/admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildPassthroughURL(passthroughTestBase, "/tasks/*", tc.suffix, "")
			if err == nil {
				t.Fatalf("期望拒绝 %q，却返回了 %q", tc.suffix, got)
			}
		})
	}
}

// TestBuildPassthroughURLStaysUnderBase 断言拼接结果永远落在 base 之下，
// 且 scheme/host 与 base 完全一致。
func TestBuildPassthroughURLStaysUnderBase(t *testing.T) {
	cases := []struct {
		name         string
		base         string
		upstreamPath string
		suffix       string
		rawQuery     string
		want         string
	}{
		{
			name:         "精确路径",
			base:         passthroughTestBase,
			upstreamPath: "/services/aigc/video-generation/video-synthesis",
			want:         "https://dashscope.aliyuncs.com/api/v1/services/aigc/video-generation/video-synthesis",
		},
		{
			name:         "前缀路径带客户端后缀",
			base:         passthroughTestBase,
			upstreamPath: "/tasks",
			suffix:       "abc-123",
			want:         "https://dashscope.aliyuncs.com/api/v1/tasks/abc-123",
		},
		{
			name:         "base 尾部斜杠被归一",
			base:         passthroughTestBase + "/",
			upstreamPath: "/tasks",
			suffix:       "id",
			want:         "https://dashscope.aliyuncs.com/api/v1/tasks/id",
		},
		{
			// 内部与尾部的重复斜杠归一化；但开头的 // 不在此列——那是协议
			// 相对地址的写法，由 TestBuildPassthroughURLRejectsHostile
			// 的"协议相对地址"用例断言必须拒绝，不能为了容错而放宽。
			name:         "内部重复斜杠被压缩",
			base:         passthroughTestBase,
			upstreamPath: "/tasks//sub/",
			suffix:       "id//",
			want:         "https://dashscope.aliyuncs.com/api/v1/tasks/sub/id",
		},
		{
			name:         "保留查询串",
			base:         passthroughTestBase,
			upstreamPath: "/tasks",
			rawQuery:     "limit=10&order=desc",
			want:         "https://dashscope.aliyuncs.com/api/v1/tasks?limit=10&order=desc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildPassthroughURL(tc.base, tc.upstreamPath, tc.suffix, tc.rawQuery)
			if err != nil {
				t.Fatalf("未预期的错误: %v", err)
			}
			if got != tc.want {
				t.Fatalf("拼接结果不符\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestBuildPassthroughURLRejectsBadBase(t *testing.T) {
	for _, base := range []string{"", "   ", "not-a-url", "ftp://example.com", "file:///etc/passwd"} {
		if _, err := buildPassthroughURL(base, "/tasks", "", ""); err == nil {
			t.Fatalf("期望拒绝 base %q", base)
		}
	}
}
