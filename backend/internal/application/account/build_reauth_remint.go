package account

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

const (
	buildReauthRemintMaxAttempts   = 5
	buildReauthRemintTimeout       = 2 * time.Minute
	buildReauthRemintSweepInterval = 10 * time.Minute
	buildReauthRemintIdlePoll      = 3 * time.Second
	buildReauthRemintSweepPageSize = 50
	buildReauthRemintLockTTL       = 3 * time.Minute
)

// buildReauthRemintState tracks auto Web→Build remint after Build reauth.
// Transient failures (network/timeout) retry with backoff; permanent upstream
// failures stop without thrashing. Success is verified by reloading Build auth.
type buildReauthRemintState struct {
	attempts  int
	nextAt    time.Time
	permanent bool
	lastError string
}

func shouldScheduleBuildReauthRemint(value accountdomain.Credential, reason string) bool {
	if value.Provider != accountdomain.ProviderBuild {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(reason))
	if text == "" {
		return false
	}
	return strings.Contains(text, "invalid_grant") ||
		strings.Contains(text, "oauth refresh") ||
		strings.Contains(text, "永久失效")
}

func (s *Service) scheduleBuildReauthRemint(buildID uint64) {
	if buildID == 0 {
		return
	}
	s.remintMu.Lock()
	defer s.remintMu.Unlock()
	if s.remintStates == nil {
		s.remintStates = make(map[uint64]buildReauthRemintState)
	}
	state, exists := s.remintStates[buildID]
	if exists && state.permanent {
		return
	}
	if !exists {
		s.remintStates[buildID] = buildReauthRemintState{nextAt: s.now()}
	} else if state.nextAt.After(s.now()) {
		// keep deferred schedule
	} else {
		state.nextAt = s.now()
		s.remintStates[buildID] = state
	}
	select {
	case s.remintWake <- struct{}{}:
	default:
	}
}

// RunBuildReauthRemint recovers Build accounts marked reauthRequired by reminting
// OAuth from the linked Web SSO (strategy=all). Network/transient errors retry;
// permanent grant/SSO failures stop and rely on existing blocked/reauth marks.
func (s *Service) RunBuildReauthRemint(ctx context.Context) {
	// Initial sweep so process restarts still pick up already-reauth builds.
	s.sweepBuildReauthRemintCandidates(ctx)
	timer := time.NewTimer(buildReauthRemintIdlePoll)
	defer timer.Stop()
	sweepTimer := time.NewTimer(buildReauthRemintSweepInterval)
	defer sweepTimer.Stop()
	for {
		s.processDueBuildReauthRemints(ctx)
		delay := s.nextBuildReauthRemintDelay()
		if delay <= 0 {
			delay = buildReauthRemintIdlePoll
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case <-s.remintWake:
		case <-timer.C:
		case <-sweepTimer.C:
			s.sweepBuildReauthRemintCandidates(ctx)
			sweepTimer.Reset(buildReauthRemintSweepInterval)
		}
	}
}

func (s *Service) nextBuildReauthRemintDelay() time.Duration {
	now := s.now()
	s.remintMu.Lock()
	defer s.remintMu.Unlock()
	var soonest time.Time
	for id, state := range s.remintStates {
		if state.permanent {
			continue
		}
		if _, running := s.remintRunning[id]; running {
			continue
		}
		if soonest.IsZero() || state.nextAt.Before(soonest) {
			soonest = state.nextAt
		}
	}
	if soonest.IsZero() {
		return buildReauthRemintIdlePoll
	}
	if !soonest.After(now) {
		return time.Millisecond
	}
	delay := soonest.Sub(now)
	if delay > buildReauthRemintIdlePoll {
		return buildReauthRemintIdlePoll
	}
	return delay
}

func (s *Service) processDueBuildReauthRemints(ctx context.Context) {
	now := s.now()
	s.remintMu.Lock()
	due := make([]uint64, 0)
	for id, state := range s.remintStates {
		if state.permanent {
			continue
		}
		if _, running := s.remintRunning[id]; running {
			continue
		}
		if state.nextAt.After(now) {
			continue
		}
		due = append(due, id)
		s.remintRunning[id] = struct{}{}
	}
	s.remintMu.Unlock()
	for _, buildID := range due {
		if ctx.Err() != nil {
			s.finishBuildReauthRemintRun(buildID)
			continue
		}
		s.runBuildReauthRemintOnce(ctx, buildID)
		s.finishBuildReauthRemintRun(buildID)
	}
}

func (s *Service) finishBuildReauthRemintRun(buildID uint64) {
	s.remintMu.Lock()
	delete(s.remintRunning, buildID)
	s.remintMu.Unlock()
}

func (s *Service) sweepBuildReauthRemintCandidates(ctx context.Context) {
	if s.accounts == nil || ctx.Err() != nil {
		return
	}
	offset := 0
	for {
		values, total, err := s.accounts.List(ctx, repository.AccountListQuery{
			Page: repository.PageQuery{Offset: offset, Limit: buildReauthRemintSweepPageSize},
			Filter: repository.AccountListFilter{
				Provider: string(accountdomain.ProviderBuild),
				Status:   "reauthRequired",
				Now:      s.now(),
			},
		})
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("build_reauth_remint_sweep_failed", "error", err)
			}
			return
		}
		for _, value := range values {
			if value.LinkedAccountID == 0 || value.LinkedProvider != accountdomain.ProviderWeb {
				continue
			}
			// Any linked Build reauth is a remint candidate (OAuth permanent path or legacy rows).
			s.scheduleBuildReauthRemint(value.ID)
		}
		offset += len(values)
		if len(values) == 0 || int64(offset) >= total {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *Service) runBuildReauthRemintOnce(ctx context.Context, buildID uint64) {
	taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildReauthRemintTimeout)
	defer cancel()

	build, err := s.accounts.Get(taskCtx, buildID)
	if err != nil {
		s.deferBuildReauthRemint(buildID, err, false)
		return
	}
	if build.Provider != accountdomain.ProviderBuild {
		s.clearBuildReauthRemint(buildID)
		return
	}
	if build.AuthStatus != accountdomain.AuthStatusReauthRequired {
		// Already recovered (manual remint / concurrent success).
		s.logger.Info("build_reauth_remint_skipped", "build_id", buildID, "reason", "already_active")
		s.clearBuildReauthRemint(buildID)
		return
	}
	webID := build.LinkedAccountID
	if webID == 0 || build.LinkedProvider != accountdomain.ProviderWeb {
		s.logger.Info("build_reauth_remint_skipped", "build_id", buildID, "reason", "no_linked_web")
		s.markBuildReauthRemintPermanent(buildID, "no linked web account")
		return
	}
	web, err := s.accounts.Get(taskCtx, webID)
	if err != nil {
		s.deferBuildReauthRemint(buildID, err, false)
		return
	}
	if web.Provider != accountdomain.ProviderWeb || web.AuthType != accountdomain.AuthTypeSSO {
		s.markBuildReauthRemintPermanent(buildID, "linked account is not web sso")
		return
	}
	if !web.Enabled || web.AuthStatus != accountdomain.AuthStatusActive {
		s.logger.Info("build_reauth_remint_skipped", "build_id", buildID, "web_id", webID, "reason", "web_not_active")
		s.markBuildReauthRemintPermanent(buildID, "linked web is not active")
		return
	}
	if web.BuildConvertBlockedAt != nil {
		s.logger.Info("build_reauth_remint_skipped", "build_id", buildID, "web_id", webID, "reason", "web_convert_blocked", "blocked_reason", web.BuildConvertBlockedReason)
		s.markBuildReauthRemintPermanent(buildID, "web convert blocked: "+web.BuildConvertBlockedReason)
		return
	}
	if strings.TrimSpace(web.EncryptedAccessToken) == "" {
		s.markBuildReauthRemintPermanent(buildID, "linked web has empty sso")
		return
	}

	lockKey := "build-reauth-remint:" + strconv.FormatUint(webID, 10)
	release, acquired, lockErr := s.refreshLock.Acquire(taskCtx, lockKey, buildReauthRemintLockTTL)
	if lockErr != nil {
		s.deferBuildReauthRemint(buildID, lockErr, false)
		return
	}
	if !acquired {
		s.deferBuildReauthRemint(buildID, ErrConversionBusy, false)
		return
	}
	defer release()

	s.logger.Info("build_reauth_remint_start", "build_id", buildID, "web_id", webID)
	_, _, _, convertErr := s.convertWebAccountToBuild(taskCtx, webID, BuildConversionAll, false)

	// Always re-check Build: mint may have succeeded even if a later step/network race erred.
	refreshed, getErr := s.accounts.Get(context.WithoutCancel(taskCtx), buildID)
	if getErr == nil && refreshed.AuthStatus == accountdomain.AuthStatusActive {
		s.logger.Info("build_reauth_remint_succeeded", "build_id", buildID, "web_id", webID, "convert_err", errorString(convertErr))
		s.clearBuildReauthRemint(buildID)
		return
	}
	if convertErr == nil {
		// Convert reported success but build not active — treat as transient and recheck later.
		s.deferBuildReauthRemint(buildID, errors.New("convert reported success but build still reauth"), false)
		return
	}
	permanent := isPermanentBuildRemintError(convertErr)
	s.logger.Warn("build_reauth_remint_failed",
		"build_id", buildID,
		"web_id", webID,
		"permanent", permanent,
		"error", convertErr,
	)
	s.deferBuildReauthRemint(buildID, convertErr, permanent)
}

func (s *Service) clearBuildReauthRemint(buildID uint64) {
	s.remintMu.Lock()
	delete(s.remintStates, buildID)
	s.remintMu.Unlock()
}

func (s *Service) markBuildReauthRemintPermanent(buildID uint64, reason string) {
	s.remintMu.Lock()
	defer s.remintMu.Unlock()
	state := s.remintStates[buildID]
	state.permanent = true
	state.lastError = trimRemintError(reason)
	s.remintStates[buildID] = state
}

func (s *Service) deferBuildReauthRemint(buildID uint64, err error, permanent bool) {
	s.remintMu.Lock()
	defer s.remintMu.Unlock()
	state := s.remintStates[buildID]
	state.attempts++
	state.lastError = trimRemintError(errorString(err))
	if permanent || state.attempts >= buildReauthRemintMaxAttempts {
		state.permanent = true
		if !permanent && state.attempts >= buildReauthRemintMaxAttempts {
			// Exhausted transient retries: leave Build reauth; do NOT mark web blocked.
			state.lastError = "transient remint exhausted: " + state.lastError
			s.logger.Warn("build_reauth_remint_give_up", "build_id", buildID, "attempts", state.attempts, "error", state.lastError)
		}
		s.remintStates[buildID] = state
		return
	}
	state.nextAt = s.now().Add(buildReauthRemintBackoff(state.attempts))
	s.remintStates[buildID] = state
}

func buildReauthRemintBackoff(attempt int) time.Duration {
	// attempt is 1-based after increment.
	delays := [...]time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		time.Hour,
	}
	index := max(0, min(attempt-1, len(delays)-1))
	return delays[index]
}

// isPermanentBuildRemintError classifies remint failures that should not be retried.
// Network / timeout / busy stay transient so a flaky path does not poison a good Web SSO.
func isPermanentBuildRemintError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		// Parent shutdown — do not burn permanent budget.
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrConversionBusy) {
		return false
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return true
	}
	if errors.Is(err, provider.ErrInvalidGrant) {
		return true
	}
	if isBuildConvertInvalidGrant(err) {
		return true
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "invalid_grant"):
		return true
	case strings.Contains(text, "sso credential rejected"):
		return true
	case strings.Contains(text, "仅 grok web sso"):
		return true
	case strings.Contains(text, "credential rejected"):
		return true
	case strings.Contains(text, "timeout"):
		return false
	case strings.Contains(text, "temporar"):
		return false
	case strings.Contains(text, "connection"):
		return false
	case strings.Contains(text, "network"):
		return false
	case strings.Contains(text, "proxy"):
		return false
	case strings.Contains(text, "eof"):
		return false
	case strings.Contains(text, "reset by peer"):
		return false
	case strings.Contains(text, "http 5"):
		return false
	case strings.Contains(text, "http 429"):
		return false
	case strings.Contains(text, "protocol mint"):
		// Mint fallback path often wraps portal 403 + mint transport issues; retry.
		return false
	default:
		// Unknown errors: retry a few times rather than permanently block.
		return false
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func trimRemintError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 300 {
		return value[:300]
	}
	return value
}
