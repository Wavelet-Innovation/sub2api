package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var grokNewReasoningModels = []string{"grok-4.6", "grok-4.7"}

func TestGrok47_ListedWithoutMovingLatestAlias(t *testing.T) {
	ids := xai.DefaultModelIDs()
	mapping := xai.DefaultModelMapping()
	for _, model := range grokNewReasoningModels {
		assert.Contains(t, ids, model)
		assert.Equal(t, model, mapping[model])
	}
	// grok / grok-latest 仍指向 grok-4.5
	assert.Equal(t, "grok-4.5", mapping["grok"])
	assert.Equal(t, "grok-4.5", mapping["grok-latest"])
}

// Grok 4.5 / 4.6 / 4.7 都是推理模型：同样走 Responses 运行时（缓存与图片输入），
// 同样剥离推理模型不支持的 penalty / stop 参数。
func TestGrok47_TreatedAsReasoningModels(t *testing.T) {
	for _, model := range append([]string{"grok-4.5"}, grokNewReasoningModels...) {
		assert.True(t, isGrokReasoningResponsesModel(model), model)
		assert.True(t, grokChatResponsesRuntimeEligible(model, "cache-key"), model)
	}
	assert.False(t, isGrokReasoningResponsesModel("grok-4.3"))
	assert.False(t, isGrokReasoningResponsesModel("grok-build-0.1"))
}

// 官方价：4.6 / 4.7 为 $2 / $6，缓存读 $0.50；4.5 缓存读为 $0.30（此前误写 $0.50）。
func TestGrok47_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tests := []struct {
		model     string
		cacheRead float64
	}{
		{"grok-4.5", 0.3e-6},
		{"grok-4.6", 0.5e-6},
		{"grok-4.7", 0.5e-6},
	}
	for _, tt := range tests {
		pricing, err := svc.GetModelPricing(tt.model)
		require.NoError(t, err, tt.model)
		assert.InDelta(t, 2e-6, pricing.InputPricePerToken, 1e-15, tt.model)
		assert.InDelta(t, 6e-6, pricing.OutputPricePerToken, 1e-15, tt.model)
		assert.InDelta(t, tt.cacheRead, pricing.CacheReadPricePerToken, 1e-15, tt.model)
	}
}
