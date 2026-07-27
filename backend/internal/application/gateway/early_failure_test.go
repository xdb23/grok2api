package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestClassifyEarlySuccessPayloadDetectsCapacityJSON(t *testing.T) {
	body := []byte(`{"code":null,"message":"The model is currently at capacity due to high demand. Please try again in a few minutes, or use a higher service tier for priority processing: https://docs.x.ai/developers/advanced-api-usage/priority-processing","param":null,"type":"error"}`)
	failure := classifyEarlySuccessPayload(body)
	if failure == nil || !failure.PlatformBusy || failure.Code != "upstream_capacity" {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestClassifyEarlySuccessPayloadDetectsCapacitySSE(t *testing.T) {
	body := []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"The server is experiencing high demand\"}}\n\n")
	failure := classifyEarlySuccessPayload(body)
	if failure == nil || !failure.PlatformBusy {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestClassifyEarlySuccessPayloadIgnoresNormalStreamStart(t *testing.T) {
	body := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n")
	if failure := classifyEarlySuccessPayload(body); failure != nil {
		t.Fatalf("unexpected failure = %#v", failure)
	}
}

func TestProbeSuccessResponseForRetryRebindsBodyOnSuccess(t *testing.T) {
	payload := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" + strings.Repeat("x", 100))
	response := &provider.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
	if failure := probeSuccessResponseForRetry(response); failure != nil {
		t.Fatalf("unexpected failure = %#v", failure)
	}
	got, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("rebinding lost body bytes: got %d want %d", len(got), len(payload))
	}
}

func TestProbeSuccessResponseForRetryClosesOnCapacity(t *testing.T) {
	payload := []byte(`{"type":"error","message":"at capacity due to high demand"}`)
	response := &provider.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}
	failure := probeSuccessResponseForRetry(response)
	if failure == nil || !failure.PlatformBusy {
		t.Fatalf("failure = %#v", failure)
	}
	got, _ := io.ReadAll(response.Body)
	if len(got) != 0 {
		t.Fatalf("capacity failure should discard body, got %q", got)
	}
}
