package service

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// buildTranscriptionMultipart returns a multipart body carrying an audio file
// plus the given text fields, mirroring what the OpenAI SDK sends.
func buildTranscriptionMultipart(t *testing.T, fields map[string]string, audio []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if audio != nil {
		part, err := w.CreateFormFile("file", "speech.mp3")
		require.NoError(t, err)
		_, err = part.Write(audio)
		require.NoError(t, err)
	}
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	return buf.Bytes(), w.FormDataContentType()
}

func TestBuildOpenAIAudioTranscriptionsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{"bare domain", "https://api.openai.com", "https://api.openai.com/v1/audio/transcriptions"},
		{"bare /v1", "https://api.openai.com/v1", "https://api.openai.com/v1/audio/transcriptions"},
		{"already full path", "https://api.openai.com/v1/audio/transcriptions", "https://api.openai.com/v1/audio/transcriptions"},
		{"third-party versioned path", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/audio/transcriptions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, buildOpenAIAudioTranscriptionsURL(tt.base))
		})
	}
}

func TestParseOpenAIAudioTranscriptionRequest_Multipart(t *testing.T) {
	t.Parallel()

	body, contentType := buildTranscriptionMultipart(t, map[string]string{
		"model":           "gpt-4o-transcribe",
		"response_format": "json",
		"stream":          "true",
		"prompt":          "technical vocabulary",
	}, []byte("fake-audio-bytes"))

	parsed, err := ParseOpenAIAudioTranscriptionRequest(body, contentType)

	require.NoError(t, err)
	require.Equal(t, "gpt-4o-transcribe", parsed.Model)
	require.Equal(t, "json", parsed.ResponseFormat)
	require.True(t, parsed.Stream)
	require.True(t, parsed.Multipart)
	require.True(t, parsed.HasFile)
}

func TestParseOpenAIAudioTranscriptionRequest_DefaultsAndErrors(t *testing.T) {
	t.Parallel()

	t.Run("stream defaults to false", func(t *testing.T) {
		t.Parallel()
		body, ct := buildTranscriptionMultipart(t, map[string]string{"model": "whisper-1"}, []byte("audio"))
		parsed, err := ParseOpenAIAudioTranscriptionRequest(body, ct)
		require.NoError(t, err)
		require.False(t, parsed.Stream)
		require.Equal(t, "whisper-1", parsed.Model)
	})

	t.Run("missing model is rejected", func(t *testing.T) {
		t.Parallel()
		body, ct := buildTranscriptionMultipart(t, map[string]string{"response_format": "text"}, []byte("audio"))
		_, err := ParseOpenAIAudioTranscriptionRequest(body, ct)
		require.ErrorContains(t, err, "model is required")
	})

	t.Run("missing file is rejected", func(t *testing.T) {
		t.Parallel()
		body, ct := buildTranscriptionMultipart(t, map[string]string{"model": "gpt-4o-transcribe"}, nil)
		_, err := ParseOpenAIAudioTranscriptionRequest(body, ct)
		require.ErrorContains(t, err, "file is required")
	})

	t.Run("invalid stream value is rejected", func(t *testing.T) {
		t.Parallel()
		body, ct := buildTranscriptionMultipart(t, map[string]string{"model": "m", "stream": "yes"}, []byte("audio"))
		_, err := ParseOpenAIAudioTranscriptionRequest(body, ct)
		require.ErrorContains(t, err, "invalid stream field value")
	})
}

func TestForwardAudioTranscriptions_ForwardsBodyVerbatimAndRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	audio := []byte("fake-audio-bytes")
	body, contentType := buildTranscriptionMultipart(t, map[string]string{
		"model":           "gpt-4o-transcribe",
		"response_format": "json",
	}, audio)

	parsed, err := ParseOpenAIAudioTranscriptionRequest(body, contentType)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"asr-rid"},
		},
		Body: io.NopCloser(strings.NewReader(`{
			"text":"hello world",
			"usage":{
				"type":"tokens",
				"input_tokens":14,
				"input_token_details":{"text_tokens":0,"audio_tokens":14},
				"output_tokens":45,
				"total_tokens":59
			}
		}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com",
		},
	}

	result, err := svc.ForwardAudioTranscriptions(context.Background(), c, account, body, parsed, "")

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, result)
	require.Equal(t, "asr-rid", result.RequestID)
	require.Equal(t, "gpt-4o-transcribe", result.Model)
	require.Equal(t, 14, result.Usage.InputTokens)
	require.Equal(t, 45, result.Usage.OutputTokens)
	require.Equal(t, "https://api.openai.com/v1/audio/transcriptions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	// No model mapping, so the multipart body must reach upstream untouched.
	require.Equal(t, body, upstream.lastBody)
	require.Equal(t, contentType, upstream.lastReq.Header.Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "hello world")
}

func TestForwardAudioTranscriptions_ModelMappingRewritesMultipartModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	audio := []byte("fake-audio-bytes-that-must-survive")
	body, contentType := buildTranscriptionMultipart(t, map[string]string{
		"model": "house-asr",
	}, audio)

	parsed, err := ParseOpenAIAudioTranscriptionRequest(body, contentType)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"text":"ok","usage":{"type":"tokens","input_tokens":3,"output_tokens":4}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       9,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"base_url":      "https://api.openai.com",
			"model_mapping": map[string]any{"house-asr": "gpt-4o-mini-transcribe"},
		},
	}

	result, err := svc.ForwardAudioTranscriptions(context.Background(), c, account, body, parsed, "")

	require.NoError(t, err)
	require.Equal(t, "house-asr", result.Model)
	require.Equal(t, "gpt-4o-mini-transcribe", result.UpstreamModel)

	// The rewritten body must still be valid multipart, carry the mapped model,
	// and preserve the audio part byte-for-byte.
	sentType := upstream.lastReq.Header.Get("Content-Type")
	sentParsed, err := ParseOpenAIAudioTranscriptionRequest(upstream.lastBody, sentType)
	require.NoError(t, err)
	require.Equal(t, "gpt-4o-mini-transcribe", sentParsed.Model)
	require.True(t, sentParsed.HasFile)
	require.Contains(t, string(upstream.lastBody), string(audio))
}

func TestForwardAudioTranscriptions_UpstreamServerErrorTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, contentType := buildTranscriptionMultipart(t, map[string]string{"model": "gpt-4o-transcribe"}, []byte("audio"))
	parsed, err := ParseOpenAIAudioTranscriptionRequest(body, contentType)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"internal"}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
	}

	_, err = svc.ForwardAudioTranscriptions(context.Background(), c, account, body, parsed, "")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusInternalServerError, failoverErr.StatusCode)
	// Nothing may be written downstream, otherwise the caller cannot retry.
	require.Zero(t, rec.Body.Len())
}

func TestExtractOpenAIAudioTranscriptionUsage(t *testing.T) {
	t.Parallel()

	t.Run("token shape", func(t *testing.T) {
		t.Parallel()
		usage := extractOpenAIAudioTranscriptionUsage([]byte(`{"usage":{"type":"tokens","input_tokens":14,"output_tokens":45}}`))
		require.Equal(t, 14, usage.InputTokens)
		require.Equal(t, 45, usage.OutputTokens)
	})

	t.Run("duration shape carries no tokens", func(t *testing.T) {
		t.Parallel()
		usage := extractOpenAIAudioTranscriptionUsage([]byte(`{"text":"hi","usage":{"type":"duration","seconds":9}}`))
		require.Zero(t, usage.InputTokens)
		require.Zero(t, usage.OutputTokens)
	})

	t.Run("non-json body is tolerated", func(t *testing.T) {
		t.Parallel()
		usage := extractOpenAIAudioTranscriptionUsage([]byte("plain transcript text"))
		require.Zero(t, usage.InputTokens)
	})
}

func TestAccountSupportsAudioTranscriptionsCapability(t *testing.T) {
	t.Parallel()

	apiKeyAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.True(t, apiKeyAccount.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioTranscriptions))

	oauthAccount := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, oauthAccount.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioTranscriptions))
}

func TestForwardAudioTranscriptions_StreamRelaysSSEAndExtractsTerminalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body, contentType := buildTranscriptionMultipart(t, map[string]string{
		"model":  "gpt-4o-transcribe",
		"stream": "true",
	}, []byte("audio"))
	parsed, err := ParseOpenAIAudioTranscriptionRequest(body, contentType)
	require.NoError(t, err)
	require.True(t, parsed.Stream)

	sse := strings.Join([]string{
		`data: {"type":"transcript.text.delta","delta":"hello"}`,
		"",
		`data: {"type":"transcript.text.delta","delta":" world"}`,
		"",
		`data: {"type":"transcript.text.done","text":"hello world","usage":{"type":"tokens","input_tokens":21,"output_tokens":8}}`,
		"",
		"data: [DONE]",
		"",
		"",
	}, "\n")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          13,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
	}

	result, err := svc.ForwardAudioTranscriptions(context.Background(), c, account, body, parsed, "")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 21, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
	require.NotNil(t, result.FirstTokenMs)
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	// The client must receive the upstream stream unchanged.
	require.Equal(t, sse, rec.Body.String())
}
