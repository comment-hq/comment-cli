package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseCLIVersion(t *testing.T) {
	cases := []struct {
		in    string
		want  [3]int
		valid bool
	}{
		{"0.1.8", [3]int{0, 1, 8}, true},
		{"v1.2.3", [3]int{1, 2, 3}, true},
		{"1.2", [3]int{1, 2, 0}, true},
		{"2", [3]int{2, 0, 0}, true},
		{"0.1.8-rc.1", [3]int{0, 1, 8}, true},
		{"1.2.3+build7", [3]int{1, 2, 3}, true},
		{" 0.1.8 ", [3]int{0, 1, 8}, true},
		{"dev", [3]int{}, false},
		{"", [3]int{}, false},
		{"abc", [3]int{}, false},
		{"1.x.0", [3]int{}, false},
	}
	for _, tc := range cases {
		got, ok := parseCLIVersion(tc.in)
		if ok != tc.valid {
			t.Errorf("parseCLIVersion(%q) valid=%v, want %v", tc.in, ok, tc.valid)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseCLIVersion(%q)=%v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCLIVersionOutdated(t *testing.T) {
	cases := []struct {
		current string
		minimum string
		want    bool
	}{
		{"0.1.7", "0.1.8", true},
		{"0.1.8", "0.1.8", false},
		{"0.2.0", "0.1.8", false},
		{"1.0.0", "0.9.9", false},
		{"0.1.8", "0.2.0", true},
		{"0.1.10", "0.1.9", false},
		{"0.1.9", "0.1.10", true},
		{"dev", "0.1.8", false},     // unparseable current → fail open
		{"0.1.7", "garbage", false}, // unparseable minimum → fail open
		// Prerelease precedence: a staging prerelease must upgrade to the stable
		// release of the same core; stable is never "older" than its prerelease.
		{"0.1.9-alpha.7", "0.1.9", true},
		{"0.1.9", "0.1.9-alpha.7", false},
		{"0.1.9-alpha.2", "0.1.9-alpha.7", true},
		{"0.1.9-alpha.7", "0.1.9-alpha.2", false},
		{"0.1.9-alpha.7", "0.1.9-alpha.7", false},
		{"0.1.10-alpha.1", "0.1.9", false}, // higher core wins despite prerelease
		{"0.1.9-alpha.7", "0.1.10", true},  // lower core, prerelease irrelevant
	}
	for _, tc := range cases {
		if got := cliVersionOutdated(tc.current, tc.minimum); got != tc.want {
			t.Errorf("cliVersionOutdated(%q,%q)=%v, want %v", tc.current, tc.minimum, got, tc.want)
		}
	}
}

func TestCLIVersionGatedCommand(t *testing.T) {
	gated := []string{"run", "docs", "sync", "botlets", "messages", "secrets", "activity", "runtime", "sessions", "plugin", "listen"}
	for _, cmd := range gated {
		if c, ok := cliVersionGatedCommand([]string{cmd}); !ok || c != cmd {
			t.Errorf("cliVersionGatedCommand(%q) = (%q,%v), want gated", cmd, c, ok)
		}
	}
	notGated := []string{"bus", "daemon", "upgrade", "uninstall", "doctor", "diagnose", "mcp", "session-exec", "__runtime-tail", "nonsense"}
	for _, cmd := range notGated {
		if _, ok := cliVersionGatedCommand([]string{cmd}); ok {
			t.Errorf("cliVersionGatedCommand(%q) gated, want not gated", cmd)
		}
	}
	if c, ok := cliVersionGatedCommand([]string{"--runtime", "claude"}); !ok || c != "run" {
		t.Errorf("root --runtime flag = (%q,%v), want (run,true)", c, ok)
	}
	if _, ok := cliVersionGatedCommand(nil); ok {
		t.Error("empty args should not be gated")
	}
}

func TestCLIVersionCacheFresh(t *testing.T) {
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	fresh := versionCheckCacheState{CheckedAt: now.Add(-30 * time.Minute), Minimum: "0.1.8"}
	if !cliVersionCacheFresh(fresh, now, time.Hour) {
		t.Error("recent cache should be fresh")
	}
	stale := versionCheckCacheState{CheckedAt: now.Add(-2 * time.Hour), Minimum: "0.1.8"}
	if cliVersionCacheFresh(stale, now, time.Hour) {
		t.Error("old cache should be stale")
	}
	if cliVersionCacheFresh(versionCheckCacheState{Minimum: "0.1.8"}, now, time.Hour) {
		t.Error("zero CheckedAt should not be fresh")
	}
	if cliVersionCacheFresh(versionCheckCacheState{CheckedAt: now}, now, time.Hour) {
		t.Error("empty minimum should not be fresh")
	}
	if cliVersionCacheFresh(fresh, now, 0) {
		t.Error("zero TTL disables the cache")
	}
}

func TestCLIInstanceIDStable(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", t.TempDir())
	a := cliInstanceID()
	b := cliInstanceID()
	if a == "" || a != b {
		t.Errorf("cliInstanceID not stable: %q vs %q", a, b)
	}
	if len(a) != 16 {
		t.Errorf("cliInstanceID length = %d, want 16", len(a))
	}
}

func TestVersionCheckTTL(t *testing.T) {
	t.Setenv(versionCheckTTLEnv, "")
	if got := versionCheckTTL(); got != defaultVersionCheckTTL {
		t.Errorf("default TTL = %v, want %v", got, defaultVersionCheckTTL)
	}
	t.Setenv(versionCheckTTLEnv, "15m")
	if got := versionCheckTTL(); got != 15*time.Minute {
		t.Errorf("env TTL = %v, want 15m", got)
	}
	t.Setenv(versionCheckTTLEnv, "not-a-duration")
	if got := versionCheckTTL(); got != defaultVersionCheckTTL {
		t.Errorf("invalid env TTL = %v, want default", got)
	}
}

// withVersionGateSeams swaps the package-level test seams and version, restoring
// them on cleanup.
func withVersionGateSeams(t *testing.T, ver string, now time.Time,
	fetch func(context.Context, string, string, string, string) (versionCheckResponse, error),
	upgrade func(string, string) error,
) {
	t.Helper()
	origVersion := version
	origNow := versionCheckNow
	origFetch := versionCheckFetch
	origUpgrade := versionCheckUpgrade
	version = ver
	versionCheckNow = func() time.Time { return now }
	versionCheckFetch = fetch
	versionCheckUpgrade = upgrade
	t.Cleanup(func() {
		version = origVersion
		versionCheckNow = origNow
		versionCheckFetch = origFetch
		versionCheckUpgrade = origUpgrade
	})
	t.Setenv("COMMENT_IO_HOME", t.TempDir())
	t.Setenv(versionCheckSkipEnv, "")
	t.Setenv(versionCheckReexecEnv, "")
}

func TestEnforceCLIVersion_DevSkips(t *testing.T) {
	fetched := false
	withVersionGateSeams(t, "dev", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			fetched = true
			return versionCheckResponse{Minimum: "9.9.9"}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	if err := enforceCLIVersion([]string{"run"}); err != nil {
		t.Errorf("dev build should allow, got %v", err)
	}
	if fetched {
		t.Error("dev build must not hit the network")
	}
}

func TestEnforceCLIVersion_SkipEnv(t *testing.T) {
	withVersionGateSeams(t, "0.1.0", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			t.Error("skip env must not fetch")
			return versionCheckResponse{}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	t.Setenv(versionCheckSkipEnv, "1")
	if err := enforceCLIVersion([]string{"run"}); err != nil {
		t.Errorf("skip env should allow, got %v", err)
	}
}

func TestEnforceCLIVersion_ReexecGuard(t *testing.T) {
	withVersionGateSeams(t, "0.1.0", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			t.Error("re-exec guard must not fetch")
			return versionCheckResponse{}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	t.Setenv(versionCheckReexecEnv, "1")
	if err := enforceCLIVersion([]string{"run"}); err != nil {
		t.Errorf("re-exec guard should allow, got %v", err)
	}
}

func TestEnforceCLIVersion_NonGatedSkips(t *testing.T) {
	withVersionGateSeams(t, "0.1.0", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			t.Error("non-gated command must not fetch")
			return versionCheckResponse{}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	if err := enforceCLIVersion([]string{"upgrade"}); err != nil {
		t.Errorf("non-gated command should allow, got %v", err)
	}
}

func TestEnforceCLIVersion_OutdatedUpgrades(t *testing.T) {
	sentinel := errors.New("upgrade-invoked")
	var gotCurrent, gotMinimum string
	withVersionGateSeams(t, "0.1.7", time.Now(),
		func(_ context.Context, _ string, current string, _ string, _ string) (versionCheckResponse, error) {
			return versionCheckResponse{Minimum: "0.1.8", Latest: "0.1.8"}, nil
		},
		func(current, minimum string) error {
			gotCurrent, gotMinimum = current, minimum
			return sentinel
		},
	)
	err := enforceCLIVersion([]string{"run"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected upgrade sentinel, got %v", err)
	}
	if gotCurrent != "0.1.7" || gotMinimum != "0.1.8" {
		t.Errorf("upgrade called with (%q,%q), want (0.1.7,0.1.8)", gotCurrent, gotMinimum)
	}
}

func TestEnforceCLIVersion_CurrentAllows(t *testing.T) {
	withVersionGateSeams(t, "0.1.8", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			return versionCheckResponse{Minimum: "0.1.8", Latest: "0.1.8"}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	if err := enforceCLIVersion([]string{"docs"}); err != nil {
		t.Errorf("current version should allow, got %v", err)
	}
}

func TestEnforceCLIVersion_FetchErrorFailsOpen(t *testing.T) {
	withVersionGateSeams(t, "0.1.0", time.Now(),
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			return versionCheckResponse{}, errors.New("network down")
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	if err := enforceCLIVersion([]string{"run"}); err != nil {
		t.Errorf("fetch error should fail open, got %v", err)
	}
}

func TestEnforceCLIVersion_UsesFreshCache(t *testing.T) {
	now := time.Now()
	sentinel := errors.New("upgrade-from-cache")
	withVersionGateSeams(t, "0.1.7", now,
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			t.Error("fresh cache must not fetch")
			return versionCheckResponse{}, nil
		},
		func(string, string) error { return sentinel },
	)
	home := versionCheckHomeDir()
	writeVersionCheckCache(home, versionCheckCacheState{CheckedAt: now.Add(-1 * time.Minute), BaseURL: versionCheckBaseURL(), Minimum: "0.1.8", Latest: "0.1.8"})
	if err := enforceCLIVersion([]string{"run"}); !errors.Is(err, sentinel) {
		t.Errorf("expected cache-driven upgrade, got %v", err)
	}
}

// A fresh cache produced by a different backend (e.g. staging) must be ignored
// when the current backend differs, so switching COMMENT_IO_BASE_URL re-checks
// the new backend instead of reusing the other environment's minimum.
func TestEnforceCLIVersion_IgnoresCacheFromOtherBackend(t *testing.T) {
	now := time.Now()
	fetched := false
	withVersionGateSeams(t, "0.1.7", now,
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			fetched = true
			return versionCheckResponse{Minimum: "0.0.0", Latest: "0.0.0"}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	home := versionCheckHomeDir()
	// Cache is fresh and would force an upgrade, but it came from another backend.
	writeVersionCheckCache(home, versionCheckCacheState{
		CheckedAt: now.Add(-1 * time.Minute),
		BaseURL:   "https://other-backend.example.test",
		Minimum:   "9.9.9",
		Latest:    "9.9.9",
	})
	if err := enforceCLIVersion([]string{"run"}); err != nil {
		t.Fatalf("expected allow after re-checking the current backend, got %v", err)
	}
	if !fetched {
		t.Error("a cache from a different backend must be ignored (should refetch)")
	}
}

func TestComparePrerelease(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "alpha", 1}, // stable release outranks any prerelease
		{"alpha", "", -1},
		{"alpha.1", "alpha.1", 0},
		{"alpha.1", "alpha.2", -1},
		{"alpha.2", "alpha.10", -1}, // numeric identifiers compare numerically
		{"alpha", "beta", -1},
		{"alpha", "alpha.1", -1}, // fewer identifiers < more identifiers
		{"1", "alpha", -1},       // numeric < alphanumeric
		{"rc.1", "rc.1", 0},
	}
	for _, tc := range cases {
		if got := comparePrerelease(tc.a, tc.b); got != tc.want {
			t.Errorf("comparePrerelease(%q,%q)=%d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCLIVersionPrerelease(t *testing.T) {
	cases := []struct{ in, want string }{
		{"0.1.9", ""},
		{"0.1.9-alpha.7", "alpha.7"},
		{"v0.1.9-rc.1", "rc.1"},
		{"0.1.9-rc.1+build5", "rc.1"},
		{"0.1.9+build5", ""},
		{" 0.1.9-alpha ", "alpha"},
	}
	for _, tc := range cases {
		if got := cliVersionPrerelease(tc.in); got != tc.want {
			t.Errorf("cliVersionPrerelease(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEnforceCLIVersion_WritesCacheOnFetch(t *testing.T) {
	now := time.Now()
	withVersionGateSeams(t, "0.1.8", now,
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			return versionCheckResponse{Minimum: "0.1.8", Latest: "0.1.8"}, nil
		},
		func(string, string) error { return errors.New("should not upgrade") },
	)
	home := versionCheckHomeDir()
	if err := enforceCLIVersion([]string{"sync"}); err != nil {
		t.Fatalf("allow expected, got %v", err)
	}
	state, ok := readVersionCheckCache(home)
	if !ok || state.Minimum != "0.1.8" {
		t.Errorf("expected cache write with minimum 0.1.8, got %+v ok=%v", state, ok)
	}
}
