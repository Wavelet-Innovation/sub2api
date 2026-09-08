package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// OpenAIAudioSpeechRequest holds the routing- and billing-relevant fields of a
// /v1/audio/speech request.
type OpenAIAudioSpeechRequest struct {
	Model string
	// Input is the text to synthesize. It is the billable input and the text
	// the moderation coordinator inspects.
	Input          string
	Voice          string
	ResponseFormat string
	// Instructions steers delivery on gpt-4o-mini-tts. It is client-supplied
	// text, so it is moderated alongside Input.
	Instructions string
	// Stream reports whether the client asked for incremental audio chunks
	// (stream_format=sse). The audio body is relayed incrementally either way.
	Stream bool
}

// ParseOpenAIAudioSpeechRequest validates a speech synthesis request body.
func ParseOpenAIAudioSpeechRequest(body []byte) (*OpenAIAudioSpeechRequest, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("failed to parse request body")
	}
	req := &OpenAIAudioSpeechRequest{
		Model:          strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		Input:          gjson.GetBytes(body, "input").String(),
		Voice:          strings.TrimSpace(gjson.GetBytes(body, "voice").String()),
		ResponseFormat: strings.TrimSpace(gjson.GetBytes(body, "response_format").String()),
		Instructions:   gjson.GetBytes(body, "instructions").String(),
	}
	if streamFormat := strings.TrimSpace(gjson.GetBytes(body, "stream_format").String()); streamFormat != "" {
		req.Stream = strings.EqualFold(streamFormat, "sse")
	}
	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("input is required")
	}
	if req.Voice == "" {
		return nil, fmt.Errorf("voice is required")
	}
	return req, nil
}

// ModerationBody returns the client-supplied text for the moderation
// coordinator. Unlike transcription, speech synthesis is driven entirely by
// client text, so this is always populated.
func (r *OpenAIAudioSpeechRequest) ModerationBody() []byte {
	if r == nil {
		return nil
	}
	payload := map[string]any{"input": r.Input}
	if instructions := strings.TrimSpace(r.Instructions); instructions != "" {
		payload["instructions"] = instructions
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return body
}

// EstimateOpenAIAudioSpeechInputTokens counts the billable input tokens for a
// speech request with the model's own tiktoken encoding.
//
// The endpoint returns raw audio and carries no usage object, so the input
// side is the only usage this gateway can measure. Generated audio duration is
// NOT metered here.
func EstimateOpenAIAudioSpeechInputTokens(model string, req *OpenAIAudioSpeechRequest) int {
	if req == nil {
		return 0
	}
	text := req.Input
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		text += "\n" + instructions
	}
	if strings.TrimSpace(text) == "" {
		return 0
	}
	codec, err := openAIInputTokensCodecForModel(model)
	if err != nil {
		return estimateTokensForText(text)
	}
	count, err := codec.Count(text)
	if err != nil || count < 0 {
		return estimateTokensForText(text)
	}
	return count
}

// ForwardAudioSpeech proxies a text-to-speech request upstream and relays the
// audio body to the client as it arrives.
func (s *OpenAIGatewayService) ForwardAudioSpeech(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	parsed *OpenAIAudioSpeechRequest,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	if parsed == nil || strings.TrimSpace(parsed.Model) == "" {
		writeOpenAIAudioSpeechError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	originalModel := parsed.Model

	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	upstreamBody := body
	if upstreamModel != originalModel {
		upstreamBody = ReplaceModelInBody(body, upstreamModel)
	}

	apiKey := account.GetOpenAIApiKey()
	if apiKey == "" {
		return nil, fmt.Errorf("account %d missing api_key", account.ID)
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	validatedURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base_url: %w", err)
	}
	targetURL := buildOpenAIAudioSpeechURL(validatedURL)

	logger.L().Debug("openai audio speech: forwarding",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.String("voice", parsed.Voice),
	)

	// Synthesis holds the connection open while audio is produced, so it uses
	// the stream-detached context like other long-lived upstream calls.
	upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, true)
	defer releaseUpstreamCtx()

	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(upstreamBody))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	upstreamReq.Header.Set("Accept", "*/*")
	for key, values := range c.Request.Header {
		if openaiCCRawAllowedHeaders[strings.ToLower(key)] {
			for _, v := range values {
				upstreamReq.Header.Add(key, v)
			}
		}
	}
	if customUA := account.GetOpenAIUserAgent(); customUA != "" {
		upstreamReq.Header.Set("user-agent", customUA)
	}
	account.ApplyHeaderOverrides(upstreamReq.Header)

	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		safeErr := sanitizeUpstreamErrorMessage(err.Error())
		setOpsUpstreamError(c, 0, safeErr, "")
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			UpstreamURL:        safeUpstreamURL(targetURL),
			Kind:               "request_error",
			Message:            safeErr,
		})
		writeOpenAIAudioSpeechError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
		return nil, fmt.Errorf("upstream request failed: %s", safeErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = truncateString(string(respBody), maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				UpstreamURL:        safeUpstreamURL(targetURL),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: !shouldDisable && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		writeOpenAIAudioSpeechUpstreamError(c, resp, respBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	audioBytes, firstByteMs, relayErr := relayOpenAIAudioSpeechBody(resp, c, s.responseHeaderFilter, startTime)

	result := &OpenAIForwardResult{
		RequestID:        firstNonEmptyString(resp.Header.Get("x-request-id"), resp.Header.Get("request-id")),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/audio/speech",
		Stream:           parsed.Stream,
		FirstTokenMs:     firstByteMs,
		Duration:         time.Since(startTime),
		// Only the input side is measurable: the response is raw audio and
		// carries no usage object. Generated audio is not metered.
		Usage: OpenAIUsage{InputTokens: EstimateOpenAIAudioSpeechInputTokens(billingModel, parsed)},
	}
	if relayErr != nil {
		return result, relayErr
	}
	logger.L().Debug("openai audio speech: completed",
		zap.Int64("account_id", account.ID),
		zap.Int("audio_bytes", audioBytes),
		zap.Int("input_tokens", result.Usage.InputTokens),
	)
	return result, nil
}

// relayOpenAIAudioSpeechBody streams the audio response to the client,
// returning the number of bytes relayed and the time to first byte.
func relayOpenAIAudioSpeechBody(
	resp *http.Response,
	c *gin.Context,
	filter *responseheaders.CompiledHeaderFilter,
	startTime time.Time,
) (int, *int, error) {
	if resp.Header != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
	}
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "audio/mpeg"
	}
	c.Writer.Header().Set("Content-Type", contentType)
	c.Writer.WriteHeader(resp.StatusCode)

	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	total := 0
	var firstByteMs *int

	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if firstByteMs == nil {
				ms := int(time.Since(startTime).Milliseconds())
				firstByteMs = &ms
			}
			written, writeErr := c.Writer.Write(buf[:n])
			total += written
			if writeErr != nil {
				// The client is gone; stop relaying. Usage is still recorded
				// because the upstream call was already billed to us.
				return total, firstByteMs, nil
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, firstByteMs, nil
			}
			return total, firstByteMs, fmt.Errorf("read upstream audio: %w", err)
		}
	}
}

func writeOpenAIAudioSpeechUpstreamError(c *gin.Context, resp *http.Response, body []byte, filter *responseheaders.CompiledHeaderFilter) {
	if c == nil || resp == nil || c.Writer.Written() {
		return
	}
	if resp.Header != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(body)
}

func writeOpenAIAudioSpeechError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func buildOpenAIAudioSpeechURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/audio/speech")
}
