package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// openAIAudioMaxFormFieldSize caps a single non-file multipart field while
// parsing the transcription request. Audio parts are skipped without being
// buffered, so this only guards oversized text fields such as "prompt".
const openAIAudioMaxFormFieldSize = 1 << 20 // 1MB

// OpenAIAudioTranscriptionRequest holds the routing- and billing-relevant
// fields parsed from a /v1/audio/transcriptions request. The audio payload
// itself is never copied out of the raw body.
type OpenAIAudioTranscriptionRequest struct {
	Model string
	// Prompt is the optional transcription hint. It is the only
	// client-supplied free text on this endpoint, so it is what the content
	// moderation coordinator inspects.
	Prompt         string
	Stream         bool
	Multipart      bool
	ContentType    string
	ResponseFormat string
	// HasFile reports whether a file part was present. Clients occasionally
	// send the model only, which upstream rejects with a confusing error.
	HasFile bool
}

// ParseOpenAIAudioTranscriptionRequest extracts model/stream/response_format
// from a transcription request. multipart/form-data is the documented wire
// format; a JSON body is accepted so relays that re-encode the request keep
// working.
func ParseOpenAIAudioTranscriptionRequest(body []byte, contentType string) (*OpenAIAudioTranscriptionRequest, error) {
	req := &OpenAIAudioTranscriptionRequest{ContentType: strings.TrimSpace(contentType)}

	mediaType, params, err := mime.ParseMediaType(req.ContentType)
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		req.Multipart = true
		if err := parseOpenAIAudioTranscriptionMultipart(body, params["boundary"], req); err != nil {
			return nil, err
		}
	} else {
		if !gjson.ValidBytes(body) {
			return nil, fmt.Errorf("request must be multipart/form-data or valid JSON")
		}
		req.Model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
		req.ResponseFormat = strings.TrimSpace(gjson.GetBytes(body, "response_format").String())
		req.Prompt = strings.TrimSpace(gjson.GetBytes(body, "prompt").String())
		if streamResult := gjson.GetBytes(body, "stream"); streamResult.Exists() {
			if streamResult.Type != gjson.True && streamResult.Type != gjson.False {
				return nil, fmt.Errorf("invalid stream field type")
			}
			req.Stream = streamResult.Bool()
		}
		req.HasFile = gjson.GetBytes(body, "file").Exists()
	}

	if req.Model == "" {
		return nil, fmt.Errorf("model is required")
	}
	if !req.HasFile {
		return nil, fmt.Errorf("file is required")
	}
	return req, nil
}

func parseOpenAIAudioTranscriptionMultipart(body []byte, boundary string, req *OpenAIAudioTranscriptionRequest) error {
	boundary = strings.TrimSpace(boundary)
	if boundary == "" {
		return fmt.Errorf("multipart boundary is required")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read multipart body: %w", err)
		}
		name := strings.TrimSpace(part.FormName())
		// File parts (the audio itself) are never buffered — the raw body is
		// forwarded verbatim, so reading them here would only waste memory.
		if strings.TrimSpace(part.FileName()) != "" {
			if name == "file" {
				req.HasFile = true
			}
			_ = part.Close()
			continue
		}
		if name == "" {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, openAIAudioMaxFormFieldSize))
		_ = part.Close()
		if err != nil {
			return fmt.Errorf("read multipart field %s: %w", name, err)
		}
		value := strings.TrimSpace(string(data))
		switch name {
		case "model":
			req.Model = value
		case "response_format":
			req.ResponseFormat = value
		case "prompt":
			req.Prompt = value
		case "stream":
			switch strings.ToLower(value) {
			case "true", "1":
				req.Stream = true
			case "false", "0", "":
				req.Stream = false
			default:
				return fmt.Errorf("invalid stream field value")
			}
		}
	}
	return nil
}

// ModerationBody returns the client-supplied text for the content moderation
// coordinator, or nil when the request carries none. The audio payload itself
// is opaque to the text pipeline and is deliberately not included.
func (r *OpenAIAudioTranscriptionRequest) ModerationBody() []byte {
	if r == nil {
		return nil
	}
	prompt := strings.TrimSpace(r.Prompt)
	if prompt == "" {
		return nil
	}
	body, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		return nil
	}
	return body
}

// ForwardAudioTranscriptions proxies an OpenAI-compatible speech-to-text
// request to the selected account's upstream. The request body is forwarded
// byte-for-byte unless model mapping requires rewriting the "model" field.
func (s *OpenAIGatewayService) ForwardAudioTranscriptions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	parsed *OpenAIAudioTranscriptionRequest,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	if parsed == nil || strings.TrimSpace(parsed.Model) == "" {
		writeOpenAIAudioTranscriptionError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	originalModel := parsed.Model

	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)

	forwardBody := body
	forwardContentType := parsed.ContentType
	if upstreamModel != originalModel {
		rewritten, rewrittenType, err := rewriteOpenAIImagesModel(body, parsed.ContentType, upstreamModel)
		if err != nil {
			return nil, fmt.Errorf("rewrite transcription model: %w", err)
		}
		forwardBody = rewritten
		forwardContentType = rewrittenType
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
	targetURL := buildOpenAIAudioTranscriptionsURL(validatedURL)

	logger.L().Debug("openai audio transcriptions: forwarding",
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", parsed.Stream),
	)

	upstreamCtx, releaseUpstreamCtx := detachStreamUpstreamContext(ctx, parsed.Stream)
	defer releaseUpstreamCtx()

	upstreamReq, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, bytes.NewReader(forwardBody))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	upstreamReq = upstreamReq.WithContext(WithHTTPUpstreamProfile(upstreamReq.Context(), HTTPUpstreamProfileOpenAI))
	upstreamReq.Header.Set("Content-Type", forwardContentType)
	upstreamReq.Header.Set("Authorization", "Bearer "+apiKey)
	if parsed.Stream {
		upstreamReq.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamReq.Header.Set("Accept", "application/json")
	}
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
		writeOpenAIAudioTranscriptionError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed")
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
		writeOpenAIAudioTranscriptionUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	result := &OpenAIForwardResult{
		RequestID:        firstNonEmptyString(resp.Header.Get("x-request-id"), resp.Header.Get("request-id")),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: "/v1/audio/transcriptions",
		Stream:           parsed.Stream,
	}

	if parsed.Stream {
		usage, firstTokenMs, err := s.relayOpenAIAudioTranscriptionStream(resp, c, startTime)
		result.Usage = usage
		result.FirstTokenMs = firstTokenMs
		result.Duration = time.Since(startTime)
		if err != nil {
			return result, err
		}
		return result, nil
	}

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if !errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			writeOpenAIAudioTranscriptionError(c, http.StatusBadGateway, "api_error", "Failed to read upstream response")
		}
		return nil, fmt.Errorf("read upstream body: %w", err)
	}

	writeOpenAIAudioTranscriptionUpstreamResponse(c, resp, respBody, s.responseHeaderFilter)

	result.Usage = extractOpenAIAudioTranscriptionUsage(respBody)
	result.Duration = time.Since(startTime)
	return result, nil
}

// relayOpenAIAudioTranscriptionStream streams an SSE transcription response to
// the client while accumulating usage from the terminal event. Upstream is
// drained even after a client disconnect so billing stays accurate.
func (s *OpenAIGatewayService) relayOpenAIAudioTranscriptionStream(
	resp *http.Response,
	c *gin.Context,
	startTime time.Time,
) (OpenAIUsage, *int, error) {
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "text/event-stream"
	}
	c.Status(resp.StatusCode)
	c.Header("Content-Type", contentType)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return OpenAIUsage{}, nil, fmt.Errorf("streaming is not supported by response writer")
	}

	usage := OpenAIUsage{}
	var firstTokenMs *int
	clientDisconnected := false
	var sseData openAISSEDataAccumulator

	processSSEData := func(dataBytes []byte) {
		mergeOpenAIAudioTranscriptionUsage(&usage, dataBytes)
	}

	processLine := func(line []byte) {
		if len(line) == 0 {
			return
		}
		if firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
		if !clientDisconnected {
			if _, writeErr := c.Writer.Write(line); writeErr != nil {
				clientDisconnected = true
				logger.L().Debug("openai audio transcriptions: client disconnected, draining upstream for billing")
			} else {
				flusher.Flush()
			}
		}
		sseData.AddLine(strings.TrimRight(string(line), "\r\n"), processSSEData)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadBytes('\n')
		processLine(line)
		if err == io.EOF {
			break
		}
		if err != nil {
			sseData.Flush(processSSEData)
			return usage, firstTokenMs, fmt.Errorf("read upstream stream: %w", err)
		}
	}
	sseData.Flush(processSSEData)
	return usage, firstTokenMs, nil
}

func writeOpenAIAudioTranscriptionUpstreamResponse(c *gin.Context, resp *http.Response, body []byte, filter *responseheaders.CompiledHeaderFilter) {
	if c == nil || resp == nil {
		return
	}
	if c.Writer.Written() {
		return
	}
	if resp.Header != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
	}
	// response_format=text/srt/vtt returns a non-JSON body; preserve whatever
	// content type upstream chose instead of forcing application/json.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Writer.Header().Set("Content-Type", ct)
	} else {
		c.Writer.Header().Set("Content-Type", "application/json")
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(body)
}

func writeOpenAIAudioTranscriptionError(c *gin.Context, statusCode int, errType, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

// extractOpenAIAudioTranscriptionUsage reads the usage object returned by
// transcription models. Two shapes exist:
//
//	{"type":"tokens","input_tokens":N,"input_token_details":{"audio_tokens":N,
//	 "text_tokens":N},"output_tokens":N}   → gpt-4o-transcribe family
//	{"type":"duration","seconds":N}        → whisper-1
//
// Duration-billed responses carry no tokens, so they yield a zero usage and
// are priced by the caller's normal unknown-token path.
func extractOpenAIAudioTranscriptionUsage(body []byte) OpenAIUsage {
	if !gjson.ValidBytes(body) {
		return OpenAIUsage{}
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.Exists() || !usage.IsObject() {
		return OpenAIUsage{}
	}
	return openAIAudioTranscriptionUsageFromResult(usage)
}

func openAIAudioTranscriptionUsageFromResult(usage gjson.Result) OpenAIUsage {
	inputTokens := firstPositiveGJSONInt(
		usage.Get("input_tokens"),
		usage.Get("prompt_tokens"),
	)
	outputTokens := firstPositiveGJSONInt(
		usage.Get("output_tokens"),
		usage.Get("completion_tokens"),
	)
	// Audio input tokens are reported separately for observability; they are
	// already included in input_tokens, so they are not added on top.
	return OpenAIUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
	}
}

// mergeOpenAIAudioTranscriptionUsage folds usage from one SSE data payload
// into dst. The terminal transcript.text.done event carries the totals.
func mergeOpenAIAudioTranscriptionUsage(dst *OpenAIUsage, data []byte) {
	if dst == nil || len(data) == 0 {
		return
	}
	if !gjson.ValidBytes(data) {
		return
	}
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() || !usage.IsObject() {
		return
	}
	parsed := openAIAudioTranscriptionUsageFromResult(usage)
	if parsed.InputTokens > 0 {
		dst.InputTokens = parsed.InputTokens
	}
	if parsed.OutputTokens > 0 {
		dst.OutputTokens = parsed.OutputTokens
	}
}

func buildOpenAIAudioTranscriptionsURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/audio/transcriptions")
}
