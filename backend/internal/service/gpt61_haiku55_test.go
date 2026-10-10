package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// ---------- GPT-6.1 Sol ----------

func TestGPT61Sol_Normalization(t *testing.T) {
	for _, model := range []string{"gpt-6.1-sol", "openai/gpt-6.1-sol", "GPT-6.1-Sol", "gpt6.1-sol", "gpt-6.1-sol-2026-09-29"} {
		t.Run(model, func(t *testing.T) {
			assert.Equal(t, "gpt-6.1-sol", normalizeKnownOpenAICodexModel(model))
		})
	}
	// Codex OAuth 转发不能把它改写成 gpt-5.4 或 gpt-6-sol
	assert.Equal(t, "gpt-6.1-sol", normalizeCodexModel("gpt-6.1-sol"))
	assert.True(t, isOpenAIGPT6Model("gpt-6.1-sol"))
	assert.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "gpt-6.1-sol"))
	// 未知 GPT-6.1 型号不兜底
	assert.Equal(t, "", normalizeKnownOpenAICodexModel("gpt-6.1-astra"))
}

func TestGPT61Sol_ModelListKeepsGPT56SolFirst(t *testing.T) {
	require.Contains(t, openai.DefaultModelIDs(), "gpt-6.1-sol")
	require.Equal(t, "gpt-5.6-sol", openai.DefaultModels[0].ID)
}

// GPT-6.1 Sol 官方价格：$2 / $10，缓存读 $0.10，缓存写 $2.50，Fast 为 2 倍。
func TestGPT61Sol_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	pricing, err := svc.GetModelPricing("gpt-6.1-sol")
	require.NoError(t, err)
	assert.InDelta(t, 2e-6, pricing.InputPricePerToken, 1e-15)
	assert.InDelta(t, 10e-6, pricing.OutputPricePerToken, 1e-15)
	assert.InDelta(t, 0.1e-6, pricing.CacheReadPricePerToken, 1e-15)
	assert.InDelta(t, 2.5e-6, pricing.CacheCreationPricePerToken, 1e-15)
	assert.InDelta(t, 4e-6, pricing.InputPricePerTokenPriority, 1e-15)
	assert.Equal(t, openAIGPT54LongContextInputThreshold, pricing.LongContextInputThreshold)
}

func TestGPT61Sol_PricingServiceStaticFallback(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-6-sol": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: 0.2e-6},
		},
	})
	pricing, err := svc.GetModelPricing("gpt-6.1-sol")
	require.NoError(t, err)
	// 缓存读价与 GPT-6 Sol 不同（$0.10 vs $0.20），不能借用 gpt-6-sol 条目
	assert.InDelta(t, 0.1e-6, pricing.CacheReadPricePerToken, 1e-15)
}

// ---------- Ultrafast service tier ----------

func TestUltrafastServiceTier_Recognized(t *testing.T) {
	require.Equal(t, "ultrafast", normalizedOpenAIServiceTierValue("ultrafast"))
	require.Equal(t, "ultrafast", normalizedOpenAIServiceTierValue(" UltraFast "))
	assert.Equal(t, 6.0, serviceTierCostMultiplier("ultrafast"))
}

// 默认策略：GPT 模型的 ultrafast 与 fast 一样被剥离，否则请求会以 6 倍价格绕过"关闭 fast"。
func TestUltrafastServiceTier_DefaultPolicyStripsForGPT(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	for _, model := range []string{"gpt-6.1-sol", "gpt-6-astra"} {
		action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, model, OpenAIFastTierUltrafast)
		assert.Equal(t, BetaPolicyActionFilter, action, model)

		body := []byte(`{"model":"` + model + `","service_tier":"ultrafast"}`)
		updated, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, model, body)
		require.NoError(t, err)
		assert.False(t, gjson.GetBytes(updated, "service_tier").Exists(), model)
	}

	action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, "o3", OpenAIFastTierUltrafast)
	assert.Equal(t, BetaPolicyActionPass, action)
}

// ---------- GPT-6 Fast 价格修正：2 倍而非 4 倍 ----------

func TestGPT6FastPricingIsDoubleStandard(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"} {
		pricing, err := svc.GetModelPricing(model)
		require.NoError(t, err)
		assert.InDelta(t, pricing.InputPricePerToken*2, pricing.InputPricePerTokenPriority, 1e-15, model)
		assert.InDelta(t, pricing.OutputPricePerToken*2, pricing.OutputPricePerTokenPriority, 1e-15, model)
		assert.InDelta(t, pricing.CacheReadPricePerToken*2, pricing.CacheReadPricePerTokenPriority, 1e-15, model)
		assert.InDelta(t, pricing.CacheCreationPricePerToken*2, pricing.CacheCreationPricePerTokenPriority, 1e-15, model)
	}
	for _, p := range []*LiteLLMModelPricing{openAIGPT6AstraFallbackPricing, openAIGPT6SolFallbackPricing, openAIGPT6LunaFallbackPricing} {
		assert.InDelta(t, p.InputCostPerToken*2, p.InputCostPerTokenPriority, 1e-15)
		assert.InDelta(t, p.OutputCostPerToken*2, p.OutputCostPerTokenPriority, 1e-15)
	}
}

// ---------- Claude Haiku 5.5 ----------

func TestClaudeHaiku55_ModelListsAndBedrock(t *testing.T) {
	found := false
	for _, m := range claude.DefaultModels {
		if m.ID == "claude-haiku-5-5" {
			found = true
		}
	}
	assert.True(t, found)
	assert.Equal(t, "anthropic.claude-haiku-5-5", domain.DefaultBedrockModelMapping["claude-haiku-5-5"])
	// Antigravity 无 Haiku，与 Haiku 4.5 一样映射到 Sonnet 4.6
	assert.Equal(t, "claude-sonnet-4-6", domain.DefaultAntigravityModelMapping["claude-haiku-5-5"])
}

// Haiku 5.5 只接受 adaptive thinking；Bedrock 上 enabled + budget_tokens 返回 400。
func TestClaudeHaiku55_BedrockThinkingBecomesAdaptive(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":8000}}`)
	out := sanitizeBedrockThinking(body, "anthropic.claude-haiku-5-5")
	assert.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	assert.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())

	// Haiku 4.5 仍支持 enabled + budget_tokens
	out = sanitizeBedrockThinking(body, "us.anthropic.claude-haiku-4-5-20251001-v1:0")
	assert.Equal(t, "enabled", gjson.GetBytes(out, "thinking.type").String())
}

const haiku55Threshold = 100000

func TestClaudeHaiku55_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	pricing, err := svc.GetModelPricing("claude-haiku-5-5")
	require.NoError(t, err)
	assert.InDelta(t, 0.1e-6, pricing.InputPricePerToken, 1e-15)
	assert.InDelta(t, 0.5e-6, pricing.OutputPricePerToken, 1e-15)
	assert.InDelta(t, 0.125e-6, pricing.CacheCreation5mPrice, 1e-15)
	assert.InDelta(t, 0.2e-6, pricing.CacheCreation1hPrice, 1e-15)
	assert.InDelta(t, 0.01e-6, pricing.CacheReadPricePerToken, 1e-15)
	assert.Equal(t, haiku55Threshold, pricing.LongContextInputThreshold)

	// 不能落到 Haiku 3 / 3.5 的兜底价
	for _, model := range []string{"anthropic.claude-haiku-5-5", "us.anthropic.claude-haiku-5-5"} {
		p, err := svc.GetModelPricing(model)
		require.NoError(t, err)
		assert.InDelta(t, 0.1e-6, p.InputPricePerToken, 1e-15, model)
	}
}

// 线上价格表（LiteLLM）里 Haiku 5.5 只有基础价，分层价写在 *_above_100k_tokens 字段里，
// 解析器不读这些字段。超过 100K 的提示必须按 5 倍计费，而不是基础价。
func TestClaudeHaiku55_LongPromptBilledAtFiveTimes(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"claude-haiku-5-5": {
				InputCostPerToken:                   0.1e-6,
				OutputCostPerToken:                  0.5e-6,
				CacheCreationInputTokenCost:         0.125e-6,
				CacheCreationInputTokenCostAbove1hr: 0.2e-6,
				CacheReadInputTokenCost:             0.01e-6,
			},
		},
	})

	short, err := svc.CalculateCost("claude-haiku-5-5", UsageTokens{InputTokens: 50000, OutputTokens: 1000}, 1)
	require.NoError(t, err)
	assert.InDelta(t, 50000*0.1e-6+1000*0.5e-6, short.TotalCost, 1e-12)

	long, err := svc.CalculateCost("claude-haiku-5-5", UsageTokens{
		InputTokens:           150000,
		OutputTokens:          1000,
		CacheReadTokens:       10000,
		CacheCreationTokens:   20000,
		CacheCreation1hTokens: 20000,
	}, 1)
	require.NoError(t, err)
	want := 150000*0.5e-6 + // 输入 $0.50
		1000*2.5e-6 + // 输出 $2.50
		10000*0.05e-6 + // 缓存读 $0.05
		20000*1e-6 // 1h 缓存写 $1
	assert.InDelta(t, want, long.TotalCost, 1e-12)
}
