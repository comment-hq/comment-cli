package commentbus

import (
	"strings"
	"testing"
)

func TestCurrentEnvironmentDefaultsToProduction(t *testing.T) {
	t.Setenv(EnvVar, "")
	if env := CurrentEnvironment(); env.IsStaging() || env.Name != EnvProduction {
		t.Fatalf("expected production, got %+v", env)
	}
}

func TestCurrentEnvironmentStagingCaseInsensitive(t *testing.T) {
	for _, value := range []string{"staging", "STAGING", " Staging "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(EnvVar, value)
			if env := CurrentEnvironment(); !env.IsStaging() {
				t.Fatalf("expected staging for %q, got %+v", value, env)
			}
		})
	}
}

func TestCurrentEnvironmentUnknownValueIsProduction(t *testing.T) {
	t.Setenv(EnvVar, "preview")
	if env := CurrentEnvironment(); env.IsStaging() {
		t.Fatalf("expected production for unknown value, got %+v", env)
	}
}

func TestEnvironmentDefaultHomeDir(t *testing.T) {
	prod, err := Environment{Name: EnvProduction}.DefaultHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(prod, "/.comment-io") {
		t.Fatalf("production home = %q, want suffix /.comment-io", prod)
	}
	staging, err := Environment{Name: EnvStaging}.DefaultHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(staging, "/.comment-io-staging") {
		t.Fatalf("staging home = %q, want suffix /.comment-io-staging", staging)
	}
	if prod == staging {
		t.Fatalf("production and staging home dirs must differ, both %q", prod)
	}
}

func TestEnvironmentDefaultSyncRootName(t *testing.T) {
	if got := (Environment{Name: EnvProduction}).DefaultSyncRootName(); got != "Comment Docs" {
		t.Fatalf("production sync root = %q", got)
	}
	if got := (Environment{Name: EnvStaging}).DefaultSyncRootName(); got != "Comment Docs (staging)" {
		t.Fatalf("staging sync root = %q", got)
	}
}

func TestEnvironmentDefaultBaseURL(t *testing.T) {
	tests := []struct {
		name       string
		env        Environment
		baseURL    string
		stagingURL string
		want       string
	}{
		{"production default", Environment{Name: EnvProduction}, "", "", "https://comment.io"},
		{"production override", Environment{Name: EnvProduction}, "https://comment.example/", "", "https://comment.example"},
		{"staging default", Environment{Name: EnvStaging}, "", "", "https://comt.dev"},
		{"staging staging-override", Environment{Name: EnvStaging}, "", "https://example.comt.dev/", "https://example.comt.dev"},
		{"staging base-url-override", Environment{Name: EnvStaging}, "https://shared.comt.dev", "", "https://shared.comt.dev"},
		{"staging prefers staging-override", Environment{Name: EnvStaging}, "https://base.example", "https://staging.example", "https://staging.example"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(BaseURLEnvVar, tc.baseURL)
			t.Setenv(StagingBaseURLEnvVar, tc.stagingURL)
			if got := tc.env.DefaultBaseURL(); got != tc.want {
				t.Fatalf("DefaultBaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveDefaultBaseURLHonorsStagingEnvironment guards the profile-loading
// fallback path: when no explicit base URL is supplied, the resolver must use
// the active environment's default rather than the hardcoded production URL.
func TestResolveDefaultBaseURLHonorsStagingEnvironment(t *testing.T) {
	t.Setenv(EnvVar, EnvStaging)
	t.Setenv(BaseURLEnvVar, "")
	t.Setenv(StagingBaseURLEnvVar, "")
	if got := resolveDefaultBaseURL(""); got != "https://comt.dev" {
		t.Fatalf("resolveDefaultBaseURL(\"\") under staging = %q, want https://comt.dev", got)
	}
	if got := resolveDefaultBaseURL("https://explicit.example"); got != "https://explicit.example" {
		t.Fatalf("resolveDefaultBaseURL with explicit value = %q, want it unchanged", got)
	}
}
