package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// earlyFailureProbeBytes caps how much of a 2xx body we inspect before handing the
// stream to the client. Capacity errors from xAI often arrive as the first JSON/SSE
// payload under HTTP 200; catching them here enables account rotation.
const earlyFailureProbeBytes = 64 << 10

// probeSuccessResponseForRetry peeks a 2xx response body for immediate capacity (or
// request-scoped safety) failures. On retryable failure the body is closed and a
// classified UpstreamFailure is returned. On success any peeked bytes are rebound
// so the caller can still stream the full response.
func probeSuccessResponseForRetry(response *provider.Response) *UpstreamFailure {
	if response == nil || response.Body == nil {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil
	}
	peeked, rest, err := peekResponsePrefix(response.Body, earlyFailureProbeBytes)
	if err != nil && len(peeked) == 0 {
		// Read error with no data: leave body as-is for the transport layer.
		response.Body = rest
		return nil
	}
	failure := classifyEarlySuccessPayload(peeked)
	if failure != nil {
		_ = rest.Close()
		response.Body = io.NopCloser(bytes.NewReader(nil))
		return failure
	}
	response.Body = rest
	return nil
}

func peekResponsePrefix(body io.ReadCloser, limit int) ([]byte, io.ReadCloser, error) {
	if body == nil {
		return nil, io.NopCloser(bytes.NewReader(nil)), nil
	}
	if limit < 1 {
		limit = earlyFailureProbeBytes
	}
	// Read only until we can classify the first JSON object / SSE event, so healthy
	// streams are not delayed waiting for a full 64 KiB window.
	var collected bytes.Buffer
	chunk := make([]byte, 4<<10)
	var readErr error
	for collected.Len() < limit {
		n, err := body.Read(chunk)
		if n > 0 {
			remain := limit - collected.Len()
			if n > remain {
				n = remain
			}
			_, _ = collected.Write(chunk[:n])
		}
		readErr = err
		if earlyProbeDecodable(collected.Bytes()) || err != nil || collected.Len() >= limit {
			break
		}
	}
	peeked := append([]byte(nil), collected.Bytes()...)
	if len(peeked) == 0 {
		if readErr == nil || errors.Is(readErr, io.EOF) {
			_ = body.Close()
			return nil, io.NopCloser(bytes.NewReader(nil)), nil
		}
		return nil, body, readErr
	}
	if errors.Is(readErr, io.EOF) {
		_ = body.Close()
		return peeked, io.NopCloser(bytes.NewReader(peeked)), nil
	}
	return peeked, &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(peeked), body), source: body}, readErr
}

func earlyProbeDecodable(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		var raw json.RawMessage
		return json.Unmarshal(trimmed, &raw) == nil
	}
	// One complete SSE event (terminated by blank line) is enough to classify.
	return bytes.Contains(data, []byte("\n\n"))
}

func classifyEarlySuccessPayload(peeked []byte) *UpstreamFailure {
	for _, raw := range extractEarlyJSONPayloads(peeked) {
		if failure := classifyEarlyJSONObject(raw); failure != nil {
			return failure
		}
	}
	return nil
}

func extractEarlyJSONPayloads(peeked []byte) [][]byte {
	trimmed := bytes.TrimSpace(peeked)
	if len(trimmed) == 0 {
		return nil
	}
	// Non-stream JSON body (OpenAI-style error under HTTP 200).
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return [][]byte{trimmed}
	}
	// SSE: take data fields from the first event block(s).
	var payloads [][]byte
	for _, block := range bytes.Split(peeked, []byte("\n\n")) {
		block = bytes.ReplaceAll(block, []byte("\r\n"), []byte("\n"))
		var dataLines [][]byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				dataLines = append(dataLines, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		payload := bytes.Join(dataLines, nil)
		payload = bytes.TrimSpace(payload)
		if bytes.Equal(payload, []byte("[DONE]")) || len(payload) == 0 {
			continue
		}
		if payload[0] == '{' || payload[0] == '[' {
			payloads = append(payloads, payload)
		}
		// Only need the first meaningful event to decide rotate-or-not.
		if len(payloads) > 0 {
			break
		}
	}
	return payloads
}

func classifyEarlyJSONObject(raw []byte) *UpstreamFailure {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	typ := strings.ToLower(strings.TrimSpace(anyString(root["type"])))
	if isLikelySuccessStreamType(typ) {
		return nil
	}
	// Successful non-stream chat/responses payloads.
	if typ == "" {
		if _, hasChoices := root["choices"]; hasChoices {
			return nil
		}
		if output, ok := root["output"]; ok && output != nil {
			return nil
		}
		if _, hasID := root["id"]; hasID {
			if status := strings.ToLower(strings.TrimSpace(anyString(root["status"]))); status == "completed" || status == "in_progress" {
				return nil
			}
		}
	}

	code, errType, message := extractUpstreamErrorMetadata(raw)
	metadataText := strings.ToLower(strings.Join([]string{code, errType, message, typ, string(raw)}, " "))

	// Only act on clear terminal/error shapes so we never rotate on a normal first delta.
	if !isEarlyTerminalErrorShape(typ, errType, root) && !isPlatformCapacityBusy(metadataText, code) {
		return nil
	}

	if isContentSafetyRejection(metadataText, code) {
		return &UpstreamFailure{
			HTTPStatus: http.StatusForbidden, Code: "upstream_content_policy",
			PublicMessage: "上游认为内容不符合使用规范", UpstreamCode: firstNonEmptyFailure(code, errType),
			RequestScoped: true, AccountScoped: false,
			Fingerprint: fmt.Sprintf("200:content_policy:%s", normalizeFailureCode(firstNonEmptyFailure(code, errType, "policy"))),
		}
	}
	if isPlatformCapacityBusy(metadataText, code) {
		return &UpstreamFailure{
			HTTPStatus: http.StatusTooManyRequests, Code: "upstream_capacity",
			PublicMessage: "上游当前繁忙，请稍后重试", UpstreamCode: firstNonEmptyFailure(code, errType),
			PlatformBusy: true, AccountScoped: false,
			Fingerprint: fmt.Sprintf("200:capacity:%s", normalizeFailureCode(firstNonEmptyFailure(code, errType, "busy"))),
		}
	}
	return nil
}

func isLikelySuccessStreamType(typ string) bool {
	switch typ {
	case "response.created", "response.in_progress", "response.output_item.added",
		"response.output_item.done", "response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done", "response.reasoning_summary_text.delta",
		"response.completed", "response.incomplete",
		"message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop",
		"ping", "message":
		return true
	default:
		return false
	}
}

func isEarlyTerminalErrorShape(typ, errType string, root map[string]any) bool {
	switch typ {
	case "error", "response.failed", "response.error":
		return true
	}
	if errType == "error" || errType == "server_error" || errType == "overloaded_error" {
		return true
	}
	if _, ok := root["error"]; ok && (typ == "" || typ == "error") {
		// OpenAI flat error object: {"message":"...","type":"error"}
		if msg := strings.TrimSpace(anyString(root["message"])); msg != "" || typ == "error" {
			return true
		}
	}
	return false
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return ""
	}
}
