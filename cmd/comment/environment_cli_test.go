package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestApplyEnvironment(t *testing.T) {
	tests := []struct {
		name         string
		argv0        string
		args         []string
		commentEnv   string // COMMENT_ENV value before the call
		wantEnv      string
		wantFiltered []string
	}{
		{
			name:         "default is production",
			argv0:        "/usr/local/bin/comment",
			args:         []string{"bus", "status"},
			wantEnv:      commentbus.EnvProduction,
			wantFiltered: []string{"bus", "status"},
		},
		{
			name:         "leading staging flag selects staging and is stripped",
			argv0:        "comment",
			args:         []string{"--staging", "bus", "run"},
			wantEnv:      commentbus.EnvStaging,
			wantFiltered: []string{"bus", "run"},
		},
		{
			name:         "production flag overrides inherited staging env",
			argv0:        "comment",
			args:         []string{"--production", "sync", "status"},
			commentEnv:   commentbus.EnvStaging,
			wantEnv:      commentbus.EnvProduction,
			wantFiltered: []string{"sync", "status"},
		},
		{
			name:         "comment-staging binary name selects staging",
			argv0:        "/opt/homebrew/bin/comment-staging",
			args:         []string{"bus", "status"},
			wantEnv:      commentbus.EnvStaging,
			wantFiltered: []string{"bus", "status"},
		},
		{
			name:         "comment-staging substring binary does not select staging",
			argv0:        "/opt/homebrew/bin/comment-staging-helper",
			args:         []string{"bus", "status"},
			wantEnv:      commentbus.EnvProduction,
			wantFiltered: []string{"bus", "status"},
		},
		{
			name:         "COMMENT_ENV fallback",
			argv0:        "comment",
			args:         []string{"doctor"},
			commentEnv:   "staging",
			wantEnv:      commentbus.EnvStaging,
			wantFiltered: []string{"doctor"},
		},
		{
			name:         "a flag value named like an env flag is preserved",
			argv0:        "comment",
			args:         []string{"messages", "send", "--body", "--staging", "--to", "alice.bot"},
			wantEnv:      commentbus.EnvProduction,
			wantFiltered: []string{"messages", "send", "--body", "--staging", "--to", "alice.bot"},
		},
		{
			name:         "env flag after the subcommand is left for the subcommand parser",
			argv0:        "comment",
			args:         []string{"bus", "run", "--staging"},
			wantEnv:      commentbus.EnvProduction,
			wantFiltered: []string{"bus", "run", "--staging"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(commentbus.EnvVar, "")
			t.Setenv("COMMENT_ENV", tc.commentEnv)

			filtered, err := applyEnvironment(tc.argv0, tc.args)
			if err != nil {
				t.Fatalf("applyEnvironment returned error: %v", err)
			}
			if got := os.Getenv(commentbus.EnvVar); got != tc.wantEnv {
				t.Fatalf("COMMENT_IO_ENV = %q, want %q", got, tc.wantEnv)
			}
			if !reflect.DeepEqual(filtered, tc.wantFiltered) {
				t.Fatalf("filtered args = %#v, want %#v", filtered, tc.wantFiltered)
			}
		})
	}
}

func TestApplyEnvironmentRejectsConflictingFlags(t *testing.T) {
	t.Setenv(commentbus.EnvVar, "")
	if _, err := applyEnvironment("comment", []string{"--staging", "--production"}); err == nil {
		t.Fatal("expected an error when both --staging and --production are passed")
	}
}
