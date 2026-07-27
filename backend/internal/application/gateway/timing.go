package gateway

import (
	"io"
	"log/slog"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// generationTiming 只记录阶段耗时和有限枚举，不保存请求体、凭据或会话键。
type generationTiming struct {
	mu             sync.Mutex
	started        time.Time
	route          string
	provider       accountdomain.Provider
	selectionWait  time.Duration
	credentialWait time.Duration
	upstreamWait   time.Duration
	firstHeaders   time.Duration
	firstBody      time.Duration
	attempts       int
	finished       bool
}

func newGenerationTiming(route string, provider accountdomain.Provider) *generationTiming {
	return &generationTiming{started: time.Now(), route: route, provider: provider}
}

func (t *generationTiming) markSelection(duration time.Duration) {
	t.mu.Lock()
	t.selectionWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markCredential(duration time.Duration) {
	t.mu.Lock()
	t.credentialWait += duration
	t.mu.Unlock()
}

func (t *generationTiming) markUpstream(duration time.Duration) {
	t.mu.Lock()
	t.attempts++
	t.upstreamWait += duration
	if t.firstHeaders == 0 {
		t.firstHeaders = time.Since(t.started)
	}
	t.mu.Unlock()
}

func (t *generationTiming) markFirstBody() {
	t.mu.Lock()
	if t.firstBody == 0 {
		t.firstBody = time.Since(t.started)
	}
	t.mu.Unlock()
}

// snapshot returns stage timings without finishing the metric (safe to call before audit write).
func (t *generationTiming) snapshot() (selection, credential, upstream, firstHeaders, firstBody time.Duration, attempts int) {
	if t == nil {
		return 0, 0, 0, 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.selectionWait, t.credentialWait, t.upstreamWait, t.firstHeaders, t.firstBody, t.attempts
}

// applyGenerationTiming copies gateway stage timings and derived TPS onto an audit record.
func applyGenerationTiming(record *auditdomain.Record, timing *generationTiming) {
	if record == nil {
		return
	}
	if timing != nil {
		selection, credential, upstream, firstHeaders, firstBody, attempts := timing.snapshot()
		record.SelectionMS = selection.Milliseconds()
		record.CredentialMS = credential.Milliseconds()
		record.UpstreamWaitMS = upstream.Milliseconds()
		record.FirstHeadersMS = firstHeaders.Milliseconds()
		record.TTFTMS = firstBody.Milliseconds()
		record.UpstreamAttempts = attempts
	}
	record.TokensPerSecond = auditdomain.ComputeTokensPerSecond(
		record.OutputTokens, record.ReasoningTokens, record.DurationMS, record.TTFTMS,
	)
}

func (t *generationTiming) finish(logger *slog.Logger, outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	total := time.Since(t.started)
	retries := max(0, t.attempts-1)
	fields := []any{
		"route", t.route, "provider", t.provider, "outcome", outcome, "total_ms", total.Milliseconds(),
		"selection_wait_ms", t.selectionWait.Milliseconds(), "credential_wait_ms", t.credentialWait.Milliseconds(),
		"upstream_wait_ms", t.upstreamWait.Milliseconds(), "first_headers_ms", t.firstHeaders.Milliseconds(),
		"first_body_ms", t.firstBody.Milliseconds(), "attempts", t.attempts, "retries", retries,
	}
	selection, credential, upstream := t.selectionWait, t.credentialWait, t.upstreamWait
	attempts := t.attempts
	t.mu.Unlock()
	labels := perfmetrics.Labels{Subsystem: "gateway", Provider: string(t.provider), Outcome: outcome}
	perfmetrics.Default.ObserveDuration("request_duration_us", labels, total)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "selection"), selection)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "credential"), credential)
	perfmetrics.Default.ObserveDuration("stage_duration_us", withTimingStage(labels, "upstream"), upstream)
	perfmetrics.Default.Add("attempt_count", labels, int64(attempts))
	if logger == nil {
		logger = slog.Default()
	}
	// Info: stream TTFT diagnosis depends on seeing selection vs upstream split.
	logger.Info("generation_timing", fields...)
}

func withTimingStage(labels perfmetrics.Labels, stage string) perfmetrics.Labels {
	labels.Stage = stage
	return labels
}

type firstByteReadCloser struct {
	io.ReadCloser
	once sync.Once
	mark func()
}

func (r *firstByteReadCloser) Read(buffer []byte) (int, error) {
	n, err := r.ReadCloser.Read(buffer)
	if n > 0 && r.mark != nil {
		r.once.Do(r.mark)
	}
	return n, err
}
