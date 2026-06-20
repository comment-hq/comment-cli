package commentsync

import (
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestResolveRootUsesEnvironmentSyncFolder(t *testing.T) {
	t.Setenv(commentbus.EnvVar, "")
	prod, err := resolveRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(prod, "/Comment Docs") {
		t.Fatalf("production sync root = %q, want suffix /Comment Docs", prod)
	}

	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	staging, err := resolveRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(staging, "/Comment Docs (staging)") {
		t.Fatalf("staging sync root = %q, want suffix /Comment Docs (staging)", staging)
	}

	if prod == staging {
		t.Fatalf("production and staging sync roots must differ, both %q", prod)
	}
}

func TestDefaultBaseURLFollowsEnvironment(t *testing.T) {
	t.Setenv(commentbus.EnvVar, "")
	t.Setenv(commentbus.BaseURLEnvVar, "")
	t.Setenv(commentbus.StagingBaseURLEnvVar, "")
	if got := DefaultBaseURL(); got != "https://comment.io" {
		t.Fatalf("production DefaultBaseURL = %q", got)
	}
	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	if got := DefaultBaseURL(); got != "https://comt.dev" {
		t.Fatalf("staging DefaultBaseURL = %q", got)
	}
}
