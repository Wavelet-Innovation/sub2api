package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testImageURL = "https://example.com/cat.png"

func anthropicURLImageRequest(content string) *AnthropicRequest {
	return &AnthropicRequest{
		Model:     "gpt-6-sol",
		MaxTokens: 1024,
		Messages:  []AnthropicMessage{{Role: "user", Content: json.RawMessage(content)}},
	}
}

// Anthropic URL 图片（source.type=url）转 Responses 时必须保留，而不是被静默丢弃。
func TestAnthropicToResponses_URLImageBlock(t *testing.T) {
	req := anthropicURLImageRequest(`[
		{"type":"text","text":"What is in this image?"},
		{"type":"image","source":{"type":"url","url":"` + testImageURL + `"}}
	]`)

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)

	var items []ResponsesInputItem
	require.NoError(t, json.Unmarshal(resp.Input, &items))
	require.Len(t, items, 1)
	var parts []ResponsesContentPart
	require.NoError(t, json.Unmarshal(items[0].Content, &parts))
	require.Len(t, parts, 2)
	assert.Equal(t, "input_image", parts[1].Type)
	assert.Equal(t, testImageURL, parts[1].ImageURL)
}

func TestAnthropicToResponses_URLImageInToolResult(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "gpt-6-sol",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"look"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"view","input":{}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"text","text":"loaded"},
				{"type":"image","source":{"type":"url","url":"` + testImageURL + `"}}
			]}]`)},
		},
	}

	resp, err := AnthropicToResponses(req)
	require.NoError(t, err)
	assert.Contains(t, string(resp.Input), testImageURL)
}

func TestAnthropicToChatCompletions_URLImageBlock(t *testing.T) {
	req := anthropicURLImageRequest(`[
		{"type":"text","text":"What is in this image?"},
		{"type":"image","source":{"type":"url","url":"` + testImageURL + `"}}
	]`)

	resp, err := AnthropicToChatCompletionsRequest(req)
	require.NoError(t, err)

	body, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"image_url":{"url":"`+testImageURL+`"`)
}

// 非 http(s) 的 URL 不是合法的图片来源，仍然丢弃。
func TestAnthropicImageSourceToURL_RejectsNonHTTP(t *testing.T) {
	assert.Equal(t, "", anthropicImageSourceToURL(&AnthropicImageSource{Type: "url", URL: "file:///etc/passwd"}))
	assert.Equal(t, "", anthropicImageSourceToURL(&AnthropicImageSource{Type: "url"}))
	assert.Equal(t, "", anthropicImageSourceToURL(nil))
	assert.Equal(t, "data:image/jpeg;base64,QQ==",
		anthropicImageSourceToURL(&AnthropicImageSource{Type: "base64", MediaType: "image/jpeg", Data: "QQ=="}))
}

// 反方向：OpenAI 客户端的 http(s) 图片 URL 转 Anthropic 时变成 url 来源。
func TestResponsesToAnthropic_URLImage(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"role":"user","content":[
			{"type":"input_text","text":"What is in this image?"},
			{"type":"input_image","image_url":"`+testImageURL+`"}
		]}
	]`)

	require.Len(t, msgs, 1)
	blocks := parseContentBlocks(msgs[0].Content)
	require.Len(t, blocks, 2)
	require.Equal(t, "image", blocks[1].Type)
	require.NotNil(t, blocks[1].Source)
	assert.Equal(t, "url", blocks[1].Source.Type)
	assert.Equal(t, testImageURL, blocks[1].Source.URL)

	// Anthropic 拒绝 url 来源携带 data/media_type 字段。
	raw, err := json.Marshal(blocks[1].Source)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"url","url":"`+testImageURL+`"}`, string(raw))
}

func TestResponsesToAnthropic_URLImageInFunctionOutput(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"function_call","call_id":"call_A","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[
			{"type":"input_image","image_url":"`+testImageURL+`"}
		]}
	]`)

	require.Len(t, msgs, 2)
	resultBlocks := parseContentBlocks(msgs[1].Content)
	require.Len(t, resultBlocks, 1)
	var content []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(resultBlocks[0].Content, &content))
	require.Len(t, content, 1)
	require.NotNil(t, content[0].Source)
	assert.Equal(t, "url", content[0].Source.Type)
	assert.Equal(t, testImageURL, content[0].Source.URL)
}

// base64 图片的 JSON 形状保持不变。
func TestAnthropicImageSource_Base64JSONUnchanged(t *testing.T) {
	raw, err := json.Marshal(&AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: "YQ=="})
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"base64","media_type":"image/png","data":"YQ=="}`, string(raw))
}

// Chat Completions 客户端 → Claude 组：chat → Responses → Anthropic 两跳后仍保留 URL。
func TestChatCompletionsToAnthropic_URLImageEndToEnd(t *testing.T) {
	req := &ChatCompletionsRequest{
		Model: "claude-opus-5-5",
		Messages: []ChatMessage{{Role: "user", Content: json.RawMessage(
			`[{"type":"text","text":"Describe this"},{"type":"image_url","image_url":{"url":"` + testImageURL + `"}}]`)}},
	}
	responsesReq, err := ChatCompletionsToResponses(req)
	require.NoError(t, err)
	anthropicReq, err := ResponsesToAnthropicRequest(responsesReq)
	require.NoError(t, err)

	require.Len(t, anthropicReq.Messages, 1)
	blocks := parseContentBlocks(anthropicReq.Messages[0].Content)
	require.Len(t, blocks, 2)
	require.NotNil(t, blocks[1].Source)
	assert.Equal(t, "url", blocks[1].Source.Type)
	assert.Equal(t, testImageURL, blocks[1].Source.URL)
}
