package main

import (
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestStagingServiceEnvironment(t *testing.T) {
	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	if got := stagingServiceEnvironment(); got != commentbus.EnvStaging {
		t.Fatalf("stagingServiceEnvironment() under staging = %q, want %q", got, commentbus.EnvStaging)
	}
	t.Setenv(commentbus.EnvVar, "")
	if got := stagingServiceEnvironment(); got != "" {
		t.Fatalf("stagingServiceEnvironment() under production = %q, want empty", got)
	}
}

func TestBuildLaunchAgentPlistBakesStagingEnvironment(t *testing.T) {
	staging := buildLaunchAgentPlist(launchAgentConfig{
		Label:       "io.comment.commentd.staging",
		Home:        "/home/user/.comment-io-staging",
		Environment: commentbus.EnvStaging,
	})
	if !strings.Contains(staging, "<key>COMMENT_IO_ENV</key>") || !strings.Contains(staging, "<string>staging</string>") {
		t.Fatalf("staging plist missing COMMENT_IO_ENV:\n%s", staging)
	}

	production := buildLaunchAgentPlist(launchAgentConfig{
		Label: "io.comment.commentd.prod",
		Home:  "/home/user/.comment-io",
	})
	if strings.Contains(production, "COMMENT_IO_ENV") {
		t.Fatalf("production plist must not mention COMMENT_IO_ENV:\n%s", production)
	}
}

func TestBuildSystemdUnitBakesStagingEnvironment(t *testing.T) {
	staging := buildSystemdUnit(systemdServiceConfig{
		Label:       "io.comment.commentd.staging",
		UnitName:    "io.comment.commentd.staging.service",
		Home:        "/home/user/.comment-io-staging",
		Environment: commentbus.EnvStaging,
		BinaryPath:  "/usr/local/bin/comment",
		StdoutPath:  "/home/user/.comment-io-staging/logs/commentd.out.log",
		StderrPath:  "/home/user/.comment-io-staging/logs/commentd.err.log",
		ProgramArguments: []string{
			"/usr/local/bin/comment", "bus", "run", "--home", "/home/user/.comment-io-staging",
		},
	})
	if !strings.Contains(staging, "Environment=\"COMMENT_IO_ENV=staging\"") {
		t.Fatalf("staging systemd unit missing COMMENT_IO_ENV:\n%s", staging)
	}

	production := buildSystemdUnit(systemdServiceConfig{
		Label:      "io.comment.commentd.prod",
		UnitName:   "io.comment.commentd.prod.service",
		Home:       "/home/user/.comment-io",
		BinaryPath: "/usr/local/bin/comment",
		StdoutPath: "/home/user/.comment-io/logs/commentd.out.log",
		StderrPath: "/home/user/.comment-io/logs/commentd.err.log",
		ProgramArguments: []string{
			"/usr/local/bin/comment", "bus", "run", "--home", "/home/user/.comment-io",
		},
	})
	if strings.Contains(production, "COMMENT_IO_ENV") {
		t.Fatalf("production systemd unit must not mention COMMENT_IO_ENV:\n%s", production)
	}
}
