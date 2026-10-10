package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClaudeSonnet55_ModelListAndBedrock(t *testing.T) {
	found := false
	for _, m := range claude.DefaultModels {
		if m.ID == "claude-sonnet-5-5" {
			found = true
			assert.Equal(t, "Claude Sonnet 5.5", m.DisplayName)
		}
	}
	assert.True(t, found)
	assert.Equal(t, "anthropic.claude-sonnet-5-5", domain.DefaultBedrockModelMapping["claude-sonnet-5-5"])
}

// Sonnet 5.5 与 Sonnet 5 都是 $2 / $10；修复前两者在兜底表里都落到 Sonnet 3.5 的 $3 / $15。
func TestClaudeSonnet5x_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tests := []struct {
		model     string
		cacheRead float64
	}{
		{"claude-sonnet-5-5", 0.1e-6},
		{"anthropic.claude-sonnet-5-5", 0.1e-6},
		{"claude-sonnet-5", 0.2e-6},
		{"us.anthropic.claude-sonnet-5-v1", 0.2e-6},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			assert.InDelta(t, 2e-6, pricing.InputPricePerToken, 1e-15)
			assert.InDelta(t, 10e-6, pricing.OutputPricePerToken, 1e-15)
			assert.InDelta(t, tt.cacheRead, pricing.CacheReadPricePerToken, 1e-15)
			assert.InDelta(t, 2.5e-6, pricing.CacheCreation5mPrice, 1e-15)
			assert.InDelta(t, 4e-6, pricing.CacheCreation1hPrice, 1e-15)
		})
	}
	// 相邻型号不受影响
	p, err := svc.GetModelPricing("claude-sonnet-4-5-20250929")
	require.NoError(t, err)
	assert.InDelta(t, 3e-6, p.InputPricePerToken, 1e-15)
}

// 价格表只有 Sonnet 5 条目时，Sonnet 5.5 不能借用（缓存读价不同）。
func TestClaudeSonnet55_FamilyFallbackDoesNotUseSonnet5(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"claude-sonnet-5": {InputCostPerToken: 2e-6, OutputCostPerToken: 10e-6, CacheReadInputTokenCost: 0.2e-6},
		},
	})
	pricing, err := svc.GetModelPricing("claude-sonnet-5-5")
	require.NoError(t, err)
	assert.InDelta(t, 0.1e-6, pricing.CacheReadPricePerToken, 1e-15)
}

// 线上 LiteLLM 价格表把 Sonnet 5.5 缓存读写成 $0.20（输入价的 10%），
// 官方价是 $0.10（5%）。按官方价计费，避免缓存读多收一倍。
func TestClaudeSonnet55_CacheReadCorrectedToFivePercent(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"claude-sonnet-5-5": {
				InputCostPerToken:                   2e-6,
				OutputCostPerToken:                  10e-6,
				CacheCreationInputTokenCost:         2.5e-6,
				CacheCreationInputTokenCostAbove1hr: 4e-6,
				CacheReadInputTokenCost:             0.2e-6,
			},
		},
	})
	pricing, err := svc.GetModelPricing("claude-sonnet-5-5")
	require.NoError(t, err)
	assert.InDelta(t, 0.1e-6, pricing.CacheReadPricePerToken, 1e-15)
	assert.InDelta(t, 2e-6, pricing.InputPricePerToken, 1e-15)
}

// Sonnet 5.5 只接受 adaptive thinking；Bedrock 上 enabled + budget_tokens 返回 400。
func TestClaudeSonnet55_BedrockThinkingBecomesAdaptive(t *testing.T) {
	body := []byte(`{"thinking":{"type":"enabled","budget_tokens":8000}}`)
	out := sanitizeBedrockThinking(body, "anthropic.claude-sonnet-5-5")
	assert.Equal(t, "adaptive", gjson.GetBytes(out, "thinking.type").String())
	assert.False(t, gjson.GetBytes(out, "thinking.budget_tokens").Exists())

	out = sanitizeBedrockThinking(body, "us.anthropic.claude-sonnet-4-5-20250929-v1:0")
	assert.Equal(t, "enabled", gjson.GetBytes(out, "thinking.type").String())
}
