package updatecheck

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestCheckAlwaysReportsUpToDateWithoutRemote(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("official release check must not be performed")
		return nil, nil
	})}
	service := NewService("dev", client)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	snapshot := service.Check(context.Background())
	if snapshot.Status != StatusUpToDate || snapshot.UpdateAvailable || snapshot.CurrentVersion != "dev" || snapshot.LatestVersion != "dev" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.CheckedAt == nil || !snapshot.CheckedAt.Equal(now) || snapshot.Error != "" {
		t.Fatalf("checked snapshot = %#v", snapshot)
	}
}

func TestNewServiceDefaultsToUpToDate(t *testing.T) {
	service := NewService("dev", nil)
	snapshot := service.Snapshot()
	if snapshot.Status != StatusUpToDate || snapshot.UpdateAvailable || snapshot.LatestVersion != "dev" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestSemanticVersionComparison(t *testing.T) {
	stable, ok := parseSemanticVersion("v3.0.1")
	if !ok {
		t.Fatal("stable version was rejected")
	}
	older, _ := parseSemanticVersion("3.0.0")
	prerelease, _ := parseSemanticVersion("v3.0.1-rc.1")
	if compareSemanticVersion(stable, older) <= 0 || compareSemanticVersion(prerelease, stable) >= 0 {
		t.Fatal("semantic version ordering is invalid")
	}
	if _, ok := parseSemanticVersion("dev"); ok {
		t.Fatal("development version was accepted as semver")
	}
	base, _ := parseSemanticVersion("v3.0.8")
	hotfix1, _ := parseSemanticVersion("v3.0.8-hotfix.1")
	hotfix2, _ := parseSemanticVersion("v3.0.8-hotfix.2")
	sameBasePrerelease, _ := parseSemanticVersion("v3.0.8-rc.1")
	next, _ := parseSemanticVersion("v3.0.9")
	if compareSemanticVersion(hotfix1, base) <= 0 || compareSemanticVersion(hotfix2, hotfix1) <= 0 || compareSemanticVersion(next, hotfix2) <= 0 {
		t.Fatal("project hotfix ordering is invalid")
	}
	if compareSemanticVersion(hotfix1, sameBasePrerelease) <= 0 {
		t.Fatal("project hotfix should follow ordinary prereleases")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
