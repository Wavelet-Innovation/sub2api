package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var gptImage25Models = []string{"gpt-image-2.5-flare", "gpt-image-2.5-sunburst"}

func TestGPTImage25_ListedAndRoutedAsImageModels(t *testing.T) {
	ids := openai.DefaultModelIDs()
	for _, model := range gptImage25Models {
		assert.Contains(t, ids, model)
		assert.True(t, IsGPTImageGenerationModel(model), model)
		assert.NoError(t, validateOpenAIImagesModel(model), model)
	}
	require.Equal(t, "gpt-5.6-sol", openai.DefaultModels[0].ID)
}

// 默认图像模型保持 gpt-image-2。
func TestGPTImage25_DefaultImageModelUnchanged(t *testing.T) {
	req := &OpenAIImagesRequest{}
	applyOpenAIImagesDefaults(req)
	assert.Equal(t, "gpt-image-2", req.Model)
}

// 价格表缺少 2.5 条目时回退到同价的 gpt-image-2，而不是计费失败。
func TestGPTImage25_PricingFallsBackToGPTImage2(t *testing.T) {
	svc := NewBillingService(&config.Config{}, &PricingService{
		pricingData: map[string]*LiteLLMModelPricing{
			"gpt-image-2": {InputCostPerToken: 5e-6, InputCostPerImageToken: 8e-6, OutputCostPerImageToken: 30e-6},
		},
	})
	for _, model := range gptImage25Models {
		pricing, err := svc.GetModelPricing(model)
		require.NoError(t, err, model)
		assert.InDelta(t, 30e-6, pricing.ImageOutputPricePerToken, 1e-15, model)
	}
}
