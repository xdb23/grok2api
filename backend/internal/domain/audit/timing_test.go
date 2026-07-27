package audit

import "testing"

func TestComputeTokensPerSecond(t *testing.T) {
	// 100 completion tokens over 2s generation after 8s TTFT in a 10s request.
	got := ComputeTokensPerSecond(80, 20, 10_000, 8_000)
	if got < 49.9 || got > 50.1 {
		t.Fatalf("tps = %v, want ~50", got)
	}
	if ComputeTokensPerSecond(0, 0, 1000, 100) != 0 {
		t.Fatal("no completion tokens must yield 0")
	}
	// Missing TTFT: use full duration.
	got = ComputeTokensPerSecond(100, 0, 2000, 0)
	if got < 49.9 || got > 50.1 {
		t.Fatalf("full-duration tps = %v, want ~50", got)
	}
}

func TestIsSuccessful(t *testing.T) {
	if !IsSuccessful(200, "") {
		t.Fatal("200 empty error should succeed")
	}
	if IsSuccessful(200, "stream_closed") {
		t.Fatal("soft failure must not count as success")
	}
	if IsSuccessful(500, "") {
		t.Fatal("5xx must not succeed")
	}
}
