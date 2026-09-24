package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Claude Opus 5.5 官方定价（USD per token）：$4 输入 / $20 输出 per MTok。
const (
	opus55InputPricePerToken      = 4e-6
	opus55OutputPricePerToken     = 20e-6
	opus55CacheRead               = 0.2e-6
	opus55CacheCreation5mPerToken = 5e-6
	opus55CacheCreation1hPerToken = 8e-6
)

// "claude-opus-5" 是 "claude-opus-5-5" 的子串；两者价格不同，不可相互误判。
func TestClaudeOpus55_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)

	tests := []struct {
		model  string
		input  float64
		output float64
	}{
		{"claude-opus-5-5", opus55InputPricePerToken, opus55OutputPricePerToken},
		{"anthropic.claude-opus-5-5", opus55InputPricePerToken, opus55OutputPricePerToken},
		{"us.anthropic.claude-opus-5-5", opus55InputPricePerToken, opus55OutputPricePerToken},
		{"claude-opus-5", opus5InputPricePerToken, opus5OutputPricePerToken},
		{"claude-opus-4-5-20251101", 5e-6, 25e-6},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			pricing, err := svc.GetModelPricing(tt.model)
			require.NoError(t, err)
			require.NotNil(t, pricing)
			assert.InDelta(t, tt.input, pricing.InputPricePerToken, 1e-12)
			assert.InDelta(t, tt.output, pricing.OutputPricePerToken, 1e-12)
		})
	}

	pricing, err := svc.GetModelPricing("claude-opus-5-5")
	require.NoError(t, err)
	assert.InDelta(t, opus55CacheRead, pricing.CacheReadPricePerToken, 1e-12)
	assert.InDelta(t, opus55CacheCreation5mPerToken, pricing.CacheCreation5mPrice, 1e-12)
	assert.InDelta(t, opus55CacheCreation1hPerToken, pricing.CacheCreation1hPrice, 1e-12)
}

// 定价数据里只有 claude-opus-5 时，claude-opus-5-5 不能借用 Opus 5 的 $5/$25。
func TestClaudeOpus55_FamilyFallbackDoesNotUseOpus5Rates(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"claude-opus-5": {
				InputCostPerToken:  opus5InputPricePerToken,
				OutputCostPerToken: opus5OutputPricePerToken,
			},
		},
	})

	pricing, err := svc.GetModelPricing("claude-opus-5-5")
	require.NoError(t, err)
	require.NotNil(t, pricing)
	assert.InDelta(t, opus55InputPricePerToken, pricing.InputPricePerToken, 1e-12)
	assert.InDelta(t, opus55OutputPricePerToken, pricing.OutputPricePerToken, 1e-12)
}

func TestClaudeOpus55_BedrockAndModelList(t *testing.T) {
	assert.Equal(t, "anthropic.claude-opus-5-5", domain.DefaultBedrockModelMapping["claude-opus-5-5"])

	found := false
	for _, m := range claude.DefaultModels {
		if m.ID == "claude-opus-5-5" {
			found = true
			assert.Equal(t, "Claude Opus 5.5", m.DisplayName)
		}
	}
	assert.True(t, found, "claude-opus-5-5 should be in claude.DefaultModels")

	// Opus 5.5 属于 4.7+：thinking.enabled 转 adaptive，并保留 cache_control.ttl
	assert.True(t, isBedrockOpus47OrNewer("anthropic.claude-opus-5-5"))
	assert.True(t, isBedrockClaude45OrNewer("anthropic.claude-opus-5-5"))
}
