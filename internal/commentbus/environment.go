package commentbus

import (
	"os"
	"path/filepath"
	"strings"
)

// Environment identifies which Comment.io deployment a CLI invocation targets.
//
// It is resolved once at process entry (see applyEnvironment in the comment
// command) and published through the COMMENT_IO_ENV variable so the daemon,
// managed runtimes, and every default resolver in this package agree on the
// same environment. Production is the default; staging is opt-in and uses a
// fully separate on-disk root, API endpoint, and synced-docs folder so the two
// can coexist on one machine without clobbering each other.
type Environment struct {
	Name string
}

const (
	// EnvProduction and EnvStaging are the two supported environment names.
	EnvProduction = "production"
	EnvStaging    = "staging"

	// EnvVar is the canonical environment selector. The CLI entry point sets
	// it from --staging/--production, the invoked binary name, or an inherited
	// value; everything downstream reads it via CurrentEnvironment.
	EnvVar = "COMMENT_IO_ENV"

	// BaseURLEnvVar overrides the default base URL for both environments.
	BaseURLEnvVar = "COMMENT_IO_BASE_URL"

	// StagingBaseURLEnvVar overrides the staging default base URL only.
	StagingBaseURLEnvVar = "COMMENT_IO_STAGING_BASE_URL"

	productionHomeDirName = ".comment-io"
	stagingHomeDirName    = ".comment-io-staging"

	productionSyncRootName = "Comment Docs"
	stagingSyncRootName    = "Comment Docs (staging)"

	// stagingBaseURL is the shared staging deployment, built from main.
	stagingBaseURL = "https://comt.dev"
)

// CurrentEnvironment resolves the active environment from COMMENT_IO_ENV.
// Anything other than an explicit, case-insensitive "staging" is production.
func CurrentEnvironment() Environment {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(EnvVar)), EnvStaging) {
		return Environment{Name: EnvStaging}
	}
	return Environment{Name: EnvProduction}
}

// IsStaging reports whether this is the staging environment.
func (e Environment) IsStaging() bool { return e.Name == EnvStaging }

// DefaultHomeDir returns the default local state root for the environment:
// ~/.comment-io for production, ~/.comment-io-staging for staging. An explicit
// --home flag or COMMENT_IO_HOME still overrides this default.
func (e Environment) DefaultHomeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	name := productionHomeDirName
	if e.IsStaging() {
		name = stagingHomeDirName
	}
	return filepath.Join(home, name), nil
}

// DefaultBaseURL returns the default API base URL for the environment, honoring
// the COMMENT_IO_BASE_URL (both environments) and COMMENT_IO_STAGING_BASE_URL
// (staging only) overrides. A profile's own base_url still takes precedence over
// this default when one is configured.
func (e Environment) DefaultBaseURL() string {
	if e.IsStaging() {
		if v := normalizeBaseURL(os.Getenv(StagingBaseURLEnvVar)); v != "" {
			return v
		}
		if v := normalizeBaseURL(os.Getenv(BaseURLEnvVar)); v != "" {
			return v
		}
		return stagingBaseURL
	}
	if v := normalizeBaseURL(os.Getenv(BaseURLEnvVar)); v != "" {
		return v
	}
	return defaultCommentBaseURL
}

// DefaultSyncRootName returns the user-facing local docs folder name for the
// environment. Staging uses a separate folder so production projections under
// ~/Comment Docs are never clobbered.
func (e Environment) DefaultSyncRootName() string {
	if e.IsStaging() {
		return stagingSyncRootName
	}
	return productionSyncRootName
}

// StagingServiceBaseURLOverride returns the explicit base-URL override to bake
// into a staging daemon's service definition, or "" when none is set or this is
// production. The installed daemon does not inherit the installing shell's
// environment, so without this an override-configured staging install would
// silently fall back to the compiled-in staging default after a restart.
// Production service files are intentionally left untouched.
func StagingServiceBaseURLOverride() string {
	if !CurrentEnvironment().IsStaging() {
		return ""
	}
	if v := strings.TrimSpace(os.Getenv(StagingBaseURLEnvVar)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(BaseURLEnvVar)); v != "" {
		return v
	}
	return ""
}

// RuntimeEnvironmentVars returns the environment entries ("KEY=VALUE") that a
// daemon-launched managed runtime needs so it resolves the same environment as
// the daemon that launched it. The managed-session and tmux launch paths scrub
// the environment down to a safe allowlist, which would otherwise strip
// COMMENT_IO_ENV and let a staging daemon spawn production runtimes (e.g. the
// runtime's own `comment sync login` would write to the production ~/Comment
// Docs root). It is derived from the active, daemon-resolved environment — never
// from a runtime-supplied value — and returns nil for production so production
// runtimes keep their environment unchanged.
func RuntimeEnvironmentVars() []string {
	env := CurrentEnvironment()
	if !env.IsStaging() {
		return nil
	}
	out := []string{EnvVar + "=" + env.Name}
	if v := strings.TrimSpace(os.Getenv(StagingBaseURLEnvVar)); v != "" {
		out = append(out, StagingBaseURLEnvVar+"="+v)
	}
	if v := strings.TrimSpace(os.Getenv(BaseURLEnvVar)); v != "" {
		out = append(out, BaseURLEnvVar+"="+v)
	}
	return out
}
