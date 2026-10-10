package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func geminiFallbackModelIDs() []string {
	var ids []string
	for _, m := range gemini.DefaultModels() {
		ids = append(ids, m.Name)
	}
	return ids
}

func TestGemini38_ModelLists(t *testing.T) {
	ids := geminiFallbackModelIDs()
	for _, model := range []string{"gemini-3.8-flash", "gemini-3.5-flash-lite", "gemini-3.1-flash-lite", "gemini-3.1-flash-lite-image", "gemini-nano-banana-2.1"} {
		assert.Contains(t, ids, "models/"+model)
	}
	// Google 已停用：gemini-2.0-flash（2026-06-01）、gemini-3-pro-preview（2026-03-09）
	for _, retired := range []string{"models/gemini-2.0-flash", "models/gemini-3-pro-preview"} {
		assert.NotContains(t, ids, retired)
	}
	for _, m := range geminicli.DefaultModels {
		assert.NotEqual(t, "gemini-2.0-flash", m.ID)
		assert.NotEqual(t, "gemini-3-pro-preview", m.ID)
	}
	assert.Equal(t, "gemini-2.5-flash", geminicli.DefaultTestModel)
}

// 价格表缺条目时的兜底价（官方正式价；3.8 Flash 至 2026-12-31 另有优惠价，由远端价格表提供）。
func TestGemini38_HardcodedFallbackPricing(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	tests := []struct {
		model             string
		input, out, cache float64
	}{
		{"gemini-3.8-flash", 1.5e-6, 7.5e-6, 0.15e-6},
		{"gemini-3.5-flash-lite", 0.3e-6, 2.5e-6, 0.03e-6},
		{"gemini-3.1-flash-lite", 0.25e-6, 1.5e-6, 0.025e-6},
	}
	for _, tt := range tests {
		pricing, err := svc.GetModelPricing(tt.model)
		require.NoError(t, err, tt.model)
		assert.InDelta(t, tt.input, pricing.InputPricePerToken, 1e-15, tt.model)
		assert.InDelta(t, tt.out, pricing.OutputPricePerToken, 1e-15, tt.model)
		assert.InDelta(t, tt.cache, pricing.CacheReadPricePerToken, 1e-15, tt.model)
	}
	// 3.6 Flash 不受影响
	p, err := svc.GetModelPricing("gemini-3.6-flash")
	require.NoError(t, err)
	assert.InDelta(t, 1.5e-6, p.InputPricePerToken, 1e-15)
}
