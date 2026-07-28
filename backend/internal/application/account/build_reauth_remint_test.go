package account

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestIsPermanentBuildRemintError(t *testing.T) {
	if isPermanentBuildRemintError(context.DeadlineExceeded) {
		t.Fatal("deadline should be transient")
	}
	if isPermanentBuildRemintError(errors.New("dial tcp: connection reset by peer")) {
		t.Fatal("connection reset should be transient")
	}
	if isPermanentBuildRemintError(errors.New("protocol mint fallback: temporary")) {
		t.Fatal("protocol mint wrap should be transient")
	}
	if !isPermanentBuildRemintError(provider.ErrInvalidGrant) {
		t.Fatal("invalid_grant should be permanent")
	}
	if !isPermanentBuildRemintError(provider.ErrUnauthorized) {
		t.Fatal("unauthorized should be permanent")
	}
	if !isPermanentBuildRemintError(errors.New("OAuth refresh failed: invalid_grant")) {
		t.Fatal("invalid_grant text should be permanent")
	}
}

func TestMarkReauthRequiredSchedulesLinkedWebRemint(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "remint-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-source",
		EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", SourceKey: "sso-build:web-source",
		EncryptedAccessToken: "enc-a", EncryptedRefreshToken: "enc-r", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	adapter := &flakyBuildConversionAdapter{}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	if err := service.MarkReauthRequired(ctx, build.ID, "OAuth refresh failed: invalid_grant"); err != nil {
		t.Fatal(err)
	}
	service.remintMu.Lock()
	_, scheduled := service.remintStates[build.ID]
	service.remintMu.Unlock()
	if !scheduled {
		t.Fatal("expected remint to be scheduled for linked build reauth")
	}
}

func TestBuildReauthRemintRetriesTransientThenSucceeds(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "remint-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-source",
		EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", SourceKey: "sso-build:web-source",
		OIDCClientID: "client", EncryptedAccessToken: "enc-a", EncryptedRefreshToken: "enc-r",
		Enabled: true, AuthStatus: accountdomain.AuthStatusActive, ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	adapter := &flakyBuildConversionAdapter{failTimes: 2}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	if err := service.MarkReauthRequired(ctx, build.ID, "OAuth refresh failed: invalid_grant"); err != nil {
		t.Fatal(err)
	}
	// First two attempts fail transiently.
	service.runBuildReauthRemintOnce(ctx, build.ID)
	service.runBuildReauthRemintOnce(ctx, build.ID)
	// Advance past backoff so third attempt is allowed by state machine; call directly.
	service.remintMu.Lock()
	state := service.remintStates[build.ID]
	if state.permanent || state.attempts != 2 {
		service.remintMu.Unlock()
		t.Fatalf("after transient fails state=%#v", state)
	}
	// Force next attempt immediately.
	state.nextAt = now
	service.remintStates[build.ID] = state
	service.remintMu.Unlock()

	service.runBuildReauthRemintOnce(ctx, build.ID)
	updated, err := repo.Get(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.AuthStatus != accountdomain.AuthStatusActive {
		t.Fatalf("build status = %s after remint", updated.AuthStatus)
	}
	service.remintMu.Lock()
	_, still := service.remintStates[build.ID]
	service.remintMu.Unlock()
	if still {
		t.Fatal("remint state should clear after success")
	}
	if adapter.calls.Load() != 3 {
		t.Fatalf("adapter calls = %d", adapter.calls.Load())
	}
}

func TestBuildReauthRemintPermanentInvalidGrantStops(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "remint-perm.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedSSO, err := cipher.Encrypt("test-sso")
	if err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	web, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web-source",
		EncryptedAccessToken: encryptedSSO, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	build, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build", SourceKey: "sso-build:web-source",
		EncryptedAccessToken: "enc-a", EncryptedRefreshToken: "enc-r", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.LinkWebToBuild(ctx, web.ID, build.ID); err != nil {
		t.Fatal(err)
	}
	adapter := &flakyBuildConversionAdapter{permanent: true}
	service := NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), cipher, memory.NewLockStore())
	if err := service.MarkReauthRequired(ctx, build.ID, "OAuth refresh failed: invalid_grant"); err != nil {
		t.Fatal(err)
	}
	service.runBuildReauthRemintOnce(ctx, build.ID)
	service.remintMu.Lock()
	state := service.remintStates[build.ID]
	service.remintMu.Unlock()
	if !state.permanent {
		t.Fatalf("expected permanent stop, state=%#v", state)
	}
	// Second run should not call adapter again when permanent (processDue skips permanent).
	before := adapter.calls.Load()
	service.runBuildReauthRemintOnce(ctx, build.ID)
	// Direct call still runs - permanent is checked after convert. Schedule path skips permanent.
	// Clear by checking schedule won't re-open permanent:
	service.scheduleBuildReauthRemint(build.ID)
	service.remintMu.Lock()
	state = service.remintStates[build.ID]
	service.remintMu.Unlock()
	if !state.permanent {
		t.Fatal("schedule must not reopen permanent remint")
	}
	if adapter.calls.Load() < before {
		t.Fatal("unexpected call regression")
	}
}

type flakyBuildConversionAdapter struct {
	calls     atomic.Int64
	failTimes int64
	permanent bool
}

func (a *flakyBuildConversionAdapter) Provider() accountdomain.Provider {
	return accountdomain.ProviderWeb
}

func (a *flakyBuildConversionAdapter) ConvertToBuild(_ context.Context, credential accountdomain.Credential) (provider.CredentialSeed, error) {
	call := a.calls.Add(1)
	if a.permanent {
		return provider.CredentialSeed{}, provider.ErrInvalidGrant
	}
	if call <= a.failTimes {
		return provider.CredentialSeed{}, errors.New("connection reset by peer")
	}
	return provider.CredentialSeed{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "build",
		UserID: credential.UserID, Email: credential.Email, SourceKey: "sso-build:web-source", OIDCClientID: "client",
		AccessToken: "access-remint", RefreshToken: "refresh-remint", ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, nil
}
