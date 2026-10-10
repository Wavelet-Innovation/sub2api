package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeKnownOpenAICodexModelGPT6(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"gpt-6-astra", "gpt-6-astra"},
		{"openai/gpt-6-sol", "gpt-6-sol"},
		{"GPT-6-Luna", "gpt-6-luna"},
		{"gpt6-astra", "gpt-6-astra"},
		{"gpt-6-sol-2026-09-01", "gpt-6-sol"},
		// 未知的 GPT-6 型号不能被兜底成 gpt-5.4
		{"gpt-6-unknown", ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			require.Equal(t, tt.want, normalizeKnownOpenAICodexModel(tt.model))
		})
	}
}

// 修复前 Codex OAuth 转发会把未识别的 GPT-6 型号改写成 gpt-5.4。
func TestNormalizeCodexModelKeepsGPT6(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna"} {
		t.Run(model, func(t *testing.T) {
			require.Equal(t, model, normalizeCodexModel(model))
		})
	}
}

func TestIsOpenAIGPT6Model(t *testing.T) {
	require.True(t, isOpenAIGPT6Model("gpt-6-astra"))
	require.True(t, isOpenAIGPT6Model("openai/gpt-6-luna-2026-09-01"))
	require.False(t, isOpenAIGPT6Model("gpt-5.6-sol"))
	require.False(t, isOpenAIGPT6Model("gpt-6"))
	require.False(t, isOpenAIGPT6Model("gpt-6-astral"))
}

func TestNormalizeOpenAIReasoningEffortForGPT6(t *testing.T) {
	require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "gpt-6-astra"))
	require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "openai/gpt-6-sol"))
	require.Equal(t, "max", normalizeOpenAIReasoningEffortForModel("max", "gpt-6-luna"))
}

func TestNormalizeOpenAICodexCompactReasoningEffortDowngradesGPT6Max(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","input":"compact me","reasoning":{"effort":"max"}}`)

	normalized, changed, err := normalizeOpenAICodexCompactReasoningEffort(body, "gpt-6-astra")

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "xhigh", gjson.GetBytes(normalized, "reasoning.effort").String())
}

// GPT-6 官方价格（USD per token）。
func TestGPT6HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)

	tests := []struct {
		model      string
		input      float64
		output     float64
		cacheRead  float64
		cacheWrite float64
	}{
		{"gpt-6-astra", 10e-6, 50e-6, 1e-6, 12.5e-6},
		{"gpt-6-sol", 2e-6, 10e-6, 0.2e-6, 2.5e-6},
		{"gpt-6-luna", 0.1e-6, 0.5e-6, 0.01e-6, 0.125e-6},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			require.NotNil(t, pricing)
			assert.InDelta(t, tt.input, pricing.InputPricePerToken, 1e-15)
			assert.InDelta(t, tt.output, pricing.OutputPricePerToken, 1e-15)
			assert.InDelta(t, tt.cacheRead, pricing.CacheReadPricePerToken, 1e-15)
			assert.InDelta(t, tt.cacheWrite, pricing.CacheCreationPricePerToken, 1e-15)
			assert.InDelta(t, tt.input*2, pricing.InputPricePerTokenPriority, 1e-15)
			assert.Equal(t, openAIGPT54LongContextInputThreshold, pricing.LongContextInputThreshold)
		})
	}
}

// 远端价格表尚未收录 GPT-6 时，应走静态兜底，而不是回退到 GPT-5.x 价格。
func TestGPT6PricingServiceStaticFallback(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-5.4": {InputCostPerToken: 2.5e-6, OutputCostPerToken: 15e-6},
		},
	})

	pricing, err := svc.GetModelPricing("gpt-6-sol")
	require.NoError(t, err)
	require.NotNil(t, pricing)
	assert.InDelta(t, 2e-6, pricing.InputPricePerToken, 1e-15)
	assert.InDelta(t, 10e-6, pricing.OutputPricePerToken, 1e-15)
}

// 价格表只给了输入价时，GPT-6 与 GPT-5.6 一样按输入价 1.25 倍补齐缓存写入价，
// 并补齐 272K 长上下文规则。
func TestGPT6ModelSpecificPricingPolicy(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)

	pricing := svc.applyModelSpecificPricingPolicy("gpt-6-astra", &ModelPricing{
		InputPricePerToken:  10e-6,
		OutputPricePerToken: 50e-6,
	})

	assert.InDelta(t, 12.5e-6, pricing.CacheCreationPricePerToken, 1e-15)
	assert.Equal(t, openAIGPT54LongContextInputThreshold, pricing.LongContextInputThreshold)
}
