package service

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// TestOpenAICodexPassthroughStripFieldsCoversGap 守住两个名单的一致性。
//
// 透传路径剥离 openAIChatGPTInternalUnsupportedFields + 本文件的差集清单；
// transform 路径剥离 openAICodexOAuthUnsupportedFields。两条路径通往同一个上游，
// 因此剥离结果必须一致——否则同一个请求走透传能成、走 transform 失败（或反之），
// 这类差异极难排查。
//
// 上游若给 openAICodexOAuthUnsupportedFields 新增字段而忘了同步这里，本测试会失败。
func TestOpenAICodexPassthroughStripFieldsCoversGap(t *testing.T) {
	covered := make(map[string]bool, len(openAIChatGPTInternalUnsupportedFields)+len(openAICodexPassthroughStripFields))
	for _, f := range openAIChatGPTInternalUnsupportedFields {
		covered[f] = true
	}
	for _, f := range openAICodexPassthroughStripFields {
		covered[f] = true
	}
	for _, f := range openAICodexOAuthUnsupportedFields {
		if !covered[f] {
			t.Fatalf("字段 %q 在 transform 路径会被剥离，但透传路径不会——"+
				"请把它加进 openAICodexPassthroughStripFields", f)
		}
	}
}

// TestStripOpenAICodexPassthroughSamplingParams 断言采样参数被删除，
// 而语义字段原样保留。
func TestStripOpenAICodexPassthroughSamplingParams(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"instructions":"be brief",
		"stream":true,
		"store":false,
		"max_output_tokens":256,
		"max_tokens":512,
		"max_completion_tokens":128,
		"temperature":0.7,
		"top_p":0.9,
		"frequency_penalty":0.1,
		"presence_penalty":0.2,
		"tools":[{"type":"function","name":"f"}]
	}`)

	got, changed, err := stripOpenAICodexPassthroughSamplingParams(body)
	if err != nil {
		t.Fatalf("剥离失败: %v", err)
	}
	if !changed {
		t.Fatal("应报告发生了改动")
	}
	for _, f := range openAICodexPassthroughStripFields {
		if gjson.GetBytes(got, f).Exists() {
			t.Fatalf("字段 %q 应被删除", f)
		}
	}
	// 语义字段必须原样保留——透传的初衷是不改变请求语义。
	for _, keep := range []string{"model", "input", "instructions", "stream", "store", "tools"} {
		if !gjson.GetBytes(got, keep).Exists() {
			t.Fatalf("字段 %q 不应被删除", keep)
		}
	}
	if gjson.GetBytes(got, "instructions").String() != "be brief" {
		t.Fatal("instructions 内容被改动了")
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("剥离后不是合法 JSON: %v", err)
	}
}

// TestStripOpenAICodexPassthroughSamplingParamsNoop 断言不含采样参数时不改动 body。
func TestStripOpenAICodexPassthroughSamplingParamsNoop(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.4","input":"hi"}`,
		`{}`,
		``,
	} {
		got, changed, err := stripOpenAICodexPassthroughSamplingParams([]byte(body))
		if err != nil {
			t.Fatalf("body=%q 报错: %v", body, err)
		}
		if changed {
			t.Fatalf("body=%q 不应报告改动", body)
		}
		if string(got) != body {
			t.Fatalf("body=%q 不应被修改，得到 %q", body, got)
		}
	}
}
