package main

import (
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// TestVersionCheckBaseURLPrefersStagingOverride guards the version gate against
// the regression Codex flagged: under --staging it must check the staging
// backend, preferring COMMENT_IO_STAGING_BASE_URL over the generic
// COMMENT_IO_BASE_URL, rather than short-circuiting on the generic value.
func TestVersionCheckBaseURLPrefersStagingOverride(t *testing.T) {
	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	t.Setenv(commentbus.BaseURLEnvVar, "https://generic.example")
	t.Setenv(commentbus.StagingBaseURLEnvVar, "https://staging.example")
	if got := versionCheckBaseURL(); got != "https://staging.example" {
		t.Fatalf("versionCheckBaseURL() under staging = %q, want https://staging.example", got)
	}
}

// TestVersionCheckBaseURLProductionUsesGenericOverride confirms production still
// honors COMMENT_IO_BASE_URL and ignores the staging-only override.
func TestVersionCheckBaseURLProductionUsesGenericOverride(t *testing.T) {
	t.Setenv(commentbus.EnvVar, commentbus.EnvProduction)
	t.Setenv(commentbus.StagingBaseURLEnvVar, "https://staging.example")
	t.Setenv(commentbus.BaseURLEnvVar, "https://generic.example")
	if got := versionCheckBaseURL(); got != "https://generic.example" {
		t.Fatalf("versionCheckBaseURL() in production = %q, want https://generic.example", got)
	}
}
