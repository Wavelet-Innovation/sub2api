package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildOpenAIAudioSpeechURL(t *testing.T) {
	t.Parallel()

	tests := []struct{ name, base, want string }{
		{"bare domain", "https://api.openai.com", "https://api.openai.com/v1/audio/speech"},
		{"bare /v1", "https://api.openai.com/v1", "https://api.openai.com/v1/audio/speech"},
		{"already full path", "https://api.openai.com/v1/audio/speech", "https://api.openai.com/v1/audio/speech"},
		{"third-party versioned path", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/audio/speech"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, buildOpenAIAudioSpeechURL(tt.base))
		})
	}
}

func TestParseOpenAIAudioSpeechRequest(t *testing.T) {
	t.Parallel()

	t.Run("parses fields", func(t *testing.T) {
		t.Parallel()
		parsed, err := ParseOpenAIAudioSpeechRequest([]byte(`{
			"model":"gpt-4o-mini-tts",
			"input":"Hello there",
			"voice":"alloy",
			"response_format":"wav",
			"instructions":"Speak slowly",
			"stream_format":"sse"
		}`))
		require.NoError(t, err)
		require.Equal(t, "gpt-4o-mini-tts", parsed.Model)
		require.Equal(t, "Hello there", parsed.Input)
		require.Equal(t, "alloy", parsed.Voice)
		require.Equal(t, "wav", parsed.ResponseFormat)
		require.Equal(t, "Speak slowly", parsed.Instructions)
		require.True(t, parsed.Stream)
	})

	t.Run("rejects missing required fields", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct{ name, body, want string }{
			{"no model", `{"input":"hi","voice":"alloy"}`, "model is required"},
			{"no input", `{"model":"tts-1","voice":"alloy"}`, "input is required"},
			{"blank input", `{"model":"tts-1","input":"   ","voice":"alloy"}`, "input is required"},
			{"no voice", `{"model":"tts-1","input":"hi"}`, "voice is required"},
			{"invalid json", `not json`, "failed to parse request body"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := ParseOpenAIAudioSpeechRequest([]byte(tc.body))
				require.ErrorContains(t, err, tc.want)
			})
		}
	})

	t.Run("stream defaults to false without stream_format", func(t *testing.T) {
		t.Parallel()
		parsed, err := ParseOpenAIAudioSpeechRequest([]byte(`{"model":"tts-1","input":"hi","voice":"alloy"}`))
		require.NoError(t, err)
		require.False(t, parsed.Stream)
	})
}

func TestOpenAIAudioSpeechModerationBodyCarriesClientText(t *testing.T) {
	t.Parallel()

	parsed, err := ParseOpenAIAudioSpeechRequest([]byte(`{"model":"gpt-4o-mini-tts","input":"read this aloud","voice":"alloy","instructions":"whisper it"}`))
	require.NoError(t, err)

	body := parsed.ModerationBody()
	require.Equal(t, "read this aloud", gjson.GetBytes(body, "input").String())
	require.Equal(t, "whisper it", gjson.GetBytes(body, "instructions").String())
}

func TestEstimateOpenAIAudioSpeechInputTokens(t *testing.T) {
	t.Parallel()

	short := &OpenAIAudioSpeechRequest{Input: "Hello"}
	long := &OpenAIAudioSpeechRequest{Input: strings.Repeat("the quick brown fox jumps over the lazy dog. ", 20)}

	shortCount := EstimateOpenAIAudioSpeechInputTokens("gpt-4o-mini-tts", short)
	longCount := EstimateOpenAIAudioSpeechInputTokens("gpt-4o-mini-tts", long)

	require.Positive(t, shortCount)
	require.Greater(t, longCount, shortCount, "longer input must cost more tokens")

	// Instructions are billable input too.
	withInstructions := &OpenAIAudioSpeechRequest{Input: "Hello", Instructions: strings.Repeat("speak very slowly ", 10)}
	require.Greater(t, EstimateOpenAIAudioSpeechInputTokens("gpt-4o-mini-tts", withInstructions), shortCount)

	require.Zero(t, EstimateOpenAIAudioSpeechInputTokens("gpt-4o-mini-tts", nil))
	require.Zero(t, EstimateOpenAIAudioSpeechInputTokens("gpt-4o-mini-tts", &OpenAIAudioSpeechRequest{Input: "   "}))
}

func TestForwardAudioSpeech_RelaysAudioAndBillsInputTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reqBody := []byte(`{"model":"gpt-4o-mini-tts","input":"Hello there, this is a test.","voice":"alloy"}`)
	parsed, err := ParseOpenAIAudioSpeechRequest(reqBody)
	require.NoError(t, err)

	audio := bytes.Repeat([]byte{0x49, 0x44, 0x33, 0x04}, 4096)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"audio/mpeg"},
			"X-Request-Id": []string{"tts-rid"},
		},
		Body: io.NopCloser(bytes.NewReader(audio)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          21,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
	}

	result, err := svc.ForwardAudioSpeech(context.Background(), c, account, reqBody, parsed, "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "tts-rid", result.RequestID)
	require.Equal(t, "gpt-4o-mini-tts", result.Model)
	require.Equal(t, "https://api.openai.com/v1/audio/speech", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	// The audio body must reach the client byte-for-byte.
	require.Equal(t, audio, rec.Body.Bytes())
	require.Equal(t, "audio/mpeg", rec.Header().Get("Content-Type"))
	require.NotNil(t, result.FirstTokenMs)
	// Input text is metered locally; the audio response carries no usage.
	require.Positive(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
}

func TestForwardAudioSpeech_ModelMappingRewritesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reqBody := []byte(`{"model":"house-tts","input":"Hello","voice":"alloy"}`)
	parsed, err := ParseOpenAIAudioSpeechRequest(reqBody)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"audio/mpeg"}},
		Body:       io.NopCloser(strings.NewReader("audio")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       22,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"base_url":      "https://api.openai.com",
			"model_mapping": map[string]any{"house-tts": "gpt-4o-mini-tts"},
		},
	}

	result, err := svc.ForwardAudioSpeech(context.Background(), c, account, reqBody, parsed, "")

	require.NoError(t, err)
	require.Equal(t, "house-tts", result.Model)
	require.Equal(t, "gpt-4o-mini-tts", result.UpstreamModel)
	require.Equal(t, "gpt-4o-mini-tts", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "Hello", gjson.GetBytes(upstream.lastBody, "input").String())
}

func TestForwardAudioSpeech_UpstreamServerErrorTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reqBody := []byte(`{"model":"gpt-4o-mini-tts","input":"Hello","voice":"alloy"}`)
	parsed, err := ParseOpenAIAudioSpeechRequest(reqBody)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/speech", bytes.NewReader(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"internal"}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          23,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
	}

	_, err = svc.ForwardAudioSpeech(context.Background(), c, account, reqBody, parsed, "")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusInternalServerError, failoverErr.StatusCode)
	require.Zero(t, rec.Body.Len(), "nothing may be written downstream before a retry")
}

func TestAccountSupportsAudioSpeechCapability(t *testing.T) {
	t.Parallel()

	require.True(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}).
		SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioSpeech))
	require.False(t, (&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}).
		SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioSpeech))
}
