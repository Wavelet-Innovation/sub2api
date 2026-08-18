package service

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex 透传路径的采样参数剥离。
//
// 背景：OAuth 账号有两条通往 ChatGPT internal Codex 端点的路径——
//
//	transform 路径：Forward 里做完整的 Codex 适配，剥离
//	               openAICodexOAuthUnsupportedFields（含采样参数）
//	passthrough 路径：账号开了 openai_passthrough 时走这里，语义交给上游，
//	               仅剥离 openAIChatGPTInternalUnsupportedFields（元数据类）
//
// 两个名单的差集正是 6 个采样参数。Codex CLI 从不发它们，所以长期没暴露问题；
// 但标准 OpenAI 客户端（Harness、各类 SDK）默认会带 max_output_tokens，上游据此
// 返回 400 {"detail":"Unsupported parameter: max_output_tokens"}。
//
// 这里把差集补上：透传模式保持"不改语义"的初衷（不补 instructions、不改 input
// 结构），只删除上游明确拒绝、且删除后不改变语义的采样参数——它们对 Codex 端点
// 本来就无效，带不带结果一样。
//
// 仅作用于 OAuth 账号：API Key 透传账号（Kiro 等）的上游是标准 Responses 实现，
// 这些参数是支持的，不能一并剥离。

// openAICodexPassthroughStripFields 是 openAICodexOAuthUnsupportedFields 相对
// openAIChatGPTInternalUnsupportedFields 的差集，即透传路径尚未覆盖的部分。
//
// 刻意写成显式列表而非运行时求差集：求差集会让"透传剥什么"隐式依赖另外两个
// 变量的演化，而这里需要的是一个可审计、可测试的明确清单。变更时两处一起改，
// TestOpenAICodexPassthroughStripFieldsCoversGap 会守住一致性。
var openAICodexPassthroughStripFields = []string{
	"max_output_tokens",
	// max_tokens 是 Chat Completions 的写法。上游的两个名单里都没有它——
	// 实测 ChatGPT internal Codex 端点同样返回
	// 400 {"detail":"Unsupported parameter: max_tokens"}。
	// 标准 OpenAI 客户端在 Responses 与 Chat Completions 之间切换时经常带上它，
	// 所以这里一并剥离。
	"max_tokens",
	"max_completion_tokens",
	"temperature",
	"top_p",
	"frequency_penalty",
	"presence_penalty",
}

// stripOpenAICodexPassthroughSamplingParams 从透传请求体中删除上游拒绝的采样参数。
//
// 返回处理后的 body 与是否发生改动。解析失败时原样返回，绝不因为剥离逻辑本身
// 让一个原本能成功的请求失败。
func stripOpenAICodexPassthroughSamplingParams(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	stripped := body
	changed := false
	for _, field := range openAICodexPassthroughStripFields {
		if !gjson.GetBytes(stripped, field).Exists() {
			continue
		}
		next, err := sjson.DeleteBytes(stripped, field)
		if err != nil {
			// 保守处理：剥离失败就整体放弃，返回原始 body。
			// 让请求带着这个参数去撞上游的 400，好过在这里把 body 改坏。
			return body, false, fmt.Errorf("strip passthrough sampling param %s: %w", field, err)
		}
		stripped = next
		changed = true
	}
	return stripped, changed, nil
}
