//go:build darwin || linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	_ "modernc.org/sqlite"
)

func TestDiagnoseNoUploadWritesBundle(t *testing.T) {
	home := privateTempHome(t, "comment-cli-diagnose-")
	logs := filepath.Join(home, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "commentd.jsonl"), []byte(`{"level":"info"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{"diagnose", "--home", home, "--no-upload"})
	if err != nil {
		t.Fatalf("diagnose failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Diagnostics bundle contents:") || !strings.Contains(output, "Skipping upload") {
		t.Fatalf("unexpected diagnose output:\n%s", output)
	}

	bundlePath := filepath.Join(home, "diagnostics")
	entries, err := os.ReadDir(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("diagnostic bundle count = %d, want 1", len(entries))
	}

	names := readTarGzNames(t, filepath.Join(bundlePath, entries[0].Name()))
	if !slices.Contains(names, "logs/commentd.jsonl") || !slices.Contains(names, "system.json") {
		t.Fatalf("bundle names = %#v", names)
	}
	if slices.Contains(names, "bus/history.sqlite") {
		t.Fatalf("history.sqlite should not be included without --include-history: %#v", names)
	}
}

func TestDiagnoseBundleIncludesHistoryOnlyWhenRequested(t *testing.T) {
	home := privateTempHome(t, "comment-cli-diagnose-history-")
	paths, err := resolveCLIPaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.History, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, entries, err := buildDiagnoseBundle(paths, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if containsBundleEntry(entries, "bus/history.sqlite") {
		t.Fatalf("history included without opt-in: %#v", entries)
	}

	_, entries, err = buildDiagnoseBundle(paths, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsBundleEntry(entries, "bus/history.sqlite") {
		t.Fatalf("history omitted with opt-in: %#v", entries)
	}
}

func TestDiagnoseBundleIncludesSetupTranscriptAttachment(t *testing.T) {
	home := privateTempHome(t, "comment-cli-diagnose-attachment-")
	paths, err := resolveCLIPaths(home)
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(privateTempHome(t, "comment-cli-setup-log-"), "setup output.log")
	if err := os.WriteFile(transcript, []byte("step output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, entries, err := buildDiagnoseBundle(paths, false, []string{transcript})
	if err != nil {
		t.Fatal(err)
	}
	if !containsBundleEntry(entries, "attachments/setup-output.log") {
		t.Fatalf("setup transcript attachment omitted: %#v", entries)
	}
}

func TestHelpMentionsDocsAndDoctorDiagnoseDifference(t *testing.T) {
	output, err := captureRun(t, []string{"help"})
	if err != nil {
		t.Fatalf("help failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"comment docs",
		"comment bus install",
		"comment run --runtime <binary>",
		"comment messages wait --bot <recipient>|--profile <handle>",
		"comment mcp run --profile <handle>",
		"Doctor vs diagnose:",
		"comment doctor   checks this computer and can fix safe local setup issues.",
		"comment diagnose creates a tar.gz support bundle and optionally uploads it; it never repairs.",
		"comment help doctor",
		"comment help diagnose",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("help output missing %q:\n%s", want, output)
		}
	}
}

func TestHelpDoctorAndDiagnoseAreFocused(t *testing.T) {
	doctorOutput, err := captureRun(t, []string{"help", "doctor"})
	if err != nil {
		t.Fatalf("help doctor failed: %v\n%s", err, doctorOutput)
	}
	for _, want := range []string{
		"comment doctor",
		"Check the local CLI environment",
		"Doctor checks local health; it does not create a support bundle.",
		"comment diagnose",
	} {
		if !strings.Contains(doctorOutput, want) {
			t.Fatalf("help doctor output missing %q:\n%s", want, doctorOutput)
		}
	}

	diagnoseOutput, err := captureRun(t, []string{"help", "diagnose"})
	if err != nil {
		t.Fatalf("help diagnose failed: %v\n%s", err, diagnoseOutput)
	}
	for _, want := range []string{
		"comment diagnose",
		"Create a diagnostic support bundle",
		"Diagnose does not repair anything.",
		"comment doctor --fix",
	} {
		if !strings.Contains(diagnoseOutput, want) {
			t.Fatalf("help diagnose output missing %q:\n%s", want, diagnoseOutput)
		}
	}
}

func TestDocsCommandPrintsFullCLIReference(t *testing.T) {
	output, err := captureRun(t, []string{"docs"})
	if err != nil {
		t.Fatalf("docs failed: %v\n%s", err, output)
	}
	for _, want := range []string{
		"# Comment.io CLI Docs",
		"## Mental Model",
		"## Fast Path For Running Agents",
		"## Doctor vs Diagnose",
		"### comment messages",
		"### comment sync",
		"COMMENT_IO_LOCAL_DOCS_ROOT",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("docs output missing %q:\n%s", want, output)
		}
	}
}

func TestMessagesSendTimeoutAllowsManagedColdStart(t *testing.T) {
	if messagesSendResponseTimeout() < managedSessionStartWait {
		t.Fatalf("messages send timeout = %s, want at least managed session start wait %s", messagesSendResponseTimeout(), managedSessionStartWait)
	}
}

func TestUploadDiagnoseBundleRejectsOversizedBundles(t *testing.T) {
	_, err := uploadDiagnoseBundle(context.Background(), "https://comment.io", bytes.Repeat([]byte{'x'}, maxUploadBytes+1))
	if err == nil || !strings.Contains(err.Error(), "bundle exceeds") {
		t.Fatalf("oversized upload err = %v", err)
	}
}

func readTarGzNames(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

func containsBundleEntry(entries []bundleEntry, name string) bool {
	for _, entry := range entries {
		if entry.name == name {
			return true
		}
	}
	return false
}

func TestHealthDoesNotInitializeBus(t *testing.T) {
	home := t.TempDir()
	if err := health(home); err != nil {
		t.Fatal(err)
	}
	if busStateExists(home) {
		t.Fatal("health created bus state")
	}
}

func TestDaemonCompatibilityHealthChecksSocket(t *testing.T) {
	uninitializedHome := t.TempDir()
	if output, err := captureRun(t, []string{"daemon", "health", "--home", uninitializedHome}); err == nil {
		t.Fatalf("daemon health without daemon err = %v output = %s", err, output)
	}
	if busStateExists(uninitializedHome) {
		t.Fatal("daemon health created bus state")
	}

	home, _, stop := startCLITestDaemon(t)
	defer stop()
	output, err := captureRun(t, []string{"daemon", "health", "--home", home})
	if err != nil {
		t.Fatalf("daemon health failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["socket_path"] == "" {
		t.Fatalf("daemon health result = %#v", result)
	}
}

func TestRepairDryRunDoesNotInitializeBus(t *testing.T) {
	home := t.TempDir()
	if err := repair([]string{"--dry-run", "--home", home}); err != nil {
		t.Fatal(err)
	}
	if busStateExists(home) {
		t.Fatal("repair dry-run created bus state")
	}
}

func TestRepairPreInitRequiresDryRunAndValidSelectors(t *testing.T) {
	home := t.TempDir()
	if err := repair([]string{"--home", home}); err == nil || !strings.Contains(err.Error(), "bus is not initialized") {
		t.Fatalf("pre-init non-dry repair err = %v", err)
	}
	if err := repair([]string{"--dry-run", "--home", home, "--message-id", "not-a-message"}); err == nil || !strings.Contains(err.Error(), "invalid message_id") {
		t.Fatalf("pre-init invalid message selector err = %v", err)
	}
	if err := repair([]string{"--dry-run", "--home", home, "--op-id", "not-an-op"}); err == nil || !strings.Contains(err.Error(), "invalid op_id") {
		t.Fatalf("pre-init invalid op selector err = %v", err)
	}
	if busStateExists(home) {
		t.Fatal("pre-init repair validation created bus state")
	}
}

func TestMessagesRepairCLIUsesDaemonSocket(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	messageID, err := commentbus.GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	opID, err := commentbus.GenerateLocalID("op", 0)
	if err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "repair",
		"--home", home,
		"--message-id", messageID,
		"--op-id", opID,
	})
	if err != nil {
		t.Fatalf("messages repair failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["dry_run"] != false {
		t.Fatalf("repair dry_run = %#v", result["dry_run"])
	}
	if actions, ok := result["actions"].([]any); !ok || len(actions) != 0 {
		t.Fatalf("repair actions = %#v", result["actions"])
	}
}

func TestMessagesRepairCLIUsesCommentIOHome(t *testing.T) {
	home := privateTempHome(t, "comment-cli-env-home-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := commentbus.OpenStore(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commentbus.EnsureOwnerCapability(paths); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_HOME", home)
	t.Setenv("HOME", t.TempDir())
	messageID, err := commentbus.GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "repair",
		"--dry-run",
		"--message-id", messageID,
	})
	if err != nil {
		t.Fatalf("messages repair via COMMENT_IO_HOME failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["dry_run"] != true {
		t.Fatalf("repair dry_run = %#v", result["dry_run"])
	}
	if result["initialized"] != true {
		t.Fatalf("repair initialized = %#v", result["initialized"])
	}
	if actions, ok := result["actions"].([]any); !ok || len(actions) != 0 {
		t.Fatalf("repair actions = %#v", result["actions"])
	}
}

func TestMessagesRepairDryRunUsesOfflineStoreWhenDaemonUnavailable(t *testing.T) {
	home := privateTempHome(t, "comment-cli-offline-repair-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := commentbus.OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commentbus.EnsureOwnerCapability(paths); err != nil {
		t.Fatal(err)
	}
	record, err := commentbus.RegisterSession(commentbus.RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(home, "botlets"),
		SessionName: "comment-reviewer",
		PaneTarget:  "%1",
		Runtime:     "claude",
		State:       "stale",
		Now:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := store.InsertLocalMessages(ctx, commentbus.LocalMessageSend{
		SenderProfile: "max.sender",
		SenderBotName: "writer",
		Recipients: []commentbus.LocalMessageRecipient{{
			Profile: "max.reviewer",
			BotName: "reviewer",
		}},
		Body: commentbus.MessageBody{Format: "markdown", Content: "Please review this."},
		Now:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	messageID := sent.Messages[0].ID
	scopeType := "profile"
	scopeID := "max.reviewer"
	generation := record.Generation
	if _, err := store.ClaimMessage(ctx, commentbus.MessageClaimOptions{
		Profile:           "max.reviewer",
		MessageID:         messageID,
		ClaimHolder:       "session:" + record.SessionID + ":" + record.Generation,
		SessionID:         &record.SessionID,
		SessionScopeType:  &scopeType,
		SessionScopeID:    &scopeID,
		SessionGeneration: &generation,
		LeaseTTL:          time.Hour,
		Now:               time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "repair",
		"--dry-run",
		"--home", home,
		"--message-id", messageID,
	})
	if err != nil {
		t.Fatalf("offline repair dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["initialized"] != true {
		t.Fatalf("repair initialized = %#v", result["initialized"])
	}
	actions, ok := result["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("repair actions = %#v", result["actions"])
	}
	action := actions[0].(map[string]any)
	if action["action"] != "requeue_message" || action["message_id"] != messageID || action["reason"] != "session_not_alive" {
		t.Fatalf("repair action = %#v", action)
	}
}

func TestMessagesRepairDryRunRejectsManagedSessionEnv(t *testing.T) {
	home := privateTempHome(t, "comment-cli-session-repair-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := commentbus.OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commentbus.EnsureOwnerCapability(paths); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	installManagedSessionEnv(t, paths, "max.reviewer", "reviewer")

	output, err := captureRun(t, []string{
		"messages", "repair",
		"--dry-run",
		"--home", home,
	})
	if err == nil || !strings.Contains(err.Error(), "owner-only command is not allowed from a managed session") {
		t.Fatalf("managed-session repair dry-run err = %v output = %s", err, output)
	}
}

func TestInitBusCreatesBusState(t *testing.T) {
	home := t.TempDir()
	if err := initBus(home); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, "bus", "history.sqlite"),
		filepath.Join(home, "bus", "capabilities", "owner.cap"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing initialized path %s: %v", path, err)
		}
	}
}

func TestBusLaunchdInstallStartStopStatusUninstall(t *testing.T) {
	userHome := t.TempDir()
	linkedHome := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(userHome, linkedHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", userHome)
	fake := installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(linkedHome, ".comment-io")
	canonicalHome := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "custom-botlets")

	installOut, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus install failed: %v\n%s", err, installOut)
	}
	installResult := decodeTestJSONMap(t, installOut)
	label := installResult["label"].(string)
	if installResult["installed"] != true || installResult["loaded"] != true || !strings.HasPrefix(label, "io.comment.commentd.") {
		t.Fatalf("install result = %#v", installResult)
	}
	if installResult["botlets_home"] != botletsHome {
		t.Fatalf("install botlets_home = %#v, want %s", installResult["botlets_home"], botletsHome)
	}
	if !busStateExists(home) {
		t.Fatal("bus install did not initialize local bus state")
	}
	plistPath := installResult["plist_path"].(string)
	if wantDir := filepath.Join(userHome, "Library", "LaunchAgents"); filepath.Dir(plistPath) != wantDir {
		t.Fatalf("plist dir = %q, want %q", filepath.Dir(plistPath), wantDir)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	plistText := string(plist)
	for _, want := range []string{
		"<string>" + bin + "</string>",
		"<string>bus</string>",
		"<string>run</string>",
		"<string>--home</string>",
		"<string>" + home + "</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>COMMENT_IO_HOME</key>",
	} {
		if !strings.Contains(plistText, want) {
			t.Fatalf("plist missing %q:\n%s", want, plistText)
		}
	}
	if strings.Contains(plistText, "--botlets-home") || strings.Contains(plistText, "BOTLETS_HOME") {
		t.Fatalf("plist should not pin botlets home outside bus config:\n%s", plistText)
	}
	if info, err := os.Stat(plistPath); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode info=%v err=%v", info, err)
	}
	fake.requireCall(t, "bootstrap", launchdDomainTarget(), plistPath)
	fake.requireCall(t, "kickstart", launchdServiceTarget(label))

	statusOut, err := captureRun(t, []string{"bus", "status", "--home", canonicalHome})
	if err != nil {
		t.Fatalf("bus status failed: %v\n%s", err, statusOut)
	}
	statusResult := decodeTestJSONMap(t, statusOut)
	if statusResult["supported"] != true || statusResult["installed"] != true || statusResult["loaded"] != true {
		t.Fatalf("loaded status result = %#v", statusResult)
	}
	if statusResult["label"] != label {
		t.Fatalf("status label = %#v, want %s", statusResult["label"], label)
	}
	if statusResult["botlets_home"] != botletsHome {
		t.Fatalf("status botlets_home = %#v, want %s", statusResult["botlets_home"], botletsHome)
	}

	stopOut, err := captureRun(t, []string{"bus", "stop", "--home", home})
	if err != nil {
		t.Fatalf("bus stop failed: %v\n%s", err, stopOut)
	}
	stopResult := decodeTestJSONMap(t, stopOut)
	if stopResult["installed"] != true || stopResult["loaded"] != false {
		t.Fatalf("stop result = %#v", stopResult)
	}

	startOut, err := captureRun(t, []string{"bus", "start", "--home", home})
	if err != nil {
		t.Fatalf("bus start failed: %v\n%s", err, startOut)
	}
	startResult := decodeTestJSONMap(t, startOut)
	if startResult["installed"] != true || startResult["loaded"] != true {
		t.Fatalf("start result = %#v", startResult)
	}
	if startResult["botlets_home"] != botletsHome {
		t.Fatalf("start botlets_home = %#v, want %s", startResult["botlets_home"], botletsHome)
	}

	uninstallOut, err := captureRun(t, []string{"bus", "uninstall", "--home", home})
	if err != nil {
		t.Fatalf("bus uninstall failed: %v\n%s", err, uninstallOut)
	}
	uninstallResult := decodeTestJSONMap(t, uninstallOut)
	if uninstallResult["installed"] != false || uninstallResult["loaded"] != false {
		t.Fatalf("uninstall result = %#v", uninstallResult)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist still exists after uninstall: %v", err)
	}
}

func TestBusLaunchdInstallFallsBackToUserDomainWhenGUIDomainUnsupported(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	fake := installFakeLaunchctl(t)
	fake.unsupportedGUIDomain = true
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, ".comment-io")

	installOut, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus install failed: %v\n%s", err, installOut)
	}
	installResult := decodeTestJSONMap(t, installOut)
	label := installResult["label"].(string)
	plistPath := installResult["plist_path"].(string)

	fake.requireCall(t, "print", launchdGUIDomainTarget())
	fake.requireCall(t, "bootstrap", launchdUserDomainTarget(), plistPath)
	fake.requireCall(t, "kickstart", launchdUserDomainTarget()+"/"+label)
}

func TestBusLaunchdInstallDryRunDoesNotMutate(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, "comment & <home>")
	botletsHome := filepath.Join(userHome, "botlets & <home>")

	output, err := captureRun(t, []string{
		"bus", "install",
		"--dry-run",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus install dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["dry_run"] != true {
		t.Fatalf("dry-run result = %#v", result)
	}
	plist := result["plist"].(string)
	if !strings.Contains(plist, "comment &amp; &lt;home&gt;") {
		t.Fatalf("plist did not XML-escape home path:\n%s", plist)
	}
	if result["botlets_home"] != botletsHome {
		t.Fatalf("dry-run botlets_home = %#v, want %s", result["botlets_home"], botletsHome)
	}
	if strings.Contains(plist, "--botlets-home") || strings.Contains(plist, "BOTLETS_HOME") {
		t.Fatalf("dry-run plist should not pin botlets home outside bus config:\n%s", plist)
	}
	if busStateExists(home) {
		t.Fatal("dry-run initialized bus state")
	}
	if fileExists(result["plist_path"].(string)) {
		t.Fatal("dry-run wrote launch agent plist")
	}
}

func TestBusLaunchdInstallReclaimsStaleDaemonSocket(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, ".comment-io")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(home, "daemon.sock")
	if err := os.WriteFile(socketPath, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	const legacyPID = 75432
	var kills []killCall
	holdersCalls := 0
	oldHolders := socketHolderPIDsFn
	oldKill := killProcessFn
	oldReach := socketReachableFn
	socketHolderPIDsFn = func(p string) []int {
		holdersCalls++
		if p != socketPath {
			return nil
		}
		return []int{legacyPID}
	}
	killProcessFn = func(pid int, sig os.Signal) error {
		kills = append(kills, killCall{pid: pid, sig: sig})
		return nil
	}
	socketReachableFn = func(_ string) bool { return false }
	t.Cleanup(func() {
		socketHolderPIDsFn = oldHolders
		killProcessFn = oldKill
		socketReachableFn = oldReach
	})

	out, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus install failed: %v\n%s", err, out)
	}
	if holdersCalls == 0 {
		t.Fatal("reclaim path did not query socket holders")
	}
	if len(kills) == 0 {
		t.Fatal("expected legacy daemon to be signaled")
	}
	if kills[0].pid != legacyPID || kills[0].sig != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM to pid %d, got %+v", legacyPID, kills[0])
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("stale socket file should have been removed: %v", err)
	}
}

type killCall struct {
	pid int
	sig os.Signal
}

func TestBusLaunchdInstallRetriesTransientBootstrap(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	fake := installFakeLaunchctl(t)
	fake.bootstrapHook = func(attempt int) ([]byte, error) {
		if attempt == 1 {
			return []byte("Bootstrap failed: 5: Input/output error\n"),
				errors.New("exit status 5")
		}
		return nil, nil
	}
	oldDelay := launchdBootstrapRetryDelay
	launchdBootstrapRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { launchdBootstrapRetryDelay = oldDelay })

	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, ".comment-io")

	out, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus install should have retried, got: %v\n%s", err, out)
	}
	if fake.bootstrapAttempts != 2 {
		t.Fatalf("expected 2 bootstrap attempts (initial + retry), got %d", fake.bootstrapAttempts)
	}
	result := decodeTestJSONMap(t, out)
	if result["loaded"] != true {
		t.Fatalf("expected loaded=true after retry, got %#v", result)
	}
}

func TestBusLaunchdInstallSurfacesNonTransientBootstrapError(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	fake := installFakeLaunchctl(t)
	fake.bootstrapHook = func(attempt int) ([]byte, error) {
		return []byte("Bootstrap failed: 37: Some other error\n"),
			errors.New("exit status 37")
	}
	oldDelay := launchdBootstrapRetryDelay
	launchdBootstrapRetryDelay = func(int) time.Duration { return 0 }
	t.Cleanup(func() { launchdBootstrapRetryDelay = oldDelay })

	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, ".comment-io")

	if _, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--bin", bin,
	}); err == nil {
		t.Fatal("expected non-transient bootstrap error to surface, got nil")
	}
	if fake.bootstrapAttempts != 1 {
		t.Fatalf("expected no retry for non-transient error, got %d attempts", fake.bootstrapAttempts)
	}
}

func TestBusSystemdInstallStartStopStatusUninstall(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	fake := installFakeSystemctl(t)
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "custom-botlets")

	installOut, err := captureRun(t, []string{
		"bus", "install",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus systemd install failed: %v\n%s", err, installOut)
	}
	installResult := decodeTestJSONMap(t, installOut)
	if installResult["service_manager"] != "systemd" || installResult["installed"] != true || installResult["loaded"] != true {
		t.Fatalf("install result = %#v", installResult)
	}
	if installResult["botlets_home"] != botletsHome {
		t.Fatalf("install botlets_home = %#v, want %s", installResult["botlets_home"], botletsHome)
	}
	if !busStateExists(home) {
		t.Fatal("bus install did not initialize local bus state")
	}
	unitName := installResult["unit_name"].(string)
	unitPath := installResult["unit_path"].(string)
	if wantDir := filepath.Join(userHome, ".config", "systemd", "user"); filepath.Dir(unitPath) != wantDir {
		t.Fatalf("unit dir = %q, want %q", filepath.Dir(unitPath), wantDir)
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	unitText := string(unit)
	for _, want := range []string{
		`ExecStart="` + bin + `" "bus" "run" "--home" "` + home + `"`,
		"Restart=always",
		`Environment="COMMENT_IO_HOME=` + home + `"`,
		"StandardOutput=append:",
		"StandardError=append:",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unitText, want) {
			t.Fatalf("unit missing %q:\n%s", want, unitText)
		}
	}
	if strings.Contains(unitText, "--botlets-home") || strings.Contains(unitText, "BOTLETS_HOME") {
		t.Fatalf("unit should not pin botlets home outside bus config:\n%s", unitText)
	}
	fake.requireCall(t, "--user", "daemon-reload")
	fake.requireCall(t, "--user", "enable", "--now", unitName)
	fake.requireCall(t, "--user", "restart", unitName)

	statusOut, err := captureRun(t, []string{"bus", "status", "--home", home})
	if err != nil {
		t.Fatalf("bus systemd status failed: %v\n%s", err, statusOut)
	}
	statusResult := decodeTestJSONMap(t, statusOut)
	if statusResult["supported"] != true || statusResult["installed"] != true || statusResult["loaded"] != true {
		t.Fatalf("loaded status result = %#v", statusResult)
	}

	stopOut, err := captureRun(t, []string{"bus", "stop", "--home", home})
	if err != nil {
		t.Fatalf("bus systemd stop failed: %v\n%s", err, stopOut)
	}
	stopResult := decodeTestJSONMap(t, stopOut)
	if stopResult["installed"] != true || stopResult["loaded"] != false {
		t.Fatalf("stop result = %#v", stopResult)
	}

	startOut, err := captureRun(t, []string{"bus", "start", "--home", home})
	if err != nil {
		t.Fatalf("bus systemd start failed: %v\n%s", err, startOut)
	}
	startResult := decodeTestJSONMap(t, startOut)
	if startResult["installed"] != true || startResult["loaded"] != true {
		t.Fatalf("start result = %#v", startResult)
	}

	uninstallOut, err := captureRun(t, []string{"bus", "uninstall", "--home", home})
	if err != nil {
		t.Fatalf("bus systemd uninstall failed: %v\n%s", err, uninstallOut)
	}
	uninstallResult := decodeTestJSONMap(t, uninstallOut)
	if uninstallResult["installed"] != false || uninstallResult["loaded"] != false {
		t.Fatalf("uninstall result = %#v", uninstallResult)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatalf("unit still exists after uninstall: %v", err)
	}
}

func TestBusSystemdInstallDryRunDoesNotMutate(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	installFakeSystemctl(t)
	bin := writeFakeCommentBinary(t)
	home := filepath.Join(userHome, "comment % <home>")
	botletsHome := filepath.Join(userHome, "botlets")

	output, err := captureRun(t, []string{
		"bus", "install",
		"--dry-run",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bin", bin,
	})
	if err != nil {
		t.Fatalf("bus systemd install dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["service_manager"] != "systemd" || result["dry_run"] != true {
		t.Fatalf("dry-run result = %#v", result)
	}
	unit := result["unit"].(string)
	if !strings.Contains(unit, `COMMENT_IO_HOME=`+strings.ReplaceAll(home, "%", "%%")) {
		t.Fatalf("unit did not escape systemd percent specifier:\n%s", unit)
	}
	if busStateExists(home) {
		t.Fatal("dry-run initialized bus state")
	}
	if fileExists(result["unit_path"].(string)) {
		t.Fatal("dry-run wrote systemd unit")
	}
}

func TestBusSystemdInstallRejectsControlCharacters(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	installFakeSystemctl(t)
	bin := writeFakeCommentBinary(t)

	output, err := captureRun(t, []string{
		"bus", "install",
		"--dry-run",
		"--home", filepath.Join(userHome, "comment\nhome"),
		"--bin", bin,
	})
	if err == nil || !strings.Contains(err.Error(), "control characters are not allowed") {
		t.Fatalf("control-char install err = %v output = %s", err, output)
	}
}

func TestBusSystemdUnavailableSuggestsForegroundRun(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	bin := writeFakeCommentBinary(t)
	oldLaunchdSupported := launchdSupported
	oldSystemdSupported := systemdSupported
	oldSystemctl := systemctlCombinedOutput
	launchdSupported = func() bool { return false }
	systemdSupported = func() bool { return true }
	systemctlCombinedOutput = func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Failed to connect to bus\n"), errors.New("exit status 1")
	}
	t.Cleanup(func() {
		launchdSupported = oldLaunchdSupported
		systemdSupported = oldSystemdSupported
		systemctlCombinedOutput = oldSystemctl
	})

	output, err := captureRun(t, []string{
		"bus", "install",
		"--home", filepath.Join(userHome, ".comment-io"),
		"--bin", bin,
	})
	if err == nil || !strings.Contains(err.Error(), "comment bus run") {
		t.Fatalf("install err = %v output = %s", err, output)
	}
}

func TestBusUninstallLaunchdIgnoresInvalidInstallConfig(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("BOTLETS_HOME", "relative-bad")
	installFakeLaunchctl(t)

	output, err := captureRun(t, []string{
		"bus", "uninstall",
		"--home", filepath.Join(userHome, ".comment-io"),
	})
	if err != nil {
		t.Fatalf("launchd uninstall should ignore invalid install config: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["installed"] != false || result["loaded"] != false {
		t.Fatalf("unexpected launchd uninstall result = %#v", result)
	}
}

func TestBusUninstallSystemdIgnoresInvalidInstallConfig(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("BOTLETS_HOME", "relative-bad")
	installFakeSystemctl(t)

	output, err := captureRun(t, []string{
		"bus", "uninstall",
		"--home", filepath.Join(userHome, ".comment-io"),
	})
	if err != nil {
		t.Fatalf("systemd uninstall should ignore invalid install config: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["installed"] != false || result["loaded"] != false {
		t.Fatalf("unexpected systemd uninstall result = %#v", result)
	}
}

func TestUninstallRequiresConfirmationNonInteractive(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = reader.Close()
	})

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--skip-cli",
		"--skip-plugins",
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("uninstall confirmation err = %v output = %s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false || result["confirmation_required"] != true || result["dry_run"] != true {
		t.Fatalf("unexpected confirmation result = %#v", result)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain: %v", err)
	}
}

func TestUninstallDryRunDoesNotMutate(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--dry-run",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["dry_run"] != true {
		t.Fatalf("unexpected dry-run result = %#v", result)
	}
	if !strings.Contains(output, "remove_comment_home") || !strings.Contains(output, "uninstall_cli") {
		t.Fatalf("dry-run output missing planned actions:\n%s", output)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain: %v", err)
	}
}

func TestUninstallRejectsUnreadableBotletsConfig(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	defaultBotletsHome := filepath.Join(userHome, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(defaultBotletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Bus, "config.json"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	if err == nil || !strings.Contains(err.Error(), "read botlets home from bus config") {
		t.Fatalf("unreadable botlets config err = %v output = %s", err, output)
	}
	if _, err := os.Stat(defaultBotletsHome); err != nil {
		t.Fatalf("default botlets home should remain: %v", err)
	}
}

func TestUninstallRejectsUnreadableSyncStatus(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	if err := os.MkdirAll(filepath.Join(home, "sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sync", "config.json"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	if err == nil || !strings.Contains(err.Error(), "read library sync status") {
		t.Fatalf("unreadable sync status err = %v output = %s", err, output)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain: %v", err)
	}
}

func TestUninstallKeepSyncRootInsideHomeFailsBeforeRemoval(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	syncRoot := filepath.Join(home, "library")
	if err := os.MkdirAll(filepath.Join(home, "sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(fmt.Sprintf(`{"version":1,"base_url":"https://comment.io","root":%q,"scope_label":"test"}`+"\n", syncRoot))
	if err := os.WriteFile(filepath.Join(home, "sync", "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--keep-sync-root",
		"--skip-cli",
		"--skip-plugins",
	})
	if err == nil || !strings.Contains(err.Error(), "--keep-sync-root cannot preserve") {
		t.Fatalf("keep sync root err = %v output = %s", err, output)
	}
	if _, err := os.Stat(syncRoot); err != nil {
		t.Fatalf("sync root should remain: %v", err)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain: %v", err)
	}
}

func TestUninstallKeepSyncRootInsideBotletsHomeFailsBeforeRemoval(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	syncRoot := filepath.Join(botletsHome, "library")
	if err := os.MkdirAll(filepath.Join(home, "sync"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(syncRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(fmt.Sprintf(`{"version":1,"base_url":"https://comment.io","root":%q,"scope_label":"test"}`+"\n", syncRoot))
	if err := os.WriteFile(filepath.Join(home, "sync", "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--keep-sync-root",
		"--skip-cli",
		"--skip-plugins",
	})
	if err == nil || !strings.Contains(err.Error(), "--keep-sync-root cannot preserve") {
		t.Fatalf("keep sync root err = %v output = %s", err, output)
	}
	if _, err := os.Stat(syncRoot); err != nil {
		t.Fatalf("sync root should remain: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain: %v", err)
	}
}

func TestUninstallFailsWhenSessionRecordsUnreadable(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Sessions, "sess_abcdefghijklmnopqrst.json"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("unreadable sessions err = %v output = %s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false {
		t.Fatalf("unexpected unreadable sessions result = %#v", result)
	}
	action, ok := findTestAction(result, "stop_managed_sessions")
	if !ok || action["status"] != "error" {
		t.Fatalf("stop_managed_sessions action = %#v", action)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain after session read failure: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain after session read failure: %v", err)
	}
}

func TestUninstallFailsWhenManagedSessionCannotBeKilled(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := commentbus.RegisterSession(commentbus.RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.agent",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.agent",
		BotletsHome: botletsHome,
		SessionName: "comment-reviewer-abc123",
		Runtime:     "claude",
		Now:         time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fakeTmux := &uninstallTestTmux{err: errors.New("tmux refused")}
	oldTmux := newUninstallTmuxController
	newUninstallTmuxController = func(string) uninstallTmuxController { return fakeTmux }
	t.Cleanup(func() { newUninstallTmuxController = oldTmux })

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("session kill failure err = %v output = %s", err, output)
	}
	if !slices.Contains(fakeTmux.killed, record.SessionName) {
		t.Fatalf("tmux kill was not attempted: %#v", fakeTmux.killed)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false {
		t.Fatalf("unexpected session kill failure result = %#v", result)
	}
	action, ok := findTestAction(result, "stop_managed_sessions")
	if !ok || action["status"] != "error" || !strings.Contains(fmt.Sprint(action["error"]), "tmux refused") {
		t.Fatalf("stop_managed_sessions action = %#v", action)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain after session kill failure: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain after session kill failure: %v", err)
	}
}

func TestUninstallStopsBmuxManagedSessionWithFallbackController(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionName := "comment-reviewer-abc123"
	socketPath, err := commentbus.BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	record, err := commentbus.RegisterSession(commentbus.RegisterSessionOptions{
		Paths:       paths,
		Host:        commentbus.SessionHostBmux,
		Profile:     "max.agent",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.agent",
		BotletsHome: botletsHome,
		SessionName: sessionName,
		PaneTarget:  socketPath,
		Runtime:     "claude",
		Now:         time.Unix(1, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(userHome, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	argsOut := filepath.Join(userHome, "bmux-kill-args.txt")
	envOut := filepath.Join(userHome, "bmux-kill-env.txt")
	bmuxBin := filepath.Join(binDir, "bmux")
	if err := os.WriteFile(bmuxBin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+shellQuoteArg(argsOut)+"\nprintf '%s\\n' \"${BMUX_TOKEN_FILE-}\" > "+shellQuoteArg(envOut)+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BMUX_BIN", bmuxBin)

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	if err != nil {
		t.Fatalf("bmux uninstall failed: %v\n%s", err, output)
	}
	argsData, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(argsData), "kill\n-S\n"+socketPath+"\n"; got != want {
		t.Fatalf("bmux kill args = %q, want %q", got, want)
	}
	envData, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, err := commentbus.BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(envData), tokenFile+"\n"; got != want {
		t.Fatalf("bmux token env = %q, want %q", got, want)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "stop_managed_sessions")
	if !ok || action["status"] != "removed" {
		t.Fatalf("stop_managed_sessions action = %#v", action)
	}
	if record.Host != commentbus.SessionHostBmux {
		t.Fatalf("test registered non-bmux record: %+v", record)
	}
}

func TestUninstallDaemonFailureFailsCommand(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	oldLaunchdSupported := launchdSupported
	oldSystemdSupported := systemdSupported
	launchdSupported = func() bool { return false }
	systemdSupported = func() bool { return false }
	t.Cleanup(func() {
		launchdSupported = oldLaunchdSupported
		systemdSupported = oldSystemdSupported
	})

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
		"--skip-cli",
		"--skip-plugins",
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("daemon failure err = %v output = %s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false {
		t.Fatalf("unexpected daemon failure result = %#v", result)
	}
	action, ok := findTestAction(result, "uninstall_daemon")
	if !ok || action["status"] != "error" {
		t.Fatalf("uninstall_daemon action = %#v", action)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("comment home should remain after daemon failure: %v", err)
	}
	if _, err := os.Stat(botletsHome); err != nil {
		t.Fatalf("botlets home should remain after daemon failure: %v", err)
	}
}

func TestUninstallPluginCommandFailureFailsCommand(t *testing.T) {
	userHome := t.TempDir()
	claudeHome := filepath.Join(userHome, ".claude")
	t.Setenv("HOME", userHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	for _, path := range []string{home, botletsHome, filepath.Join(claudeHome, "skills", "comment")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	oldCombined := uninstallCombinedOutput
	oldLookPath := uninstallLookPath
	uninstallCombinedOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if command == "/fake/claude" {
			return []byte("plugin state is corrupted\n"), errors.New("exit status 1")
		}
		return []byte("ok\n"), nil
	}
	uninstallLookPath = func(command string) (string, error) {
		switch command {
		case "npm":
			return "/fake/npm", nil
		case "claude":
			return "/fake/claude", nil
		default:
			return "", errors.New("not found")
		}
	}
	t.Cleanup(func() {
		uninstallCombinedOutput = oldCombined
		uninstallLookPath = oldLookPath
	})

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 1 {
		t.Fatalf("plugin failure err = %v output = %s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false {
		t.Fatalf("unexpected plugin failure result = %#v", result)
	}
	action, ok := findTestAction(result, "uninstall_claude_plugins")
	if !ok || action["status"] != "error" {
		t.Fatalf("uninstall_claude_plugins action = %#v", action)
	}
}

func TestUninstallRemovesLocalStatePluginsAndCLI(t *testing.T) {
	userHome := t.TempDir()
	claudeHome := filepath.Join(userHome, ".claude")
	t.Setenv("HOME", userHome)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	home := filepath.Join(userHome, ".comment-io")
	botletsHome := filepath.Join(userHome, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, "agents"),
		filepath.Join(botletsHome, "agents"),
		filepath.Join(botletsHome, "bots", "reviewer"),
		filepath.Join(claudeHome, "skills", "comment"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-io-plugins", "comment-io"),
		filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-io", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-io", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "botspring-claude-code-plugin"),
		filepath.Join(userHome, ".openclaw", "skills", "comment"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.env"), []byte("COMMENT_IO_ARK_KEY=ark_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(`{"bots":[]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var externalCalls []string
	oldCombined := uninstallCombinedOutput
	oldLookPath := uninstallLookPath
	uninstallCombinedOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
		externalCalls = append(externalCalls, command+" "+strings.Join(args, " "))
		return []byte("ok\n"), nil
	}
	uninstallLookPath = func(command string) (string, error) {
		switch command {
		case "npm":
			return "/fake/npm", nil
		case "claude":
			return "/fake/claude", nil
		default:
			return "", errors.New("not found")
		}
	}
	t.Cleanup(func() {
		uninstallCombinedOutput = oldCombined
		uninstallLookPath = oldLookPath
	})

	output, err := captureRun(t, []string{
		"uninstall",
		"--home", home,
		"--botlets-home", botletsHome,
		"--yes",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["confirmed"] != true {
		t.Fatalf("unexpected uninstall result = %#v", result)
	}
	for _, path := range []string{
		home,
		botletsHome,
		filepath.Join(claudeHome, "skills", "comment"),
		filepath.Join(userHome, ".openclaw", "skills", "comment"),
		// Plugin caches must be removed for every org-name generation:
		// botspring-ai (oldest), botlets-ai, comment-io (interim), and comment-hq (current).
		filepath.Join(claudeHome, "plugins", "cache", "comment-io-plugins", "comment-io"),
		filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-io", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-io", "botspring-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "comment-io-claude-code-plugin"),
		filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "botspring-claude-code-plugin"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed, err=%v", path, err)
		}
	}
	if !slices.Contains(externalCalls, "/fake/npm uninstall -g @comment-io/cli") {
		t.Fatalf("npm uninstall was not invoked: %#v", externalCalls)
	}
	if !slices.Contains(externalCalls, "/fake/claude plugin uninstall comment-io@comment-io-plugins") {
		t.Fatalf("claude plugin uninstall was not invoked: %#v", externalCalls)
	}
	sawBootout := false
	for _, call := range fake.calls {
		if len(call) > 0 && call[0] == "bootout" {
			sawBootout = true
		}
	}
	if !sawBootout {
		t.Fatalf("launchd bootout was not invoked: %#v", fake.calls)
	}
}

func findTestAction(result map[string]any, name string) (map[string]any, bool) {
	actions, ok := result["actions"].([]any)
	if !ok {
		return nil, false
	}
	for _, raw := range actions {
		action, ok := raw.(map[string]any)
		if ok && action["name"] == name {
			return action, true
		}
	}
	return nil, false
}

func TestUpgradeInstallsLatestPackageAndReinstallsDaemonWithFreshBinary(t *testing.T) {
	target := cliNativeTargetName()
	if target == "" {
		t.Skip("native CLI target is not supported on this platform")
	}
	dir := privateTempHome(t, "comment-cli-upgrade-")
	prefix := filepath.Join(dir, "npm-prefix")
	root := filepath.Join(dir, "npm-root")
	home := filepath.Join(dir, "comment-home")
	botletsHome := filepath.Join(dir, "botlets-home")
	npmLog := filepath.Join(dir, "npm.log")
	commentLog := filepath.Join(dir, "comment.log")
	npmBin := filepath.Join(dir, "npm")
	commentBin := filepath.Join(prefix, "bin", "comment")
	packageRoot := filepath.Join(root, "@comment-io", "cli")
	packageBin := filepath.Join(packageRoot, "bin", "comment.js")
	nativeBin := filepath.Join(packageRoot, "dist", target)

	writeExecutableScript(t, npmBin, `#!/bin/sh
log_path=`+strconvQuote(npmLog)+`
npm_prefix=`+strconvQuote(prefix)+`
npm_root=`+strconvQuote(root)+`
printf "%s\n" "$*" >> "$log_path"
if [ "$1" = "install" ]; then
  echo "npm warn using cached metadata" >&2
  exit 0
fi
if [ "$1" = "prefix" ] && [ "$2" = "-g" ]; then
  echo "npm warn prefix warning should stay on stderr" >&2
  printf "%s\n" "$npm_prefix"
  exit 0
fi
if [ "$1" = "root" ] && [ "$2" = "-g" ]; then
  echo "npm warn root warning should stay on stderr" >&2
  printf "%s\n" "$npm_root"
  exit 0
fi
echo "unexpected npm args: $*" >&2
exit 1
`)
	writeUpgradePackageJSON(t, packageRoot, "9.9.9")
	writeExecutableScript(t, packageBin, `#!/bin/sh
log_path=`+strconvQuote(commentLog)+`
printf "%s\n" "$*" >> "$log_path"
if [ "$1" = "bus" ] && [ "$2" = "health" ]; then
  printf '{"ok":true,"version":"9.9.9","source":"shim"}\n'
  exit 0
fi
echo "unexpected package comment args: $*" >&2
exit 1
`)
	writeExecutableScript(t, nativeBin, `#!/bin/sh
log_path=`+strconvQuote(commentLog)+`
printf "%s\n" "$*" >> "$log_path"
if [ "$1" = "bus" ] && [ "$2" = "health" ]; then
  printf '{"ok":true,"version":"9.9.9","source":"native"}\n'
  exit 0
fi
if [ "$1" = "bus" ] && [ "$2" = "install" ]; then
  printf "bus install npm=%s\n" "$COMMENT_IO_NPM_BIN" >> "$log_path"
  printf '{"installed":true,"loaded":true}\n'
  exit 0
fi
if [ "$1" = "daemon" ] && [ "$2" = "health" ]; then
  printf '{"ok":true,"version":"9.9.9","socket":true}\n'
  exit 0
fi
echo "unexpected comment args: $*" >&2
exit 1
`)
	if err := os.MkdirAll(filepath.Dir(commentBin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(packageBin, commentBin); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(commentBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	output, err := captureRun(t, []string{
		"upgrade",
		"--npm", npmBin,
		"--home", home,
		"--botlets-home", botletsHome,
	})
	if err != nil {
		t.Fatalf("upgrade failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["package"] != "@comment-io/cli@latest" {
		t.Fatalf("upgrade result = %#v", result)
	}
	cliResult := result["cli"].(map[string]any)
	paths := cliResult["paths"].(map[string]any)
	if paths["comment_bin"] != commentBin || paths["service_bin"] != nativeBin {
		t.Fatalf("paths = %#v, want comment=%s service=%s", paths, commentBin, nativeBin)
	}
	health := cliResult["health"].(map[string]any)
	if health["source"] != "shim" {
		t.Fatalf("cli health = %#v, want npm shim health", health)
	}
	serviceHealth := cliResult["service_health"].(map[string]any)
	if serviceHealth["source"] == "shim" {
		t.Fatalf("service health = %#v, want native service health", serviceHealth)
	}
	if paths["path_bin"] != commentBin {
		t.Fatalf("path_bin = %#v, want %s", paths["path_bin"], commentBin)
	}
	daemon := result["daemon"].(map[string]any)
	if daemon["skipped"] != false {
		t.Fatalf("daemon result = %#v", daemon)
	}

	npmLogText, err := os.ReadFile(npmLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"install -g --prefer-online @comment-io/cli@latest",
		"prefix -g",
		"root -g",
	} {
		if !strings.Contains(string(npmLogText), want) {
			t.Fatalf("npm log missing %q:\n%s", want, string(npmLogText))
		}
	}
	commentLogText, err := os.ReadFile(commentLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commentLogText), "bus health --home "+home) {
		t.Fatalf("fresh CLI health should use an isolated home, log:\n%s", string(commentLogText))
	}
	for _, want := range []string{
		"bus health --home ",
		"bus install --home " + home + " --botlets-home " + botletsHome + " --bin " + nativeBin,
		"bus install npm=" + npmBin,
		"daemon health --home " + home,
	} {
		if !strings.Contains(string(commentLogText), want) {
			t.Fatalf("comment log missing %q:\n%s", want, string(commentLogText))
		}
	}
}

func TestDefaultUpgradePackageFollowsSelectedEnvironment(t *testing.T) {
	t.Setenv("COMMENT_IO_CLI_PACKAGE", "")

	t.Setenv(commentbus.EnvVar, commentbus.EnvProduction)
	if got := defaultUpgradePackage(); got != "@comment-io/cli@latest" {
		t.Fatalf("production default package = %q, want @comment-io/cli@latest", got)
	}

	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	if got := defaultUpgradePackage(); got != "@comment-io/cli@staging" {
		t.Fatalf("staging default package = %q, want @comment-io/cli@staging", got)
	}
}

func TestDefaultUpgradePackageOverrideWinsOverEnvironment(t *testing.T) {
	t.Setenv(commentbus.EnvVar, commentbus.EnvStaging)
	t.Setenv("COMMENT_IO_CLI_PACKAGE", "@comment-io/cli@canary")

	if got := defaultUpgradePackage(); got != "@comment-io/cli@canary" {
		t.Fatalf("override package = %q, want @comment-io/cli@canary", got)
	}
}

func TestUpgradeRejectsGlobalBinOutsideInstalledPackage(t *testing.T) {
	target := cliNativeTargetName()
	if target == "" {
		t.Skip("native CLI target is not supported on this platform")
	}
	dir := privateTempHome(t, "comment-cli-upgrade-package-bin-")
	prefix := filepath.Join(dir, "npm-prefix")
	root := filepath.Join(dir, "npm-root")
	home := filepath.Join(dir, "comment-home")
	npmBin := filepath.Join(dir, "npm")
	npmLog := filepath.Join(dir, "npm.log")
	commentLog := filepath.Join(dir, "package-comment.log")
	staleLog := filepath.Join(dir, "stale-comment.log")
	commentBin := filepath.Join(prefix, "bin", "comment")
	packageRoot := filepath.Join(root, "@comment-io", "cli")
	packageBin := filepath.Join(root, "@comment-io", "cli", "bin", "comment.js")
	nativeBin := filepath.Join(root, "@comment-io", "cli", "dist", target)

	writeExecutableScript(t, npmBin, `#!/bin/sh
log_path=`+strconvQuote(npmLog)+`
npm_prefix=`+strconvQuote(prefix)+`
npm_root=`+strconvQuote(root)+`
printf "%s\n" "$*" >> "$log_path"
if [ "$1" = "install" ]; then
  exit 0
fi
if [ "$1" = "prefix" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_prefix"
  exit 0
fi
if [ "$1" = "root" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_root"
  exit 0
fi
	exit 1
	`)
	writeUpgradePackageJSON(t, packageRoot, "9.9.9")
	writeExecutableScript(t, packageBin, `#!/bin/sh
log_path=`+strconvQuote(commentLog)+`
printf "%s\n" "$*" >> "$log_path"
if [ "$1" = "bus" ] && [ "$2" = "health" ]; then
  printf '{"ok":true,"version":"9.9.9"}\n'
  exit 0
fi
if [ "$1" = "bus" ] && [ "$2" = "install" ]; then
  printf '{"installed":true,"loaded":true}\n'
  exit 0
fi
if [ "$1" = "daemon" ] && [ "$2" = "health" ]; then
  printf '{"ok":true,"version":"9.9.9"}\n'
  exit 0
fi
exit 1
`)
	writeExecutableScript(t, nativeBin, "#!/bin/sh\nexit 0\n")
	writeExecutableScript(t, commentBin, `#!/bin/sh
echo stale >> `+strconvQuote(staleLog)+`
printf '{"ok":true,"version":"old"}\n'
exit 0
`)

	output, err := captureRun(t, []string{"upgrade", "--npm", npmBin, "--home", home, "--skip-daemon"})
	if err == nil || !strings.Contains(err.Error(), "does not resolve into installed package root") {
		t.Fatalf("upgrade should fail when npm global bin is stale: err=%v output=%s", err, output)
	}
	if _, err := os.Stat(commentLog); !os.IsNotExist(err) {
		t.Fatalf("package-local comment should not have been used: %v", err)
	}
	if _, err := os.Stat(staleLog); !os.IsNotExist(err) {
		t.Fatalf("stale PATH comment should not have been used: %v", err)
	}
}

func TestUpgradeWarnsWhenStaleCommentIsEarlierOnPath(t *testing.T) {
	dir := privateTempHome(t, "comment-cli-upgrade-stale-path-")
	prefix := filepath.Join(dir, "npm-prefix")
	root := filepath.Join(dir, "npm-root")
	npmBin := filepath.Join(dir, "npm")
	commentBin := filepath.Join(prefix, "bin", "comment")
	packageRoot := filepath.Join(root, "@comment-io", "cli")
	packageBin := filepath.Join(packageRoot, "bin", "comment.js")
	staleDir := filepath.Join(dir, "stale-bin")
	staleComment := filepath.Join(staleDir, "comment")

	writeExecutableScript(t, npmBin, `#!/bin/sh
npm_prefix=`+strconvQuote(prefix)+`
npm_root=`+strconvQuote(root)+`
if [ "$1" = "prefix" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_prefix"
  exit 0
fi
if [ "$1" = "root" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_root"
  exit 0
fi
exit 1
`)
	writeUpgradePackageJSON(t, packageRoot, "9.9.9")
	writeExecutableScript(t, packageBin, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Dir(commentBin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(packageBin, commentBin); err != nil {
		t.Fatal(err)
	}
	writeExecutableScript(t, staleComment, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", staleDir+string(os.PathListSeparator)+filepath.Dir(commentBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	paths, err := resolveNpmInstalledCommentPaths(context.Background(), npmBin)
	if err != nil {
		t.Fatalf("path resolution should keep fresh absolute bin usable: %v", err)
	}
	if paths.PathBin != staleComment || !strings.Contains(paths.PathWarning, "not fresh global binary") {
		t.Fatalf("paths = %#v, want stale path warning for %s", paths, staleComment)
	}
}

func TestUpgradeAcceptsNativeCommentEarlierOnPath(t *testing.T) {
	target := cliNativeTargetName()
	if target == "" {
		t.Skip("native CLI target is not supported on this platform")
	}
	dir := privateTempHome(t, "comment-cli-upgrade-native-path-")
	prefix := filepath.Join(dir, "npm-prefix")
	root := filepath.Join(dir, "npm-root")
	npmBin := filepath.Join(dir, "npm")
	commentBin := filepath.Join(prefix, "bin", "comment")
	packageRoot := filepath.Join(root, "@comment-io", "cli")
	packageBin := filepath.Join(packageRoot, "bin", "comment.js")
	nativeBin := filepath.Join(packageRoot, "dist", target)
	localDir := filepath.Join(dir, "local-bin")
	localComment := filepath.Join(localDir, "comment")

	writeExecutableScript(t, npmBin, `#!/bin/sh
npm_prefix=`+strconvQuote(prefix)+`
npm_root=`+strconvQuote(root)+`
if [ "$1" = "prefix" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_prefix"
  exit 0
fi
if [ "$1" = "root" ] && [ "$2" = "-g" ]; then
  printf "%s\n" "$npm_root"
  exit 0
fi
exit 1
`)
	writeUpgradePackageJSON(t, packageRoot, "9.9.9")
	writeExecutableScript(t, packageBin, "#!/bin/sh\nexit 0\n")
	writeExecutableScript(t, nativeBin, "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Dir(commentBin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(packageBin, commentBin); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nativeBin, localComment); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", localDir+string(os.PathListSeparator)+filepath.Dir(commentBin)+string(os.PathListSeparator)+os.Getenv("PATH"))

	paths, err := resolveNpmInstalledCommentPaths(context.Background(), npmBin)
	if err != nil {
		t.Fatalf("path resolution should accept the native bin from the fresh package: %v", err)
	}
	if paths.PathBin != localComment || paths.PathWarning != "" {
		t.Fatalf("paths = %#v, want native path without warning for %s", paths, localComment)
	}
}

func TestUpgradeRejectsUnsupportedPlatforms(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		if err := validateUpgradeSupportedPlatformForRuntime(goos, "amd64"); err != nil {
			t.Fatalf("%s should support upgrade: %v", goos, err)
		}
	}
	for _, goarch := range []string{"amd64", "arm64"} {
		if err := validateUpgradeSupportedPlatformForRuntime("linux", goarch); err != nil {
			t.Fatalf("linux/%s should support upgrade: %v", goarch, err)
		}
	}
	err := validateUpgradeSupportedPlatformForRuntime("windows", "amd64")
	if err == nil || !strings.Contains(err.Error(), "macOS and Linux on x64 or arm64 only") {
		t.Fatalf("windows support err = %v", err)
	}
	err = validateUpgradeSupportedPlatformForRuntime("linux", "386")
	if err == nil || !strings.Contains(err.Error(), "macOS and Linux on x64 or arm64 only") {
		t.Fatalf("unsupported arch err = %v", err)
	}
}

func TestValidateUpgradeDaemonVersionRejectsOldDaemon(t *testing.T) {
	if err := validateUpgradeDaemonVersion(map[string]any{"version": "old"}, "new"); err == nil {
		t.Fatal("expected daemon version mismatch to fail")
	}
	if err := validateUpgradeDaemonVersion(map[string]any{"version": "new"}, "new"); err != nil {
		t.Fatalf("matching daemon version failed: %v", err)
	}
}

func TestUpgradeDaemonInstallEnvClearsAmbientBotletsHome(t *testing.T) {
	t.Setenv("BOTLETS_HOME", "/tmp/ambient-botlets")
	cleared := upgradeDaemonInstallEnv(upgradeOptions{}, "/custom/npm")
	if got := envValue(cleared, "BOTLETS_HOME"); got != "" {
		t.Fatalf("BOTLETS_HOME = %q, want empty", got)
	}
	if got := envValue(cleared, autoUpdateNpmBinaryEnv); got != "/custom/npm" {
		t.Fatalf("%s = %q, want /custom/npm", autoUpdateNpmBinaryEnv, got)
	}
	explicit := upgradeDaemonInstallEnv(upgradeOptions{BotletsHome: "/tmp/explicit-botlets"}, "/custom/npm")
	if got := envValue(explicit, "BOTLETS_HOME"); got != "/tmp/ambient-botlets" {
		t.Fatalf("explicit botlets home should preserve ambient BOTLETS_HOME, got %q", got)
	}
	if got := envValue(explicit, autoUpdateNpmBinaryEnv); got != "/custom/npm" {
		t.Fatalf("explicit botlets home %s = %q, want /custom/npm", autoUpdateNpmBinaryEnv, got)
	}
}

func TestRunUpgradeCommandTimeoutKillsChildProcessGroup(t *testing.T) {
	output, err := runUpgradeCommand(context.Background(), 50*time.Millisecond, "/bin/sh", "-c", "sleep 5 & wait")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout err = %v output = %s", err, string(output))
	}
}

func TestRunUpgradeCommandCancellationKillsChildProcessGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runUpgradeCommand(ctx, time.Minute, "/bin/sh", "-c", "sleep 5 & wait")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("cancel err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled upgrade command did not stop")
	}
}

func TestPluginDoctorCleanCache(t *testing.T) {
	claudeHome := t.TempDir()
	output, err := captureRun(t, []string{"plugin", "doctor", "--claude-home", claudeHome})
	if err != nil {
		t.Fatalf("plugin doctor clean failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["stale"] != false {
		t.Fatalf("clean result = %#v", result)
	}
}

func TestPluginDoctorDetectsAndFixesStaleClaudeCache(t *testing.T) {
	claudeHome := t.TempDir()
	cacheDir := filepath.Join(claudeHome, "plugins", "cache", "comment-io-plugins", "comment-io")
	otherCacheDir := filepath.Join(claudeHome, "plugins", "cache", "other-marketplace", "other-plugin")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherCacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "comment-io.js"), []byte("subscribe_agents();\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "README.md"), []byte("uses claude/channel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	channelDir := filepath.Join(cacheDir, "claude", "channel")
	if err := os.MkdirAll(channelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(channelDir, "config.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherCacheDir, "comment-io-notes.md"), []byte("mentions mcpServers but is not the Comment.io plugin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{"plugin", "doctor", "--claude-home", claudeHome})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 2 {
		t.Fatalf("plugin doctor stale err = %v output = %s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != false || result["stale"] != true {
		t.Fatalf("stale result = %#v", result)
	}
	repairCommands, ok := result["repair_commands"].([]any)
	if !ok || len(repairCommands) == 0 || !strings.Contains(repairCommands[0].(string), "--claude-home '"+claudeHome+"' --fix") {
		t.Fatalf("repair_commands = %#v", result["repair_commands"])
	}
	if findings, ok := result["findings"].([]any); !ok || len(findings) != 3 {
		t.Fatalf("findings = %#v", result["findings"])
	}

	fixOutput, err := captureRun(t, []string{"plugin", "doctor", "--claude-home", claudeHome, "--fix"})
	if err != nil {
		t.Fatalf("plugin doctor fix failed: %v\n%s", err, fixOutput)
	}
	fixResult := decodeTestJSONMap(t, fixOutput)
	if fixResult["ok"] != true || fixResult["fixed"] != true {
		t.Fatalf("fix result = %#v", fixResult)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("stale cache dir still exists: %v", err)
	}
	if _, err := os.Stat(otherCacheDir); err != nil {
		t.Fatalf("unrelated cache dir should remain: %v", err)
	}
}

func TestBusInitAndHealthUseCommentIOHome(t *testing.T) {
	home := privateTempHome(t, "comment-cli-env-home-")
	defaultHome := privateTempHome(t, "comment-cli-default-home-")
	t.Setenv("COMMENT_IO_HOME", home)
	t.Setenv("HOME", defaultHome)

	initOut, err := captureRun(t, []string{"bus", "init"})
	if err != nil {
		t.Fatalf("bus init should use COMMENT_IO_HOME: %v\n%s", err, initOut)
	}
	initResult := decodeTestJSONMap(t, initOut)
	if initResult["home"] != home {
		t.Fatalf("init home = %#v, want %s", initResult["home"], home)
	}
	if !busStateExists(home) {
		t.Fatal("COMMENT_IO_HOME bus state was not initialized")
	}

	healthOut, err := captureRun(t, []string{"bus", "health"})
	if err != nil {
		t.Fatalf("bus health should use COMMENT_IO_HOME: %v\n%s", err, healthOut)
	}
	healthResult := decodeTestJSONMap(t, healthOut)
	if healthResult["home"] != home || healthResult["initialized"] != true {
		t.Fatalf("health result = %#v, want initialized env home %s", healthResult, home)
	}
	if busStateExists(filepath.Join(defaultHome, ".comment-io")) {
		t.Fatal("bus commands initialized default HOME instead of COMMENT_IO_HOME")
	}
}

func TestSecretsAddGetWritesPrivateSecretsFile(t *testing.T) {
	home := privateTempHome(t, "comment-cli-secrets-home-")

	addOut, err := captureRun(t, []string{"secrets", "add", "OPENAI_API_KEY", "sk-test-secret", "--home", home})
	if err != nil {
		t.Fatalf("secrets add failed: %v\n%s", err, addOut)
	}
	if strings.Contains(addOut, "sk-test-secret") {
		t.Fatalf("secrets add printed the secret value:\n%s", addOut)
	}
	addResult := decodeTestJSONMap(t, addOut)
	if addResult["ok"] != true || addResult["name"] != "OPENAI_API_KEY" {
		t.Fatalf("add result = %#v", addResult)
	}
	path := filepath.Join(home, ".secrets")
	if addResult["path"] != path {
		t.Fatalf("secrets path = %#v, want %s", addResult["path"], path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf(".secrets mode = %o, want 600", mode)
	}

	getOut, err := captureRun(t, []string{"secrets", "get", "OPENAI_API_KEY", "--home", home})
	if err != nil {
		t.Fatalf("secrets get failed: %v\n%s", err, getOut)
	}
	if getOut != "sk-test-secret\n" {
		t.Fatalf("secrets get output = %q", getOut)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file secretsFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("invalid secrets file json: %v", err)
	}
	if file.Version != 1 || file.Secrets["OPENAI_API_KEY"] != "sk-test-secret" || file.UpdatedAt == "" {
		t.Fatalf("secrets file = %#v", file)
	}
}

func TestSecretsUseCommentIOHomeAndReadStdin(t *testing.T) {
	home := privateTempHome(t, "comment-cli-secrets-env-home-")
	defaultHome := privateTempHome(t, "comment-cli-secrets-default-home-")
	t.Setenv("COMMENT_IO_HOME", home)
	t.Setenv("HOME", defaultHome)

	addOut, err := captureRunWithStdin(t, []string{"secrets", "add", "ANTHROPIC_API_KEY", "--value-stdin"}, "sk-ant-test\n")
	if err != nil {
		t.Fatalf("secrets add from stdin failed: %v\n%s", err, addOut)
	}
	getOut, err := captureRun(t, []string{"secrets", "get", "ANTHROPIC_API_KEY"})
	if err != nil {
		t.Fatalf("secrets get via COMMENT_IO_HOME failed: %v\n%s", err, getOut)
	}
	if getOut != "sk-ant-test\n" {
		t.Fatalf("secrets get output = %q", getOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".secrets")); err != nil {
		t.Fatalf("COMMENT_IO_HOME .secrets missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultHome, ".comment-io", ".secrets")); !os.IsNotExist(err) {
		t.Fatalf("default HOME .secrets should not exist: %v", err)
	}
}

func TestSecretsValidateNameAndMissingSecret(t *testing.T) {
	home := privateTempHome(t, "comment-cli-secrets-invalid-home-")
	if output, err := captureRun(t, []string{"secrets", "add", "bad-name", "value", "--home", home}); err == nil || !strings.Contains(err.Error(), "invalid secret name") {
		t.Fatalf("invalid secret name err = %v output = %s", err, output)
	}
	if output, err := captureRun(t, []string{"secrets", "get", "MISSING_SECRET", "--home", home}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing secret err = %v output = %s", err, output)
	}
}

func TestBusRunUsesCommentIOHome(t *testing.T) {
	home := privateTempHome(t, "comment-cli-run-env-home-")
	defaultHome := privateTempHome(t, "comment-cli-run-default-home-")
	t.Setenv("COMMENT_IO_HOME", home)
	t.Setenv("HOME", defaultHome)
	if initOut, err := captureRun(t, []string{"bus", "init"}); err != nil {
		t.Fatalf("bus init should use COMMENT_IO_HOME: %v\n%s", err, initOut)
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDaemonWithContext(ctx, "", "", 0, 0, "", "")
	}()
	waitForPath(t, paths.Socket)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon run failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop after cancellation")
	}
	if busStateExists(filepath.Join(defaultHome, ".comment-io")) {
		t.Fatal("bus run initialized default HOME instead of COMMENT_IO_HOME")
	}
}

func TestMCPRunResolvesProfileAndLaunchesChild(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comt.dev"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var capturedProgram mcpProgram
	var capturedEnv []string
	previousRunMCPChild := runMCPChild
	runMCPChild = func(program mcpProgram, env []string) error {
		capturedProgram = program
		capturedEnv = env
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{"run", "--home", home, "--profile", "@max.reviewer", "--entrypoint", entrypoint}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if capturedProgram.Command == "" || len(capturedProgram.Args) != 1 || capturedProgram.Args[0] != entrypoint {
		t.Fatalf("captured program = %#v, want node entrypoint %s", capturedProgram, entrypoint)
	}
	if envValue(capturedEnv, "COMMENT_IO_AGENT_PROFILE") != "max.reviewer" {
		t.Fatalf("profile env = %q", envValue(capturedEnv, "COMMENT_IO_AGENT_PROFILE"))
	}
	if envValue(capturedEnv, "COMMENT_IO_AGENT_SECRET") != "as_mcpsecret" {
		t.Fatalf("agent secret env was not passed from profile")
	}
	if envValue(capturedEnv, "COMMENT_IO_BASE_URL") != "https://comt.dev" {
		t.Fatalf("base url env = %q", envValue(capturedEnv, "COMMENT_IO_BASE_URL"))
	}
	if envValue(capturedEnv, "COMMENT_IO_HOME") != home {
		t.Fatalf("home env = %q, want %s", envValue(capturedEnv, "COMMENT_IO_HOME"), home)
	}
	if envValue(capturedEnv, "COMMENT_IO_MCP_LAUNCHED_BY") != "comment-cli" {
		t.Fatalf("launcher env missing: %#v", capturedEnv)
	}
	if envValue(capturedEnv, "COMMENT_IO_MCP_DAEMON_AVAILABLE") != "0" {
		t.Fatalf("daemon availability env = %q", envValue(capturedEnv, "COMMENT_IO_MCP_DAEMON_AVAILABLE"))
	}
}

func TestMCPRunAllowsMatchingBaseURLOverride(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-base-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://staging.comt.dev"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-base-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var capturedEnv []string
	previousRunMCPChild := runMCPChild
	runMCPChild = func(_ mcpProgram, env []string) error {
		capturedEnv = env
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{
		"run",
		"--home", home,
		"--profile", "max.reviewer",
		"--base-url", "https://staging.comt.dev/some/path?ignored=true",
		"--entrypoint", entrypoint,
	}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if envValue(capturedEnv, "COMMENT_IO_BASE_URL") != "https://staging.comt.dev" {
		t.Fatalf("base url env = %q", envValue(capturedEnv, "COMMENT_IO_BASE_URL"))
	}
}

func TestMCPRunIgnoresAmbientBaseURLForLegacyProfile(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-legacy-env-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-legacy-env-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BASE_URL", "https://comt.dev")
	var capturedEnv []string
	previousRunMCPChild := runMCPChild
	runMCPChild = func(_ mcpProgram, env []string) error {
		capturedEnv = env
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{"run", "--home", home, "--profile", "max.reviewer", "--entrypoint", entrypoint}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if envValue(capturedEnv, "COMMENT_IO_BASE_URL") != "https://comment.io" {
		t.Fatalf("legacy profile base URL = %q, want production default", envValue(capturedEnv, "COMMENT_IO_BASE_URL"))
	}
}

func TestMCPRunRejectsMismatchedBaseURLOverride(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-base-mismatch-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comment.io"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-base-mismatch-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureRun(t, []string{
		"mcp", "run",
		"--home", home,
		"--profile", "max.reviewer",
		"--base-url", "https://comt.dev",
		"--entrypoint", entrypoint,
	})
	if err == nil || !strings.Contains(err.Error(), "must match selected profile base_url") {
		t.Fatalf("mismatched base url err = %v output = %s", err, output)
	}
}

func TestMCPRunRejectsProfileBaseURLUserinfo(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-userinfo-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://user:pass@comment.io"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-userinfo-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureRun(t, []string{
		"mcp", "run",
		"--home", home,
		"--profile", "max.reviewer",
		"--entrypoint", entrypoint,
	})
	if err == nil || !strings.Contains(err.Error(), "username or password") {
		t.Fatalf("userinfo base url err = %v output = %s", err, output)
	}
}

func TestMCPRunDoesNotForwardAmbientSecrets(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-env-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comment.io"}`)
	entrypoint := filepath.Join(privateTempHome(t, "comment-cli-mcp-env-entry-"), "comment-mcp.mjs")
	if err := os.WriteFile(entrypoint, []byte("console.log('unused')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "ambient-access-key")
	t.Setenv("AXIOM_TOKEN", "ambient-axiom-token")
	t.Setenv("COMMENT_IO_AGENT_SECRET", "as_wrongambient")
	var capturedEnv []string
	previousRunMCPChild := runMCPChild
	runMCPChild = func(_ mcpProgram, env []string) error {
		capturedEnv = env
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{"run", "--home", home, "--profile", "max.reviewer", "--entrypoint", entrypoint}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if envValue(capturedEnv, "AWS_ACCESS_KEY_ID") != "" || envValue(capturedEnv, "AXIOM_TOKEN") != "" {
		t.Fatalf("ambient secrets leaked into MCP env: %#v", capturedEnv)
	}
	if envValue(capturedEnv, "COMMENT_IO_AGENT_SECRET") != "as_mcpsecret" {
		t.Fatalf("profile secret did not override ambient secret: %q", envValue(capturedEnv, "COMMENT_IO_AGENT_SECRET"))
	}
}

func TestMCPRunIgnoresAmbientEntrypointOverride(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-entry-env-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comment.io"}`)
	t.Setenv("COMMENT_IO_MCP_ENTRYPOINT", filepath.Join(t.TempDir(), "evil.mjs"))
	t.Setenv("COMMENT_IO_CLI_PACKAGE_ROOT", filepath.Join(t.TempDir(), "evil-package"))
	var capturedProgram mcpProgram
	previousRunMCPChild := runMCPChild
	runMCPChild = func(program mcpProgram, _ []string) error {
		capturedProgram = program
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{"run", "--home", home, "--profile", "max.reviewer"}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if len(capturedProgram.Args) != 1 || !strings.HasSuffix(capturedProgram.Args[0], filepath.Join("packages", "comment-mcp", "src", "index.ts")) {
		t.Fatalf("captured program = %#v, want repo-local MCP entrypoint", capturedProgram)
	}
	if strings.Contains(capturedProgram.Args[0], "evil") {
		t.Fatalf("ambient MCP entrypoint was used: %#v", capturedProgram)
	}
}

func TestMCPRunDoesNotUseCWDRepoFallback(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-cwd-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comment.io"}`)
	hostileRoot := privateTempHome(t, "comment-cli-mcp-hostile-root-")
	hostileEntrypoint := filepath.Join(hostileRoot, "packages", "comment-mcp", "src", "index.ts")
	if err := os.MkdirAll(filepath.Dir(hostileEntrypoint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostileRoot, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostileEntrypoint, []byte("console.log('evil')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(hostileRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldCWD); err != nil {
			t.Fatal(err)
		}
	})
	var capturedProgram mcpProgram
	previousRunMCPChild := runMCPChild
	runMCPChild = func(program mcpProgram, _ []string) error {
		capturedProgram = program
		return nil
	}
	t.Cleanup(func() { runMCPChild = previousRunMCPChild })

	if err := runMCP([]string{"run", "--home", home, "--profile", "max.reviewer"}); err != nil {
		t.Fatalf("mcp run failed: %v", err)
	}
	if len(capturedProgram.Args) != 1 || capturedProgram.Args[0] == hostileEntrypoint || strings.HasPrefix(capturedProgram.Args[0], hostileRoot) {
		t.Fatalf("cwd repo fallback was used: %#v", capturedProgram)
	}
}

func TestMCPRunDoesNotUseAmbientTSX(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-ambient-tsx-home-")
	writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_mcpsecret","base_url":"https://comment.io"}`)
	repoRoot := privateTempHome(t, "comment-cli-mcp-no-node-modules-repo-")
	entrypoint := filepath.Join(repoRoot, "packages", "comment-mcp", "src", "index.ts")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("console.log('dev')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ambientDir := privateTempHome(t, "comment-cli-mcp-ambient-tsx-")
	writeExecutableScript(t, filepath.Join(ambientDir, "tsx"), "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", ambientDir)

	_, err := mcpProgramForEntrypoint(entrypoint, "", repoRoot)
	if err == nil || !strings.Contains(err.Error(), "requires tsx") {
		t.Fatalf("ambient tsx should not be used; err = %v", err)
	}
}

func TestMCPRunDoesNotUseAncestorTSXOutsideRepo(t *testing.T) {
	parent := privateTempHome(t, "comment-cli-mcp-ancestor-tsx-parent-")
	repoRoot := filepath.Join(parent, "repo")
	entrypoint := filepath.Join(repoRoot, "packages", "comment-mcp", "src", "index.ts")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "package.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("console.log('dev')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutableScript(t, filepath.Join(parent, "node_modules", ".bin", "tsx"), "#!/bin/sh\nexit 0\n")

	_, err := mcpProgramForEntrypoint(entrypoint, "", repoRoot)
	if err == nil || !strings.Contains(err.Error(), "requires tsx") {
		t.Fatalf("ancestor tsx outside repo should not be used; err = %v", err)
	}
}

func TestMCPRunRejectsMissingProfile(t *testing.T) {
	home := privateTempHome(t, "comment-cli-mcp-missing-home-")
	output, err := captureRun(t, []string{"mcp", "run", "--home", home, "--profile", "max.reviewer", "--entrypoint", "/tmp/missing.mjs"})
	if err == nil || !strings.Contains(err.Error(), "agent profile") {
		t.Fatalf("missing profile err = %v output = %s", err, output)
	}
}

func TestMessagesCLIUsesDaemonSocket(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "hello from cli",
	})
	if err != nil {
		t.Fatalf("messages send failed: %v\n%s", err, sendOut)
	}
	sendResp := decodeTestSocketResponse(t, sendOut)
	messageID := firstMessageID(t, sendResp)

	waitOut, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err != nil {
		t.Fatalf("messages wait failed: %v\n%s", err, waitOut)
	}
	waitResult := resultMap(t, decodeTestSocketResponse(t, waitOut))
	if waitResult["message_id"] != messageID {
		t.Fatalf("wait message_id = %#v, want %s", waitResult["message_id"], messageID)
	}
	if _, ok := waitResult["body"]; ok {
		t.Fatalf("wait leaked body content: %#v", waitResult)
	}

	receiveOut, err := captureRun(t, []string{
		"messages", "receive",
		"--home", home,
		"--bot", "reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages receive failed: %v\n%s", err, receiveOut)
	}
	receiveResult := resultMap(t, decodeTestSocketResponse(t, receiveOut))
	message := receiveResult["message"].(map[string]any)
	body := message["body"].(map[string]any)
	if body["content"] != "hello from cli" {
		t.Fatalf("received body = %#v", body["content"])
	}

	ackOut, err := captureRun(t, []string{
		"messages", "ack",
		"--home", home,
		"--bot", "reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages ack failed: %v\n%s", err, ackOut)
	}
	ackResult := resultMap(t, decodeTestSocketResponse(t, ackOut))
	if ackResult["state"] != "acked" {
		t.Fatalf("ack state = %#v", ackResult["state"])
	}
}

func TestMessagesWaitRewakeClaimsAndExitsTwo(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "rewake body",
	})
	if err != nil {
		t.Fatalf("messages send failed: %v\n%s", err, sendOut)
	}
	messageID := firstMessageID(t, decodeTestSocketResponse(t, sendOut))

	// --rewake blocks until a message arrives, claims it, prints the received
	// message, and returns the exit-2 sentinel that an asyncRewake hook needs.
	rewakeOut, err := captureRun(t, []string{
		"messages", "wait", "--rewake",
		"--home", home,
		"--bot", "reviewer",
		"--timeout-ms", "2000",
	})
	if code, _ := exitForError(err); code != 2 {
		t.Fatalf("rewake exit code = %d (err %v), want 2\nout=%s", code, err, rewakeOut)
	}

	var result map[string]any
	if jsonErr := json.Unmarshal([]byte(rewakeOut), &result); jsonErr != nil {
		t.Fatalf("rewake output is not JSON: %v\n%s", jsonErr, rewakeOut)
	}
	message, ok := result["message"].(map[string]any)
	if !ok {
		t.Fatalf("rewake output missing message: %#v", result)
	}
	if message["id"] != messageID {
		t.Fatalf("rewake message id = %#v, want %s", message["id"], messageID)
	}
	body, ok := message["body"].(map[string]any)
	if !ok || body["content"] != "rewake body" {
		t.Fatalf("rewake body = %#v", message["body"])
	}

	// The message must now be claimed (received), not still waiting: a plain wait
	// with no timeout should report a timeout rather than re-surfacing it.
	waitOut, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err != nil {
		t.Fatalf("post-rewake wait failed: %v\n%s", err, waitOut)
	}
	postWait := resultMap(t, decodeTestSocketResponse(t, waitOut))
	if postWait["message_id"] == messageID {
		t.Fatalf("message %s re-surfaced after rewake claim: %#v", messageID, postWait)
	}
}

func TestRewakeWaitParamsIncludesRewakeAndSessionTriple(t *testing.T) {
	t.Setenv("COMMENT_IO_SESSION_ID", "sess_aaaaaaaaaaaaaaaaaaaa")
	t.Setenv("COMMENT_IO_SESSION_GENERATION", "gen_bbbbbbbbbbbbbbbb")

	params := rewakeWaitParams("reviewer", "max.reviewer", nil, 600000)
	if params["rewake"] != true {
		t.Fatalf("rewake not set: %#v", params)
	}
	if params["bot"] != "reviewer" || params["profile"] != "max.reviewer" {
		t.Fatalf("bot/profile not forwarded: %#v", params)
	}
	if params["timeout_ms"] != 600000 {
		t.Fatalf("timeout_ms not forwarded: %#v", params)
	}
	if params["session_id"] != "sess_aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("session_id not forwarded from env: %#v", params)
	}
	if params["session_generation"] != "gen_bbbbbbbbbbbbbbbb" {
		t.Fatalf("session_generation not forwarded from env: %#v", params)
	}

	// Malformed / empty managed-session env values are dropped, never forwarded,
	// so a bad value can't fail the wait outright.
	t.Setenv("COMMENT_IO_SESSION_ID", "not-a-session-id")
	t.Setenv("COMMENT_IO_SESSION_GENERATION", "")
	params = rewakeWaitParams("", "max.reviewer", nil, 0)
	if params["rewake"] != true {
		t.Fatalf("rewake not set without env triple: %#v", params)
	}
	if _, ok := params["session_id"]; ok {
		t.Fatalf("malformed session_id forwarded: %#v", params)
	}
	if _, ok := params["session_generation"]; ok {
		t.Fatalf("empty session_generation forwarded: %#v", params)
	}
	if _, ok := params["timeout_ms"]; ok {
		t.Fatalf("zero timeout_ms should be omitted: %#v", params)
	}
}

func TestListenClaimReleaseCLI(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	// Add a free (non-daemon-managed) configured profile alongside the managed
	// reviewer bot, then reload so the daemon sees it.
	freeProfile := filepath.Join(home, "agents", "max.free.json")
	if err := os.WriteFile(freeProfile, []byte(`{"agent_secret":"as_freesecret","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home}); err != nil {
		t.Fatalf("reload-profiles failed: %v", err)
	}

	// Claiming the daemon-managed reviewer handle is refused with MANAGED_HANDLE.
	managedOut, err := captureRun(t, []string{"listen", "claim", "--home", home, "--profile", "max.reviewer"})
	if err == nil {
		t.Fatalf("expected managed-handle claim to fail; out=%s", managedOut)
	}
	managedResp := decodeFailedSocketResponse(t, managedOut)
	if managedResp.Error == nil || managedResp.Error.Code != "MANAGED_HANDLE" {
		t.Fatalf("managed-handle claim error = %+v, want MANAGED_HANDLE", managedResp.Error)
	}

	// Claiming a free handle succeeds.
	claimOut, err := captureRun(t, []string{"listen", "claim", "--home", home, "--profile", "max.free", "--session", "sess-cli-one"})
	if err != nil {
		t.Fatalf("free-handle claim failed: %v\n%s", err, claimOut)
	}
	claimResult := resultMap(t, decodeTestSocketResponse(t, claimOut))
	if claimResult["claimed"] != true || claimResult["claimed_by"] != "sess-cli-one" {
		t.Fatalf("free-handle claim result = %#v", claimResult)
	}

	// A second claim on the held handle is refused with HANDLE_BUSY.
	busyOut, err := captureRun(t, []string{"listen", "claim", "--home", home, "--profile", "max.free"})
	if err == nil {
		t.Fatalf("expected second claim to fail; out=%s", busyOut)
	}
	busyResp := decodeFailedSocketResponse(t, busyOut)
	if busyResp.Error == nil || busyResp.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("second claim error = %+v, want HANDLE_BUSY", busyResp.Error)
	}

	// Releasing with a non-matching session is refused (no takeover).
	wrongRelOut, err := captureRun(t, []string{"listen", "release", "--home", home, "--profile", "max.free", "--session", "sess-other"})
	if err == nil {
		t.Fatalf("expected wrong-session release to fail; out=%s", wrongRelOut)
	}
	if wr := decodeFailedSocketResponse(t, wrongRelOut); wr.Error == nil || wr.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("wrong-session release error = %+v, want HANDLE_BUSY", wr.Error)
	}

	// Releasing with the matching session frees the handle.
	releaseOut, err := captureRun(t, []string{"listen", "release", "--home", home, "--profile", "max.free", "--session", "sess-cli-one"})
	if err != nil {
		t.Fatalf("release failed: %v\n%s", err, releaseOut)
	}
	releaseResult := resultMap(t, decodeTestSocketResponse(t, releaseOut))
	if releaseResult["released"] != true {
		t.Fatalf("release result = %#v", releaseResult)
	}

	// After release a fresh claim succeeds.
	reclaimOut, err := captureRun(t, []string{"listen", "claim", "--home", home, "--profile", "max.free"})
	if err != nil {
		t.Fatalf("reclaim after release failed: %v\n%s", err, reclaimOut)
	}

	// handles --json lists both the managed reviewer and the now-claimed free handle.
	handlesOut, err := captureRun(t, []string{"listen", "handles", "--json", "--home", home})
	if err != nil {
		t.Fatalf("listen handles failed: %v\n%s", err, handlesOut)
	}
	rows := resultMap(t, decodeTestSocketResponse(t, handlesOut))["handles"].([]any)
	sawManaged := false
	sawFree := false
	for _, raw := range rows {
		entry := raw.(map[string]any)
		switch entry["handle"] {
		case "max.reviewer":
			if entry["managed"] != true {
				t.Fatalf("max.reviewer should be managed: %#v", entry)
			}
			sawManaged = true
		case "max.free":
			if entry["managed"] != false || entry["claimed"] != true {
				t.Fatalf("max.free listen state = %#v", entry)
			}
			sawFree = true
		}
	}
	if !sawManaged || !sawFree {
		t.Fatalf("listen handles missing expected entries: %#v", rows)
	}
}

func TestMessageBusCLISmokeUsesFakeTmuxBinary(t *testing.T) {
	home, botletsHome, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	if _, err := os.Stat(filepath.Join(fakeBinDir, "tmux")); err != nil {
		t.Fatalf("fake tmux binary is not first on PATH: %v", err)
	}

	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "smoke body with $(shell) and ; metacharacters",
	})
	if err != nil {
		t.Fatalf("messages send failed: %v\n%s", err, sendOut)
	}
	sendResp := decodeTestSocketResponse(t, sendOut)
	messageID := firstMessageID(t, sendResp)

	waitOut, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err != nil {
		t.Fatalf("messages wait failed: %v\n%s", err, waitOut)
	}
	if waitID := resultMap(t, decodeTestSocketResponse(t, waitOut))["message_id"]; waitID != messageID {
		t.Fatalf("wait message_id = %#v, want %s", waitID, messageID)
	}

	receiveOut, err := captureRun(t, []string{
		"messages", "receive",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages receive failed: %v\n%s", err, receiveOut)
	}
	received := resultMap(t, decodeTestSocketResponse(t, receiveOut))["message"].(map[string]any)
	if received["id"] != messageID {
		t.Fatalf("received message = %#v", received)
	}

	ackOut, err := captureRun(t, []string{
		"messages", "ack",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages ack failed: %v\n%s", err, ackOut)
	}
	if state := resultMap(t, decodeTestSocketResponse(t, ackOut))["state"]; state != "acked" {
		t.Fatalf("ack state = %#v", state)
	}

	tmuxLog, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(tmuxLog)
	if !strings.Contains(logText, "new-session") || !strings.Contains(logText, "send-keys") {
		t.Fatalf("fake tmux log missing launch or nudge commands:\n%s", logText)
	}
	nudgeLine := ""
	for _, line := range strings.Split(logText, "\n") {
		if strings.Contains(line, "comment messages receive "+messageID) {
			nudgeLine = line
			break
		}
	}
	if nudgeLine == "" {
		t.Fatalf("fake tmux log missing nudge for %s:\n%s", messageID, logText)
	}
	if strings.Contains(nudgeLine, "smoke body") || strings.ContainsAny(nudgeLine, "`\"'$;&|<>\r\n") {
		t.Fatalf("tmux nudge exposed unsafe content: %q", nudgeLine)
	}
}

func TestRunRuntimeParserPassesClaudeArgsWithoutDelimiter(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--model", "opus",
		"--runtime=claude",
		"--profile", "@max.reviewer",
		"--cwd", "/tmp/work",
		"--setup-attempt-id", "bla_1234567890abc",
		"--name", "Claude Session",
		"--tmux",
		"review this",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Runtime != "claude" || options.Profile != "max.reviewer" || options.CWD != "/tmp/work" || options.SetupAttemptID != "bla_1234567890abc" {
		t.Fatalf("wrapper options = %+v", options)
	}
	wantArgs := []string{"--model", "opus", "--name", "Claude Session", "--tmux", "review this"}
	if !slices.Equal(options.RuntimeArgs, wantArgs) {
		t.Fatalf("runtime args = %#v, want %#v", options.RuntimeArgs, wantArgs)
	}
}

func TestRunRuntimeParserRespectsDelimiter(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--runtime", "claude",
		"--",
		"--profile", "belongs-to-claude",
		"--cwd", "also-claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"--profile", "belongs-to-claude", "--cwd", "also-claude"}
	if !slices.Equal(options.RuntimeArgs, wantArgs) {
		t.Fatalf("runtime args = %#v, want %#v", options.RuntimeArgs, wantArgs)
	}
}

func TestRunRuntimeParserAcceptsTaskRole(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--role", "task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Role != commentbus.RuntimeRoleTask {
		t.Fatalf("runtime role = %q, want task", options.Role)
	}
}

func TestRunRuntimeParserAcceptsDetach(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--detach",
		"review this",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !options.Detach {
		t.Fatalf("expected Detach=true, got options = %+v", options)
	}
	// --detach is consumed by the launcher, never forwarded to the runtime.
	wantArgs := []string{"review this"}
	if !slices.Equal(options.RuntimeArgs, wantArgs) {
		t.Fatalf("runtime args = %#v, want %#v", options.RuntimeArgs, wantArgs)
	}
}

func TestRunRuntimeParserRejectsDetachWithValue(t *testing.T) {
	// The inline-value form must be rejected, not silently forwarded to the
	// runtime as an unknown flag.
	for _, arg := range []string{"--detach=true", "--detach=1", "--detach=false"} {
		_, err := parseRuntimeRunArgs([]string{"--runtime", "claude", "--profile", "max.reviewer", arg})
		if err == nil || !strings.Contains(err.Error(), "--detach does not accept a value") {
			t.Fatalf("parseRuntimeRunArgs(%q) err = %v, want detach-value error", arg, err)
		}
	}
}

func TestRunRuntimeParserDetachAfterDelimiterIsRuntimeArg(t *testing.T) {
	// Past `--`, `--detach` belongs to the runtime, not the launcher.
	options, err := parseRuntimeRunArgs([]string{
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--",
		"--detach",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Detach {
		t.Fatalf("expected Detach=false for post-delimiter flag, got options = %+v", options)
	}
	if !slices.Equal(options.RuntimeArgs, []string{"--detach"}) {
		t.Fatalf("runtime args = %#v, want [--detach]", options.RuntimeArgs)
	}
}

func TestShouldSkipRuntimeAttach(t *testing.T) {
	if shouldSkipRuntimeAttach(runtimeRunOptions{}) {
		t.Fatal("expected attach by default")
	}
	if !shouldSkipRuntimeAttach(runtimeRunOptions{Detach: true}) {
		t.Fatal("expected skip when Detach set")
	}
	t.Setenv("COMMENT_IO_SKIP_ATTACH", "1")
	if !shouldSkipRuntimeAttach(runtimeRunOptions{}) {
		t.Fatal("expected skip when COMMENT_IO_SKIP_ATTACH=1")
	}
	t.Setenv("COMMENT_IO_SKIP_ATTACH", "0")
	if shouldSkipRuntimeAttach(runtimeRunOptions{}) {
		t.Fatal("expected attach when COMMENT_IO_SKIP_ATTACH=0")
	}
}

func TestRunRuntimeParserAcceptsTaskRoleForProfileShortcut(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--home", "/tmp/comment-home",
		"--role", "task",
		"max.reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.BotShortcut != "max.reviewer" || options.Role != commentbus.RuntimeRoleTask || options.Runtime != "" || options.Profile != "" {
		t.Fatalf("runtime shortcut options = %+v", options)
	}
}

func TestRunRuntimeParserRejectsTaskRoleForBotletsShortcut(t *testing.T) {
	_, err := parseRuntimeRunArgs([]string{
		"--home", "/tmp/comment-home",
		"--role", "task",
		"reviewer",
	})
	if err == nil || !strings.Contains(err.Error(), "only starts the main Botlets session") {
		t.Fatalf("parser err = %v, want Botlets main-session error", err)
	}
}

func TestRunRuntimeParserAcceptsBotletsShortcut(t *testing.T) {
	options, err := parseRuntimeRunArgs([]string{
		"--home", "/tmp/comment-home",
		"reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.BotShortcut != "reviewer" || options.Home != "/tmp/comment-home" || options.Runtime != "" || options.Profile != "" {
		t.Fatalf("runtime shortcut options = %+v", options)
	}
}

func TestRunRuntimeParserDefaultsToDefaultBotlet(t *testing.T) {
	bareOptions, err := parseRuntimeRunArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bareOptions.BotShortcut != "default" || bareOptions.Runtime != "" || bareOptions.Profile != "" {
		t.Fatalf("bare runtime default shortcut options = %+v", bareOptions)
	}

	options, err := parseRuntimeRunArgs([]string{
		"--home", "/tmp/comment-home",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.BotShortcut != "default" || options.Home != "/tmp/comment-home" || options.Runtime != "" || options.Profile != "" {
		t.Fatalf("runtime default shortcut options = %+v", options)
	}
}

func TestBotletsSetupRunCommandUsesSupportedShortcut(t *testing.T) {
	// "guy" is the canonical guide slug and "default" is the recognized legacy
	// slug; both collapse to the bare `comment run` shortcut.
	if got := botletsSetupRunCommand("guy"); got != "comment run" {
		t.Fatalf("guy setup run command = %q", got)
	}
	if got := botletsSetupRunCommand("default"); got != "comment run" {
		t.Fatalf("default setup run command = %q", got)
	}
	if got := botletsSetupRunCommand("reviewer"); got != "comment run reviewer" {
		t.Fatalf("setup run command = %q", got)
	}
	if got := botletsShortcutRuntimeArgs("claude", "reviewer"); !slices.Equal(got, []string{"--dangerously-skip-permissions"}) {
		t.Fatalf("claude shortcut runtime args = %#v", got)
	}
	if got := botletsShortcutRuntimeArgs("codex", "reviewer"); !slices.Equal(got, []string{"--yolo"}) {
		t.Fatalf("codex shortcut runtime args = %#v", got)
	}
}

func TestIsDefaultGuideSlugRecognizesCanonicalAndLegacy(t *testing.T) {
	// Canonical "guy" plus the pre-rebrand legacy "default" both resolve to the
	// account guide (case/space-insensitive); unrelated slugs do not.
	for _, s := range []string{"guy", "GUY", " guy ", "default", "Default", " default "} {
		if !isDefaultGuideSlug(s) {
			t.Fatalf("expected %q to be recognized as the guide slug", s)
		}
	}
	for _, s := range []string{"", "reviewer", "guydefault", "guy-helper", "default-runtime"} {
		if isDefaultGuideSlug(s) {
			t.Fatalf("did not expect %q to be recognized as the guide slug", s)
		}
	}
}

func TestRunRuntimeBotletsShortcutAttachesManagedSession(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return true, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"reviewer",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
}

func TestRunRuntimeRoleFlagSetsTaskRole(t *testing.T) {
	var got runtimeRunOptions
	oldRun := runRuntimeCommand
	runRuntimeCommand = func(options runtimeRunOptions) error {
		got = options
		return nil
	}
	t.Cleanup(func() { runRuntimeCommand = oldRun })

	output, err := captureRun(t, []string{
		"run",
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--role", "task",
		"review",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if got.Role != commentbus.RuntimeRoleTask {
		t.Fatalf("runtime options = %+v, want task role", got)
	}
}

func TestRunRuntimePassesReservedWordsToRuntime(t *testing.T) {
	for _, word := range []string{"status", "task"} {
		t.Run(word, func(t *testing.T) {
			var got runtimeRunOptions
			oldRun := runRuntimeCommand
			runRuntimeCommand = func(options runtimeRunOptions) error {
				got = options
				return nil
			}
			t.Cleanup(func() { runRuntimeCommand = oldRun })

			output, err := captureRun(t, []string{
				"run",
				word,
				"--runtime", "claude",
				"--profile", "max.reviewer",
				"review",
			})
			if err != nil {
				t.Fatalf("run failed: %v\n%s", err, output)
			}
			wantArgs := []string{word, "review"}
			if !slices.Equal(got.RuntimeArgs, wantArgs) {
				t.Fatalf("runtime args = %#v, want %#v", got.RuntimeArgs, wantArgs)
			}
			if got.Role != commentbus.RuntimeRoleMain {
				t.Fatalf("runtime role = %q, want main", got.Role)
			}
		})
	}
}

func TestRootRuntimeShorthandDispatchesToRun(t *testing.T) {
	var got runtimeRunOptions
	oldRun := runRuntimeCommand
	runRuntimeCommand = func(options runtimeRunOptions) error {
		got = options
		return nil
	}
	t.Cleanup(func() { runRuntimeCommand = oldRun })

	output, err := captureRun(t, []string{
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--name", "Claude Session",
		"hello",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if output != "" {
		t.Fatalf("output = %q", output)
	}
	wantArgs := []string{"--name", "Claude Session", "hello"}
	if got.Runtime != "claude" || got.Profile != "max.reviewer" || !slices.Equal(got.RuntimeArgs, wantArgs) {
		t.Fatalf("runtime options = %+v, want args %#v", got, wantArgs)
	}
}

func TestRunRuntimeStartsDaemonTmuxWithForwardedArgs(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	cwd := privateTempHome(t, "comment-cli-run-cwd-")
	var attached string
	overrideTransientRuntimeAttachForTest(t, func(record commentbus.TransientRuntimeRecord) error {
		attached = record.SessionName
		return nil
	})
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
		"--cwd", cwd,
		"--name", "Claude Session",
		"hello",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-run-runner-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	logData, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, want := range []string{
		"new-session",
		"-c " + cwd,
		"-e COMMENT_IO_HOME=" + home,
		"COMMENT_IO_PROFILE='max.runner'",
		"PATH='" + filepath.Join(home, "bus", "bin") + string(os.PathListSeparator),
		"claude",
		"--name",
		"Claude Session",
		"hello",
		"pipe-pane",
		"__runtime-tail",
		"--bytes 65536",
		"exec",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("fake tmux log missing %q:\n%s", want, logText)
		}
	}
}

func TestRunRuntimeAttachesManagedSessionForProfile(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return true, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.reviewer",
		"--dangerously-skip-permissions",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	if strings.HasPrefix(attached, "comment-run-reviewer-") {
		t.Fatalf("attached transient runtime instead of managed session: %q", attached)
	}
	logData, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	if strings.Contains(logText, "comment-run-reviewer-") {
		t.Fatalf("comment run launched a transient runtime for managed profile:\n%s", logText)
	}
	if !strings.Contains(logText, "new-session") || !strings.Contains(logText, "session-exec") {
		t.Fatalf("fake tmux log missing managed session launch:\n%s", logText)
	}
}

func TestRunRuntimeBotletsShortcutUsesSavedRuntime(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return true, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"reviewer",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	logData, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, want := range []string{"new-session", "session-exec"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("fake tmux log missing %q:\n%s", want, logText)
		}
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	records, err := commentbus.ListSessionRecords(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("session records = %#v", records)
	}
	wantCommand := []string{"claude", "--dangerously-skip-permissions"}
	if !slices.Equal(records[0].RuntimeCommand, wantCommand) {
		t.Fatalf("runtime command = %#v, want %#v", records[0].RuntimeCommand, wantCommand)
	}
}

func TestRunRuntimeProfileShortcutUsesSavedRuntime(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	writeTestAgentProfile(t, home, "max.personal", `{"agent_secret":"as_personalsecret","base_url":"https://comment.io","runtime":"claude"}`)
	if output, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home}); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, output)
	}
	var attached string
	oldAttach := runTransientRuntimeAttach
	runTransientRuntimeAttach = func(_ commentbus.Paths, record commentbus.TransientRuntimeRecord) error {
		attached = record.SessionName
		return nil
	}
	t.Cleanup(func() { runTransientRuntimeAttach = oldAttach })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"max.personal",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-run-personal-") {
		t.Fatalf("attached transient session = %q", attached)
	}
	logData, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, want := range []string{"new-session", "claude", "--dangerously-skip-permissions", "COMMENT_IO_PROFILE='max.personal'"} {
		if !strings.Contains(logText, want) {
			t.Fatalf("fake tmux log missing %q:\n%s", want, logText)
		}
	}
	if strings.Contains(logText, "session-exec") {
		t.Fatalf("plain profile shortcut should not use Botlets managed session:\n%s", logText)
	}
}

func TestRunRuntimeProfileShortcutStartsTaskRole(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	writeTestAgentProfile(t, home, "max.personal", `{"agent_secret":"as_personalsecret","base_url":"https://comment.io","runtime":"claude"}`)
	if output, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home}); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, output)
	}
	var attached commentbus.TransientRuntimeRecord
	overrideTransientRuntimeAttachForTest(t, func(record commentbus.TransientRuntimeRecord) error {
		attached = record
		return nil
	})
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--role", "task",
		"max.personal",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if attached.Profile != "max.personal" || attached.Role != commentbus.RuntimeRoleTask {
		t.Fatalf("attached transient runtime = %+v, want max.personal task", attached)
	}
	if !strings.HasPrefix(attached.SessionName, "comment-run-personal-") {
		t.Fatalf("attached transient session = %q", attached.SessionName)
	}
}

func TestRunRuntimeProfileShortcutIgnoresBrokenBotletsRegistry(t *testing.T) {
	home, botletsHome, stop := startCLITestDaemon(t)
	defer stop()
	writeTestAgentProfile(t, home, "max.personal", `{"agent_secret":"as_personalsecret","base_url":"https://comment.io","runtime":"claude"}`)
	if output, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home, "--botlets-home", botletsHome}); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var attached string
	oldAttach := runTransientRuntimeAttach
	runTransientRuntimeAttach = func(_ commentbus.Paths, record commentbus.TransientRuntimeRecord) error {
		attached = record.SessionName
		return nil
	}
	t.Cleanup(func() { runTransientRuntimeAttach = oldAttach })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"max.personal",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-run-personal-") {
		t.Fatalf("attached transient session = %q", attached)
	}
}

func TestResolveBotletsRunShortcutUsesProfileRuntime(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".comment-io")
	botletsHome := filepath.Join(root, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://comment.io","runtime":"codex"}`)
	registry := `{"bots":[{"name":"reviewer","handle":"max.reviewer","credential_profile":` + strconvQuote(profilePath) + `,"managed_session":{"enabled":true,"host":"tmux"}}]}`
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}

	entry, err := resolveBotletsRunShortcut(paths, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ManagedSession.Runtime != "codex" {
		t.Fatalf("managed runtime = %q, want codex", entry.ManagedSession.Runtime)
	}
	configured, ok := configuredManagedSessionRuntime(paths, "max.reviewer")
	if !ok {
		t.Fatal("configured managed runtime not found")
	}
	if configured.Runtime != "codex" || configured.AllowLegacyRetry {
		t.Fatalf("configured managed runtime = %+v, want codex with legacy retry disabled when legacy registry has no runtime", configured)
	}
}

func TestConfiguredManagedSessionRuntimeAllowsLegacyRetryWhenRegistryRuntimeMatchesProfile(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, ".comment-io")
	botletsHome := filepath.Join(root, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeTestAgentProfile(t, home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://comment.io","runtime":"codex"}`)
	registry := `{"bots":[{"name":"reviewer","handle":"max.reviewer","credential_profile":` + strconvQuote(profilePath) + `,"managed_session":{"enabled":true,"runtime":"codex","host":"tmux"}}]}`
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}

	configured, ok := configuredManagedSessionRuntime(paths, "max.reviewer")
	if !ok {
		t.Fatal("configured managed runtime not found")
	}
	if configured.Runtime != "codex" || !configured.AllowLegacyRetry {
		t.Fatalf("configured managed runtime = %+v, want codex with legacy retry allowed", configured)
	}
}

func TestResolveAgentProfileRunShortcutSurfacesInvalidRuntime(t *testing.T) {
	home := privateTempHome(t, "comment-cli-profile-runtime-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, home, "max.bad", `{"agent_secret":"as_badsecret","base_url":"https://comment.io","runtime":"gpt"}`)

	_, err = resolveAgentProfileRunShortcut(paths, "max.bad")
	if err == nil || !strings.Contains(err.Error(), "invalid profile runtime") {
		t.Fatalf("profile shortcut err = %v, want invalid profile runtime", err)
	}
}

func TestRunRuntimeProfileShortcutPrefersInvalidRuntimeOverBrokenBotlets(t *testing.T) {
	home := privateTempHome(t, "comment-cli-profile-runtime-botlets-")
	botletsHome := privateTempHome(t, "comment-cli-profile-runtime-botlets-home-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, home, "max.bad", `{"agent_secret":"as_badsecret","base_url":"https://comment.io","runtime":"gpt"}`)
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"max.bad",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid profile runtime") {
		t.Fatalf("run err = %v output = %s, want invalid profile runtime", err, output)
	}
	if strings.Contains(err.Error(), "could not load Botlets bot registry") {
		t.Fatalf("run err surfaced Botlets registry instead of profile runtime: %v", err)
	}
}

func TestRunRuntimeProfileShortcutPrefersAttachedBotletsBot(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return true, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"max.reviewer",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	if strings.HasPrefix(attached, "comment-run-reviewer-") {
		t.Fatalf("profile shortcut launched transient runtime instead of attached Botlets bot: %q", attached)
	}
}

func TestRunRuntimeProfileShortcutDoesNotBypassAttachedBotletsError(t *testing.T) {
	home, botletsHome, stop := startCLITestDaemon(t)
	defer stop()
	profilePath := filepath.Join(home, "agents", "max.reviewer.json")
	registry := `{"bots":[{"name":"reviewer","handle":"max.reviewer","credential_profile":` + strconvQuote(profilePath) + `,"managed_session":{"enabled":false,"runtime":"claude","host":"tmux"}}]}`
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home, "--botlets-home", botletsHome}); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, output)
	}

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"max.reviewer",
	})
	if err == nil || !strings.Contains(err.Error(), "not configured for `comment run <bot>`") {
		t.Fatalf("run err = %v output = %s", err, output)
	}
}

func TestRunRuntimeEmitsSetupTelemetryForManagedSession(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return true, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })
	oldTelemetry := emitBotletsSetupTelemetryForRuntime
	var telemetryMsg string
	var telemetryBaseURL string
	var telemetryData map[string]any
	emitBotletsSetupTelemetryForRuntime = func(_ context.Context, _ *http.Client, baseURL string, msg string, data map[string]any) {
		telemetryMsg = msg
		telemetryBaseURL = baseURL
		telemetryData = map[string]any{}
		for key, value := range data {
			telemetryData[key] = value
		}
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForRuntime = oldTelemetry })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.reviewer",
		"--setup-attempt-id", "bla_1234567890abc",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	if telemetryMsg != "botlets_local_runtime_started" || telemetryBaseURL != "https://comment.io" {
		t.Fatalf("telemetry msg/base = %q/%q data=%#v", telemetryMsg, telemetryBaseURL, telemetryData)
	}
	if telemetryData["setup_attempt_id"] != "bla_1234567890abc" || telemetryData["bot_slug"] != "reviewer" || telemetryData["runtime"] != filepath.Join(fakeBinDir, "claude") {
		t.Fatalf("telemetry data = %#v", telemetryData)
	}
}

func TestRunRuntimeEmitsFailureTelemetryOnLaunchError(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(commentbus.Paths, string) error { t.Fatalf("unexpected attach"); return nil }
	t.Cleanup(func() { runRuntimeAttach = oldAttach })

	oldTelemetry := emitBotletsSetupTelemetryForRuntime
	var telemetryMsg string
	var telemetryData map[string]any
	emitBotletsSetupTelemetryForRuntime = func(_ context.Context, _ *http.Client, _ string, msg string, data map[string]any) {
		telemetryMsg = msg
		telemetryData = data
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForRuntime = oldTelemetry })

	// A runtime mismatch (reviewer is configured for claude) fails after profile
	// resolution. No --setup-attempt-id is supplied, proving the failure emit is
	// un-gated (unlike the success emit).
	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", "codex",
		"--profile", "max.reviewer",
	})
	if err == nil {
		t.Fatalf("expected runtime mismatch error, output=%s", output)
	}
	if telemetryMsg != "botlets_local_runtime_failed" {
		t.Fatalf("telemetry msg = %q, want botlets_local_runtime_failed (data=%#v)", telemetryMsg, telemetryData)
	}
	if reason, _ := telemetryData["reason"].(string); reason == "" {
		t.Fatalf("failure telemetry missing reason: %#v", telemetryData)
	}
	if _, gated := telemetryData["setup_attempt_id"]; gated {
		t.Fatalf("failure telemetry should be un-gated, got setup_attempt_id: %#v", telemetryData)
	}
}

func TestRunRuntimeRejectsManagedRuntimeMismatchBeforeStart(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		t.Fatalf("unexpected attach to %q", sessionName)
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", "codex",
		"--profile", "max.reviewer",
	})
	if err == nil || !strings.Contains(err.Error(), "uses runtime") {
		t.Fatalf("run err = %v output = %s", err, output)
	}
	if logData, readErr := os.ReadFile(tmuxLogPath); readErr == nil && strings.Contains(string(logData), "new-session") {
		t.Fatalf("runtime mismatch launched tmux session:\n%s", string(logData))
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
}

func TestRunRuntimeRejectsUnknownManagedRuntimeBeforeStart(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		t.Fatalf("unexpected attach to %q", sessionName)
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", "definitely-missing-comment-runtime",
		"--profile", "max.reviewer",
	})
	if err == nil || !strings.Contains(err.Error(), "uses runtime") {
		t.Fatalf("run err = %v output = %s", err, output)
	}
	if logData, readErr := os.ReadFile(tmuxLogPath); readErr == nil && strings.Contains(string(logData), "new-session") {
		t.Fatalf("unknown runtime launched tmux session:\n%s", string(logData))
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}
}

func TestRunRuntimeDefaultsBotletsProfileToBrainRoot(t *testing.T) {
	home, botletsHome, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	tmuxLogPath := filepath.Join(fakeBinDir, "tmux.log")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeCLILocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	profilePath := filepath.Join(home, "agents", "max.reviewer.json")
	registry := `{"bots":[{"name":"reviewer","handle":"max.reviewer","credential_profile":` + strconvQuote(profilePath) + `,"brain_ref":{"workspace_id":"ws_brain","owner_agent_id":"ag_owner","bot_agent_id":"ag_bot","container_id":"lc_brain","root_folder_id":"lf_brain","relative_path":"Botlets/max/reviewer/brain","setup_generation":5},"managed_session":{"enabled":true,"runtime":"claude","host":"tmux"}}]}`
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := captureRun(t, []string{"bus", "reload-profiles", "--home", home, "--botlets-home", botletsHome}); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, output)
	}
	var attached string
	oldAttach := runRuntimeAttach
	runRuntimeAttach = func(_ commentbus.Paths, sessionName string) error {
		attached = sessionName
		return nil
	}
	t.Cleanup(func() { runRuntimeAttach = oldAttach })
	oldHas := runRuntimeHas
	runRuntimeHas = func(commentbus.Paths, string) (bool, error) { return false, nil }
	t.Cleanup(func() { runRuntimeHas = oldHas })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.reviewer",
		"hello",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if !strings.HasPrefix(attached, "comment-reviewer-") {
		t.Fatalf("attached tmux session = %q", attached)
	}
	logData, err := os.ReadFile(tmuxLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if logText := string(logData); !strings.Contains(logText, "-c "+brainRoot) {
		t.Fatalf("fake tmux log missing brain-root cwd %q:\n%s", brainRoot, logText)
	}
}

func TestRunRuntimeDoesNotReplayOutputWhenUserDetaches(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]

	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error { return nil })
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if strings.Contains(output, "claude --resume") {
		t.Fatalf("runtime output replayed while session was still alive: %q", output)
	}
}

func TestRunRuntimeTreatsAttachMissingSessionAsCleanExit(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]

	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error { return cliExitError{Code: 1} })
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return true })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	})
	if err != nil {
		t.Fatalf("fast-exit attach race should succeed: %v\n%s", err, output)
	}
}

func TestRunRuntimeDoesNotReplayOutputWhenSessionCheckFails(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]

	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error { return nil })
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	})
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	if strings.Contains(output, "claude --resume") {
		t.Fatalf("runtime output replayed after tmux inspect failure: %q", output)
	}
}

func TestRuntimeListPrintsTrackedRuntimes(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]

	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error { return nil })
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	if output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	}); err != nil {
		t.Fatalf("run failed: %v\n%s", err, output)
	}
	statusOutput, err := captureRun(t, []string{"runtime", "list", "--home", home, "--profile", "max.runner"})
	if err != nil {
		t.Fatalf("runtime list failed: %v\n%s", err, statusOutput)
	}
	response := resultMap(t, decodeTestSocketResponse(t, `{"ok":true,"result":`+statusOutput+`}`))
	runtimes := response["runtimes"].([]any)
	if len(runtimes) != 1 {
		t.Fatalf("runtime list runtimes = %#v", response)
	}
	status := runtimes[0].(map[string]any)
	runtimeResult := status["runtime"].(map[string]any)
	if status["health"] != "healthy" || runtimeResult["profile"] != "max.runner" || runtimeResult["role"] != "main" {
		t.Fatalf("runtime list entry = %#v", status)
	}

	mainStatusOutput, err := captureRun(t, []string{"runtime", "status", "--home", home, "--profile", "max.runner"})
	if err != nil {
		t.Fatalf("runtime status failed: %v\n%s", err, mainStatusOutput)
	}
	mainStatus := resultMap(t, decodeTestSocketResponse(t, `{"ok":true,"result":`+mainStatusOutput+`}`))
	mainRuntime := mainStatus["runtime"].(map[string]any)
	if mainStatus["health"] != "healthy" || mainRuntime["profile"] != "max.runner" || mainRuntime["role"] != "main" {
		t.Fatalf("runtime status = %#v", mainStatus)
	}
}

func TestRunRuntimeExistingAttachFailureDoesNotStopRuntime(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	attachCalls := 0
	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error {
		attachCalls++
		if attachCalls == 2 {
			return errors.New("attach failed")
		}
		return nil
	})
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	if output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	}); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, output)
	}
	if output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	}); err == nil || !strings.Contains(err.Error(), "attach failed") {
		t.Fatalf("second run err = %v output = %s", err, output)
	}
	statusOutput, err := captureRun(t, []string{"runtime", "list", "--home", home, "--profile", "max.runner"})
	if err != nil {
		t.Fatalf("runtime list failed: %v\n%s", err, statusOutput)
	}
	response := resultMap(t, decodeTestSocketResponse(t, `{"ok":true,"result":`+statusOutput+`}`))
	runtimes := response["runtimes"].([]any)
	if len(runtimes) != 1 {
		t.Fatalf("existing runtime was forgotten after failed attach: %#v", response)
	}
}

func TestRunRuntimeAttachesExistingWhenRuntimePathMissing(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeBinDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	overrideTransientRuntimeAttachForTest(t, func(commentbus.TransientRuntimeRecord) error { return nil })
	overrideTransientRuntimeExitedForTest(t, func(commentbus.TransientRuntimeRecord) bool { return false })

	if output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", filepath.Join(fakeBinDir, "claude"),
		"--profile", "max.runner",
	}); err != nil {
		t.Fatalf("first run failed: %v\n%s", err, output)
	}
	output, err := captureRun(t, []string{
		"run",
		"--home", home,
		"--runtime", "definitely-missing-comment-runtime",
		"--profile", "max.runner",
	})
	if err != nil {
		t.Fatalf("existing run should attach without resolving missing runtime: %v\n%s", err, output)
	}
}

func TestRuntimeTmuxSessionExistsDistinguishesMissingFromInspectFailure(t *testing.T) {
	dir := privateTempHome(t, "comment-cli-tmux-has-")
	tmuxPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
case "${TMUX_TEST_MODE:-ok}" in
  missing)
    echo "can't find session" >&2
    exit 1
    ;;
  fail)
    echo "tmux socket permission denied" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMMENT_IO_TMUX_BIN", "") // exercise the PATH fallback, not a pin

	t.Setenv("TMUX_TEST_MODE", "ok")
	live, err := runtimeTmuxSessionExists(commentbus.Paths{}, "comment-reviewer-abc123")
	if err != nil || !live {
		t.Fatalf("runtimeTmuxSessionExists ok = %v, %v", live, err)
	}

	t.Setenv("TMUX_TEST_MODE", "missing")
	live, err = runtimeTmuxSessionExists(commentbus.Paths{}, "comment-reviewer-abc123")
	if err != nil || live {
		t.Fatalf("runtimeTmuxSessionExists missing = %v, %v", live, err)
	}

	t.Setenv("TMUX_TEST_MODE", "fail")
	live, err = runtimeTmuxSessionExists(commentbus.Paths{}, "comment-reviewer-abc123")
	if err == nil || live {
		t.Fatalf("runtimeTmuxSessionExists failure = %v, %v", live, err)
	}
}

func TestAttachRuntimeTmuxSessionUsesIsolatedSocket(t *testing.T) {
	dir := privateTempHome(t, "comment-cli-tmux-attach-")
	tmuxPath := filepath.Join(dir, "tmux")
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$TMUX_TEST_LOG"
exit 0
`
	if err := os.WriteFile(tmuxPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COMMENT_IO_TMUX_BIN", "") // exercise the PATH fallback, not a pin
	t.Setenv("TMUX_TEST_LOG", logPath)

	t.Setenv("TMUX", "")
	if err := attachRuntimeTmuxSession(commentbus.Paths{}, "comment-run-reviewer-abc123"); err != nil {
		t.Fatalf("attach outside tmux failed: %v", err)
	}

	t.Setenv("TMUX", "/tmp/tmux-501/default,123,0")
	if err := attachRuntimeTmuxSession(commentbus.Paths{}, "comment-run-reviewer-def456"); err != nil {
		t.Fatalf("attach inside different tmux server failed: %v", err)
	}

	t.Setenv("TMUX", "/tmp/tmux-501/comment-run-reviewer-ghi789,123,0")
	if err := attachRuntimeTmuxSession(commentbus.Paths{}, "comment-run-reviewer-ghi789"); err != nil {
		t.Fatalf("switch inside same tmux server failed: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logData)), "\n")
	want := []string{
		"-L comment-run-reviewer-abc123 attach-session -t comment-run-reviewer-abc123",
		"-L comment-run-reviewer-def456 attach-session -t comment-run-reviewer-def456",
		"-L comment-run-reviewer-ghi789 switch-client -t comment-run-reviewer-ghi789",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("tmux commands = %#v, want %#v", lines, want)
	}
}

func TestAttachRuntimeTmuxSessionMissingTmuxIsClear(t *testing.T) {
	// PATH points only at an empty dir and no pin is set, so tmux cannot be found.
	emptyDir := privateTempHome(t, "comment-cli-no-tmux-")
	t.Setenv("PATH", emptyDir)
	t.Setenv("TMUX", "")
	t.Setenv("COMMENT_IO_TMUX_BIN", "")

	err := attachRuntimeTmuxSession(commentbus.Paths{}, "comment-run-reviewer-abc123")
	if err == nil {
		t.Fatal("expected attach to fail when tmux is not installed")
	}
	var exitErr cliExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("error = %T %v; want cliExitError", err, err)
	}
	if exitErr.Code != exitTmuxMissing {
		t.Fatalf("exit code = %d; want %d (exitTmuxMissing)", exitErr.Code, exitTmuxMissing)
	}
	if !strings.Contains(exitErr.Message, "tmux is required") {
		t.Fatalf("message should explain tmux is required + how to install: %q", exitErr.Message)
	}
}

func TestAttachRuntimeTmuxSessionHonorsTmuxBinPin(t *testing.T) {
	// tmux is NOT on PATH, but the user pinned it via COMMENT_IO_TMUX_BIN (the
	// exact override the missing-tmux guidance tells them to set). Attach must
	// exec the pinned binary rather than failing with a "set COMMENT_IO_TMUX_BIN"
	// message that re-running with the pin wouldn't fix.
	dir := privateTempHome(t, "comment-cli-tmux-pin-")
	pinned := filepath.Join(dir, "my-tmux")
	logPath := filepath.Join(dir, "tmux.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TMUX_TEST_LOG\"\nexit 0\n"
	if err := os.WriteFile(pinned, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(dir, "emptybin")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", emptyDir) // no tmux on PATH
	t.Setenv("TMUX_TEST_LOG", logPath)
	t.Setenv("TMUX", "")
	t.Setenv("COMMENT_IO_TMUX_BIN", pinned)

	if err := attachRuntimeTmuxSession(commentbus.Paths{}, "comment-run-reviewer-abc123"); err != nil {
		t.Fatalf("attach with pinned tmux failed: %v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("pinned tmux was not executed: %v", err)
	}
	if !strings.Contains(string(logData), "attach-session -t comment-run-reviewer-abc123") {
		t.Fatalf("pinned tmux invocation = %q", string(logData))
	}
}

func TestRuntimeTmuxSessionExistsMissingTmuxIsClear(t *testing.T) {
	emptyDir := privateTempHome(t, "comment-cli-no-tmux-has-")
	t.Setenv("PATH", emptyDir)
	t.Setenv("COMMENT_IO_TMUX_BIN", "")

	_, err := runtimeTmuxSessionExists(commentbus.Paths{}, "comment-reviewer-abc123")
	if err == nil {
		t.Fatal("expected has-session to fail when tmux is not installed")
	}
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != exitTmuxMissing {
		t.Fatalf("error = %v; want cliExitError code %d", err, exitTmuxMissing)
	}
}

func TestExitForErrorMapsMissingTmux(t *testing.T) {
	// Daemon-originated socket error → dedicated exit code + the clear message.
	sockErr := cliSocketError{Code: commentbus.SocketErrorCodeTmuxNotInstalled, Message: "ignored; CLI builds its own local message"}
	code, stderr := exitForError(sockErr)
	if code != exitTmuxMissing {
		t.Fatalf("socket TMUX_NOT_INSTALLED exit code = %d; want %d", code, exitTmuxMissing)
	}
	if !strings.Contains(stderr, "tmux is required") {
		t.Fatalf("socket TMUX_NOT_INSTALLED stderr = %q; want install guidance", stderr)
	}

	// A cliExitError with no message stays silent (forwarding a child's status).
	if code, stderr := exitForError(cliExitError{Code: 7}); code != 7 || stderr != "" {
		t.Fatalf("silent cliExitError = (%d, %q); want (7, \"\")", code, stderr)
	}

	// Generic errors stay exit 1 with their text.
	if code, stderr := exitForError(errors.New("boom")); code != 1 || stderr != "boom" {
		t.Fatalf("generic error = (%d, %q); want (1, \"boom\")", code, stderr)
	}

	// Other socket errors are not remapped.
	if code, _ := exitForError(cliSocketError{Code: "UPSTREAM_ERROR", Message: "nope"}); code != 1 {
		t.Fatalf("non-tmux socket error exit code = %d; want 1", code)
	}
}

func TestPrintRuntimeExitOutputShowsRecentLines(t *testing.T) {
	logPath := filepath.Join(privateTempHome(t, "comment-cli-runtime-output-"), "runtime.log")
	lines := make([]string, 0, 90)
	for i := 0; i < 90; i++ {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	printRuntimeExitOutput(logPath)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	if strings.Contains(output, "line-0") || !strings.Contains(output, "line-89") {
		t.Fatalf("runtime output tail = %q", output)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime output log should be removed after replay, stat err=%v", err)
	}
}

func TestPrintRuntimeExitOutputWaitsForFinalTailUpdate(t *testing.T) {
	logPath := filepath.Join(privateTempHome(t, "comment-cli-runtime-output-wait-"), "runtime.log")
	if err := os.WriteFile(logPath, []byte("old output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	updated := make(chan struct{})
	go func() {
		defer close(updated)
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(logPath, []byte("claude --resume sess_123\n"), 0o600)
	}()

	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	printRuntimeExitOutput(logPath)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout
	<-updated
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	output := string(data)
	if !strings.Contains(output, "claude --resume sess_123") {
		t.Fatalf("runtime output tail = %q", output)
	}
}

func TestCopyRuntimeOutputTailKeepsBoundedBytes(t *testing.T) {
	logPath := filepath.Join(privateTempHome(t, "comment-cli-runtime-tail-"), "runtime.log")
	input := strings.Repeat("x", runtimeTailDefaultBytes+100) + "\nresume line\n"
	var output strings.Builder
	if err := copyRuntimeOutputTail(strings.NewReader(input), &output, logPath, 1024); err != nil {
		t.Fatal(err)
	}
	if output.String() != input {
		t.Fatal("runtime tailer did not preserve live output")
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(logData) > 1024 {
		t.Fatalf("runtime tail log length = %d, want <= 1024", len(logData))
	}
	if !strings.Contains(string(logData), "resume line") {
		t.Fatalf("runtime tail log missing final output: %q", string(logData))
	}
}

func TestResolveRuntimeCommandPathAbsolute(t *testing.T) {
	got, err := resolveRuntimeCommandPath("/home/user/.local/bin/claude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/home/user/.local/bin/claude" {
		t.Fatalf("got %q, want absolute pass-through", got)
	}
}

func TestResolveRuntimeCommandPathBareNameFromPATH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-runtime")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := resolveRuntimeCommandPath("my-runtime")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bin {
		t.Fatalf("got %q, want %q", got, bin)
	}
}

func TestResolveRuntimeCommandPathBareNameNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := resolveRuntimeCommandPath("definitely-not-installed-runtime")
	if err == nil {
		t.Fatal("expected error when runtime is not on PATH")
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("error should hint about PATH; got %v", err)
	}
}

func TestResolveRuntimeCommandPathHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveRuntimeCommandPath("~/.local/bin/claude")
	if err != nil {
		t.Fatalf("unexpected error expanding ~: %v", err)
	}
	if got != bin {
		t.Fatalf("got %q, want %q", got, bin)
	}
}

func TestResolveRuntimeCommandPathRelative(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wrapper")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRuntimeCommandPath("./wrapper")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("expected absolute path, got %q", got)
	}
}

func TestAttachRuntimeBmuxSessionUsesCanonicalSocketAndFiltersEnv(t *testing.T) {
	userHome := privateTempHome(t, "comment-cli-bmux-attach-user-")
	t.Setenv("HOME", userHome)
	home := privateTempHome(t, "comment-cli-bmux-attach-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	sessionName := "comment-reviewer-abc123"
	socketPath, err := commentbus.BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	tokenFile, err := commentbus.BmuxTokenFileForSession(paths, sessionName)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WritePrivateFileAtomic(tokenFile, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argsOut := filepath.Join(home, "bmux-args.txt")
	envOut := filepath.Join(home, "bmux-env.txt")
	shadowOut := filepath.Join(home, "bmux-shadow.txt")
	bmuxBin := filepath.Join(home, "bmux")
	script := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"status\" ]; then printf 'pid 12345\\nchild_pgid 12345\\nchild_alive false\\nforeground \\n'; exit 0; fi",
		"printf '%s\\n' \"$@\" > " + shellQuoteArg(argsOut),
		"printf 'token=%s\\nfile=%s\\npath=%s\\nld=%s\\ndyld=%s\\ncustom=%s\\n' \"${BMUX_TOKEN-}\" \"${BMUX_TOKEN_FILE-}\" \"${PATH-}\" \"${LD_LIBRARY_PATH-}\" \"${DYLD_INSERT_LIBRARIES-}\" \"${COMMENT_IO_BMUX_BIN-}\" > " + shellQuoteArg(envOut),
		"",
	}, "\n")
	if err := os.WriteFile(bmuxBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	label := launchdLabelForHome(paths.Home)
	launchAgents := filepath.Join(userHome, "Library", "LaunchAgents")
	if err := os.MkdirAll(launchAgents, 0o700); err != nil {
		t.Fatal(err)
	}
	plist := buildLaunchAgentPlist(launchAgentConfig{
		Label:            label,
		Home:             paths.Home,
		BmuxBinary:       bmuxBin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run", "--home", paths.Home},
	})
	if err := os.WriteFile(filepath.Join(launchAgents, label+".plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	shadowDir := filepath.Join(home, "shadow-bin")
	if err := os.MkdirAll(shadowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shadowDir, "bmux"), []byte("#!/bin/sh\nprintf shadow > \"$BMUX_SHADOW_OUT\"\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BMUX_BIN", filepath.Join(home, "wrong-bmux"))
	t.Setenv("BMUX_SHADOW_OUT", shadowOut)
	t.Setenv("BMUX_TOKEN", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	t.Setenv("BMUX_TOKEN_FILE", filepath.Join(home, "stale.token"))
	t.Setenv("LD_LIBRARY_PATH", filepath.Join(home, "loader"))
	t.Setenv("DYLD_INSERT_LIBRARIES", filepath.Join(home, "loader.dylib"))
	t.Setenv("PATH", shadowDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	record := commentbus.SessionRecord{
		Host:        commentbus.SessionHostBmux,
		SessionName: sessionName,
		PaneTarget:  socketPath,
	}
	if err := attachRuntimeBmuxSession(paths, record); err != nil {
		t.Fatal(err)
	}
	argsData, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(argsData), "attach\n-S\n"+socketPath+"\n"; got != want {
		t.Fatalf("bmux args = %q, want %q", got, want)
	}
	envData, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	envLines := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(envData), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			envLines[key] = value
		}
	}
	if envLines["token"] != "" || envLines["file"] != tokenFile || envLines["ld"] != "" || envLines["dyld"] != "" || envLines["custom"] != "" {
		t.Fatalf("bmux env = %q", string(envData))
	}
	if strings.Contains(envLines["path"], shadowDir) {
		t.Fatalf("bmux attach inherited caller PATH %q", envLines["path"])
	}
	if _, err := os.Stat(shadowOut); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PATH-shadowed bmux was invoked, stat err=%v", err)
	}
	controller := managedRuntimeBmuxController(paths)
	if controller.Binary != bmuxBin {
		t.Fatalf("managedRuntimeBmuxController Binary = %q, want service pin %q", controller.Binary, bmuxBin)
	}
	if !managedSessionExited(paths, record) {
		t.Fatal("managedSessionExited should treat bmux child_alive=false as runtime exited even while the control socket can answer status")
	}

	transientArgsOut := filepath.Join(home, "transient-bmux-args.txt")
	transientStatusOut := filepath.Join(home, "transient-bmux-status.txt")
	transientBmuxBin := filepath.Join(home, "transient-bmux")
	transientScript := strings.Join([]string{
		"#!/bin/sh",
		"if [ \"$1\" = \"status\" ]; then printf status > " + shellQuoteArg(transientStatusOut) + "; printf 'pid 23456\\nchild_pgid 23456\\nchild_alive false\\nforeground \\n'; exit 0; fi",
		"printf '%s\\n' \"$@\" > " + shellQuoteArg(transientArgsOut),
		"",
	}, "\n")
	if err := os.WriteFile(transientBmuxBin, []byte(transientScript), 0o700); err != nil {
		t.Fatal(err)
	}
	transientRecord := commentbus.TransientRuntimeRecord{
		Host:        commentbus.SessionHostBmux,
		BmuxBinary:  transientBmuxBin,
		SessionName: sessionName,
		PaneTarget:  socketPath,
	}
	if err := attachTransientRuntimeSession(paths, transientRecord); err != nil {
		t.Fatal(err)
	}
	transientArgsData, err := os.ReadFile(transientArgsOut)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(transientArgsData), "attach\n-S\n"+socketPath+"\n"; got != want {
		t.Fatalf("transient bmux args = %q, want %q", got, want)
	}
	if !transientRuntimeSessionExited(paths, transientRecord) {
		t.Fatal("transientRuntimeSessionExited should use the transient bmux pin")
	}
	if _, err := os.Stat(transientStatusOut); err != nil {
		t.Fatalf("transient bmux status was not invoked: %v", err)
	}

	cleanEquivalent := record
	cleanEquivalent.PaneTarget = filepath.Dir(socketPath) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(socketPath)
	if filepath.Clean(cleanEquivalent.PaneTarget) != socketPath {
		t.Fatalf("test setup clean-equivalent pane target = %q, clean = %q, want %q", cleanEquivalent.PaneTarget, filepath.Clean(cleanEquivalent.PaneTarget), socketPath)
	}
	if err := attachRuntimeBmuxSession(paths, cleanEquivalent); err == nil {
		t.Fatal("attachRuntimeBmuxSession accepted clean-equivalent non-canonical socket path")
	}
	bad := record
	bad.PaneTarget = filepath.Join(paths.Bus, "bmux", "other.sock")
	if err := attachRuntimeBmuxSession(paths, bad); err == nil {
		t.Fatal("attachRuntimeBmuxSession accepted non-canonical socket path")
	}
}

func TestShouldRetryWithoutRuntimePath(t *testing.T) {
	cases := []struct {
		name string
		resp commentbus.SocketResponse
		want bool
	}{
		{
			name: "success not retried",
			resp: commentbus.SocketResponse{OK: true},
			want: false,
		},
		{
			name: "unrelated validation error not retried",
			resp: commentbus.SocketResponse{Error: &commentbus.SocketError{Code: "VALIDATION_ERROR", Message: "invalid cwd"}},
			want: false,
		},
		{
			name: "forbidden not retried",
			resp: commentbus.SocketResponse{Error: &commentbus.SocketError{Code: "FORBIDDEN", Message: "no"}},
			want: false,
		},
		{
			name: "unexpected param triggers retry",
			resp: commentbus.SocketResponse{Error: &commentbus.SocketError{Code: "VALIDATION_ERROR", Message: "unexpected param"}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRetryWithoutRuntimePath(tc.resp); got != tc.want {
				t.Fatalf("shouldRetryWithoutRuntimePath = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateManagedSessionRuntime(t *testing.T) {
	record := commentbus.SessionRecord{Profile: "max.reviewer", Runtime: "claude"}
	if err := validateManagedSessionRuntime(record, "claude"); err != nil {
		t.Fatalf("matching runtime should pass: %v", err)
	}
	if err := validateManagedSessionRuntime(record, "/home/user/.local/bin/claude"); err != nil {
		t.Fatalf("matching runtime path should pass: %v", err)
	}
	if err := validateManagedSessionRuntime(record, "/home/user/.local/bin/codex"); err == nil || !strings.Contains(err.Error(), "uses runtime") {
		t.Fatalf("mismatched runtime err = %v", err)
	}
	if err := validateManagedSessionRuntime(record, "/home/user/.local/share/claude/versions/2.1.152"); err == nil || !strings.Contains(err.Error(), "uses runtime") {
		t.Fatalf("opaque runtime path err = %v", err)
	}
}

func TestRuntimeRunEnvStripsManagedSessionState(t *testing.T) {
	env := runtimeRunEnv([]string{
		"PATH=/bin",
		"COMMENT_IO_HOME=/old",
		"COMMENT_IO_PROFILE=max.old",
		"COMMENT_IO_BOT_NAME=reviewer",
		"COMMENT_IO_SESSION_ID=sess_old",
		"COMMENT_IO_SESSION_GENERATION=gen_old",
		"COMMENT_IO_SESSION_CAP_FILE=/tmp/cap",
		"COMMENT_IO_RUNTIME_RUN=1",
	}, "/comment-home", "max.reviewer")
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{
		"COMMENT_IO_HOME=/old",
		"COMMENT_IO_PROFILE=max.old",
		"COMMENT_IO_BOT_NAME=",
		"COMMENT_IO_SESSION_ID=",
		"COMMENT_IO_SESSION_GENERATION=",
		"COMMENT_IO_SESSION_CAP_FILE=",
		"COMMENT_IO_RUNTIME_RUN=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("env leaked %q in %#v", forbidden, env)
		}
	}
	for _, required := range []string{"PATH=/bin", "COMMENT_IO_HOME=/comment-home", "COMMENT_IO_PROFILE=max.reviewer"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("env missing %q in %#v", required, env)
		}
	}
}

func TestMessagesReleaseCLIParsesFlagsAfterMessageID(t *testing.T) {
	claimID := "clm_climessagerelease1234567890"
	notificationID := "ntf_climessagerelease1234567890"
	claimedAt := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	leaseExpiresAt := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	client := &cliFakeNotificationClient{
		leases: []*commentbus.CloudNotificationLease{{
			ClaimID:        claimID,
			NotificationID: notificationID,
			ClaimedAt:      claimedAt,
			LeaseExpiresAt: leaseExpiresAt,
			Notification: commentbus.CloudNotification{
				ID:         notificationID,
				Type:       "mention",
				DocSlug:    "abc123",
				DocTitle:   "CLI Message Release",
				FromHandle: "max.sender",
				Context:    "Release this message from the CLI.",
				CreatedAt:  claimedAt,
			},
		}},
	}
	home, botletsHome, stop := startCLITestDaemonWithNotificationClient(t, client)
	defer stop()

	waitOut, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err != nil {
		t.Fatalf("messages wait failed: %v\n%s", err, waitOut)
	}
	messageID := resultMap(t, decodeTestSocketResponse(t, waitOut))["message_id"].(string)
	receiveOut, err := captureRun(t, []string{
		"messages", "receive",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages receive failed: %v\n%s", err, receiveOut)
	}
	opID := "op_climessagerelease1234"
	releaseOut, err := captureRun(t, []string{
		"messages", "release",
		messageID,
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		"--op-id", opID,
		"--reason", "cli reason after positional",
	})
	if err != nil {
		t.Fatalf("messages release failed: %v\n%s", err, releaseOut)
	}
	releaseResult := resultMap(t, decodeTestSocketResponse(t, releaseOut))
	if releaseResult["state"] != "released" {
		t.Fatalf("release result = %#v", releaseResult)
	}
	client.mu.Lock()
	mutations := append([]cliNotificationMutationCall{}, client.mutations...)
	client.mu.Unlock()
	releaseMutation := cliNotificationMutationCall{}
	for _, mutation := range mutations {
		if mutation.Operation == "release" {
			releaseMutation = mutation
		}
	}
	if releaseMutation.Operation != "release" || releaseMutation.ClaimID != claimID || releaseMutation.OpID != opID {
		t.Fatalf("message release mutations = %+v", mutations)
	}
}

func TestActivityCompleteCLIRequiresManagedSessionOrRuntimeProfile(t *testing.T) {
	home := privateTempHome(t, "comment-cli-activity-complete-session-")
	output, err := captureRun(t, []string{
		"activity", "complete", "msg_abcdefghijklmnopqrst",
		"--home", home,
	})
	if err == nil || !strings.Contains(err.Error(), "activity complete requires a managed comment run session or runtime profile") {
		t.Fatalf("activity complete err = %v output = %s", err, output)
	}
}

func TestActivityCompleteCLIUsesTransientRuntimeProfile(t *testing.T) {
	claimID := "clm_cliactivitycomplete1234567890"
	notificationID := "ntf_cliactivitycomplete1234567890"
	claimedAt := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	leaseExpiresAt := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	client := &cliFakeNotificationClient{
		leases: []*commentbus.CloudNotificationLease{{
			ClaimID:        claimID,
			NotificationID: notificationID,
			ClaimedAt:      claimedAt,
			LeaseExpiresAt: leaseExpiresAt,
			Notification: commentbus.CloudNotification{
				ID:         notificationID,
				Type:       "mention",
				DocSlug:    "abc123",
				DocTitle:   "CLI Activity Complete",
				FromHandle: "max.sender",
				Context:    "Finish without a visible reply.",
				CreatedAt:  claimedAt,
			},
		}},
	}
	home, botletsHome, stop := startCLITestDaemonWithNotificationClient(t, client)
	defer stop()

	waitOut, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--botlets-home", botletsHome,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err != nil {
		t.Fatalf("messages wait failed: %v\n%s", err, waitOut)
	}
	messageID := resultMap(t, decodeTestSocketResponse(t, waitOut))["message_id"].(string)
	receiveOut, err := captureRun(t, []string{
		"messages", "receive",
		"--home", home,
		"--botlets-home", botletsHome,
		"--profile", "max.reviewer",
		messageID,
	})
	if err != nil {
		t.Fatalf("messages receive failed: %v\n%s", err, receiveOut)
	}

	t.Setenv("COMMENT_IO_RUNTIME_RUN", "1")
	t.Setenv("COMMENT_IO_PROFILE", "max.reviewer")
	opID := "op_cliactivitycomplete1234"
	completeOut, err := captureRun(t, []string{
		"activity", "complete",
		messageID,
		"--home", home,
		"--op-id", opID,
	})
	if err != nil {
		t.Fatalf("activity complete failed: %v\n%s", err, completeOut)
	}
	completeResult := resultMap(t, decodeTestSocketResponse(t, completeOut))
	if completeResult["state"] != "acked" {
		t.Fatalf("activity complete result = %#v", completeResult)
	}

	client.mu.Lock()
	handling := append([]cliNotificationHandlingCall{}, client.handlingActivities...)
	mutations := append([]cliNotificationMutationCall{}, client.mutations...)
	client.mu.Unlock()
	completes := 0
	for _, call := range handling {
		if call.Action == "complete" && call.Outcome == "no_response" && call.ClaimID == claimID {
			completes++
		}
	}
	if completes != 1 {
		t.Fatalf("handling activities = %+v", handling)
	}
	acks := 0
	for _, mutation := range mutations {
		if mutation.Operation == "ack" && mutation.ClaimID == claimID && mutation.OpID == opID {
			acks++
		}
	}
	if acks != 1 {
		t.Fatalf("activity complete mutations = %+v", mutations)
	}
}

func TestNotificationsCommandRemoved(t *testing.T) {
	output, err := captureRun(t, []string{"notifications", "ack", "clm_removed1234567890"})
	if err == nil || !strings.Contains(err.Error(), "comment notifications has been removed") {
		t.Fatalf("notifications removed err = %v output = %s", err, output)
	}
}

func TestSessionsCLIUsesDaemonSocket(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	startResult := resultMap(t, decodeTestSocketResponse(t, startOut))
	session := startResult["session"].(map[string]any)
	sessionID := session["session_id"].(string)

	statusOut, err := captureRun(t, []string{"sessions", "status", "--home", home, "--session-id", sessionID})
	if err != nil {
		t.Fatalf("sessions status failed: %v\n%s", err, statusOut)
	}
	statusResult := resultMap(t, decodeTestSocketResponse(t, statusOut))
	sessions := statusResult["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions status count = %d", len(sessions))
	}

	stopOut, err := captureRun(t, []string{"sessions", "stop", "--home", home, "--session-id", sessionID})
	if err != nil {
		t.Fatalf("sessions stop failed: %v\n%s", err, stopOut)
	}
	stopResult := resultMap(t, decodeTestSocketResponse(t, stopOut))
	if stopResult["state"] != "dead" {
		t.Fatalf("stop state = %#v", stopResult["state"])
	}
}

func TestSessionsCLIAcceptsFlagsAfterPositionals(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "reviewer", "--home", home})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	sessionID := session["session_id"].(string)

	statusOut, err := captureRun(t, []string{"sessions", "status", "reviewer", "--home", home, "--session-id", sessionID})
	if err != nil {
		t.Fatalf("sessions status failed: %v\n%s", err, statusOut)
	}
	if sessions := resultMap(t, decodeTestSocketResponse(t, statusOut))["sessions"].([]any); len(sessions) != 1 {
		t.Fatalf("sessions status count = %d", len(sessions))
	}

	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "flags after positionals",
	})
	if err != nil {
		t.Fatalf("message send failed: %v\n%s", err, sendOut)
	}
	messageID := resultMap(t, decodeTestSocketResponse(t, sendOut))["messages"].([]any)[0].(map[string]any)["id"].(string)
	nudgeOut, err := captureRun(t, []string{"sessions", "nudge", "reviewer", messageID, "--home", home})
	if err != nil {
		t.Fatalf("sessions nudge failed: %v\n%s", err, nudgeOut)
	}

	stopOut, err := captureRun(t, []string{"sessions", "stop", "reviewer", "--home", home})
	if err != nil {
		t.Fatalf("sessions stop failed: %v\n%s", err, stopOut)
	}
	if resultMap(t, decodeTestSocketResponse(t, stopOut))["state"] != "dead" {
		t.Fatalf("stop response = %s", stopOut)
	}
}

func TestSessionsCLIRejectsExtraPositionals(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "start rejects surplus after bot flag",
			args: []string{"sessions", "start", "--bot", "reviewer", "otherbot"},
			want: "sessions start accepts at most one positional bot",
		},
		{
			name: "start rejects multiple positional bots",
			args: []string{"sessions", "start", "reviewer", "otherbot"},
			want: "sessions start accepts at most one positional bot",
		},
		{
			name: "status rejects surplus after bot flag",
			args: []string{"sessions", "status", "--bot", "reviewer", "otherbot"},
			want: "sessions status accepts at most one positional bot",
		},
		{
			name: "stop rejects surplus after bot flag",
			args: []string{"sessions", "stop", "--bot", "reviewer", "otherbot"},
			want: "sessions stop accepts at most one positional bot",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := captureRun(t, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("extra positional err = %v output = %s", err, output)
			}
		})
	}
}

func TestSessionsCLIRejectsMissingFlagValueAfterPositionals(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	output, err := captureRun(t, []string{"sessions", "status", "reviewer", "--home"})
	if err == nil || !strings.Contains(err.Error(), "flag needs an argument") {
		t.Fatalf("missing flag value err = %v output = %s", err, output)
	}
	workingOut, err := captureRun(t, []string{"sessions", "status", "reviewer", "--home", home})
	if err != nil {
		t.Fatalf("sessions status with explicit value failed: %v\n%s", err, workingOut)
	}
}

func TestManagedSessionCLIRejectsMismatchedSelectors(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", session["capability_file"].(string))

	if output, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "writer",
		"--to", "reviewer",
		"--body", "wrong sender",
	}); err == nil || !strings.Contains(err.Error(), "selected bot does not match managed session") {
		t.Fatalf("mismatched session bot err = %v output = %s", err, output)
	}
	if output, err := captureRun(t, []string{
		"messages", "receive",
		"--home", home,
		"--profile", "max.writer",
		"msg_ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	}); err == nil || !strings.Contains(err.Error(), "selected profile does not match managed session") {
		t.Fatalf("mismatched session profile err = %v output = %s", err, output)
	}

	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "session sender",
	})
	if err != nil {
		t.Fatalf("matching session send failed: %v\n%s", err, sendOut)
	}
	if output, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"}); err == nil || !strings.Contains(err.Error(), "owner-only command is not allowed from a managed session") {
		t.Fatalf("owner-only session start err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIUsesCommentHomeEnvForNudgeCommand(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	sendOut, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body", "custom home receive",
	})
	if err != nil {
		t.Fatalf("message send failed: %v\n%s", err, sendOut)
	}
	message := resultMap(t, decodeTestSocketResponse(t, sendOut))["messages"].([]any)[0].(map[string]any)
	messageID := message["id"].(string)

	t.Setenv("COMMENT_IO_HOME", home)
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", session["capability_file"].(string))
	receiveOut, err := captureRun(t, []string{"messages", "receive", messageID})
	if err != nil {
		t.Fatalf("messages receive should use COMMENT_IO_HOME: %v\n%s", err, receiveOut)
	}
	received := resultMap(t, decodeTestSocketResponse(t, receiveOut))["message"].(map[string]any)
	if received["id"] != messageID {
		t.Fatalf("received message = %#v, want %s", received, messageID)
	}
}

func TestManagedSessionCLIRejectsWrongHomeBeforeReadingCapability(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	capabilityFile := session["capability_file"].(string)
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	stop()
	if err := os.Rename(capabilityFile, capabilityFile+".moved"); err != nil {
		t.Fatal(err)
	}
	wrongHome, err := os.MkdirTemp("/tmp", "comment-cli-wrong-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wrongHome) })
	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", wrongHome,
		"--bot", "reviewer",
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file does not match selected home") {
		t.Fatalf("wrong-home session err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsInvalidEnvBeforeReadingCapability(t *testing.T) {
	home := t.TempDir()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("cap_thisShouldNotBeReadBySessionAuth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", ".")
	t.Setenv("COMMENT_IO_BOT_NAME", ".")
	t.Setenv("COMMENT_IO_SESSION_ID", ".")
	t.Setenv("COMMENT_IO_SESSION_GENERATION", "owner")
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", paths.OwnerCapability)

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "invalid managed-session environment") {
		t.Fatalf("invalid env err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsPartialEnvBeforeOwnerFallback(t *testing.T) {
	home := t.TempDir()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("cap_thisShouldNotBeReadByPartialSessionEnv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", "max.reviewer")
	t.Setenv("COMMENT_IO_BOT_NAME", "reviewer")

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "incomplete managed-session environment") {
		t.Fatalf("partial env err = %v output = %s", err, output)
	}
}

func TestSocketAuthAllowsExplicitProfileWithProfileOnlyEnv(t *testing.T) {
	home := privateTempHome(t, "comment-cli-profile-only-env-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commentbus.EnsureOwnerCapability(paths); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", "max.staging-test")

	auth, err := socketAuth(context.Background(), paths, socketAuthOptions{
		Profile:      "max.staging-test",
		NeedProfile:  true,
		AllowSession: true,
	})
	if err != nil {
		t.Fatalf("profile-only env with explicit profile should fall through to owner auth: %v", err)
	}
	if auth == nil || auth.Mode != "owner" || auth.Profile == nil || *auth.Profile != "max.staging-test" {
		t.Fatalf("auth = %#v, want owner auth scoped to profile", auth)
	}
	if _, err := ownerOnlyAuth(paths, "max.staging-test"); err == nil || !strings.Contains(err.Error(), "incomplete managed-session environment") {
		t.Fatalf("ownerOnlyAuth should still reject profile-only env; got %v", err)
	}
}

func TestSessionAuthFromEnvSkipsTransientRuntimeContext(t *testing.T) {
	// Regression: when a transient runtime started by
	// `comment run --runtime <bin> --profile <handle>` sets
	// COMMENT_IO_RUNTIME_RUN=1 and COMMENT_IO_PROFILE (but no bot/session
	// capability vars), follow-up `comment messages …` invocations from
	// inside the runtime must not error with
	// "incomplete managed-session environment". Instead, sessionAuthFromEnv
	// should return ok=false so callers fall through to owner auth scoped
	// to the profile.
	home := t.TempDir()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_RUNTIME_RUN", "1")
	t.Setenv("COMMENT_IO_PROFILE", "max.staging-test")
	t.Setenv("COMMENT_IO_BOT_NAME", "")
	t.Setenv("COMMENT_IO_SESSION_ID", "")
	t.Setenv("COMMENT_IO_SESSION_GENERATION", "")
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", "")

	auth, bot, present, err := sessionAuthFromEnv(paths)
	if err != nil {
		t.Fatalf("transient runtime env should not produce an error; got %v", err)
	}
	if present {
		t.Fatalf("transient runtime env should not be detected as a managed session; got auth=%#v bot=%q", auth, bot)
	}
	if auth != nil {
		t.Fatalf("auth should be nil for transient runtime; got %#v", auth)
	}
}

func TestSessionAuthFromEnvDoesNotBypassManagedSessionWhenRuntimeFlagPresent(t *testing.T) {
	home := privateTempHome(t, "comment-cli-runtime-managed-env-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	record := installManagedSessionEnv(t, paths, "max.reviewer", "reviewer")
	t.Setenv("COMMENT_IO_RUNTIME_RUN", "1")

	auth, bot, present, err := sessionAuthFromEnv(paths)
	if err != nil {
		t.Fatalf("managed-session env should still validate with runtime marker; got %v", err)
	}
	if !present || auth == nil || auth.Mode != "session" || bot != "reviewer" {
		t.Fatalf("managed-session auth bypassed: auth=%#v bot=%q present=%v", auth, bot, present)
	}
	if auth.SessionID == nil || *auth.SessionID != record.SessionID {
		t.Fatalf("session auth id = %#v, want %s", auth.SessionID, record.SessionID)
	}
	if _, err := ownerOnlyAuth(paths, "max.reviewer"); err == nil || !strings.Contains(err.Error(), "owner-only command is not allowed from a managed session") {
		t.Fatalf("ownerOnlyAuth should still reject managed-session env with runtime marker; got %v", err)
	}
}

func TestManagedSessionCLIRejectsSymlinkCapabilityBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	capabilityFile := session["capability_file"].(string)
	target := filepath.Join(home, "symlink-target.cap")
	if err := os.WriteFile(target, []byte("cap_fakeSessionTokenThatShouldNotBeRead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(capabilityFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, capabilityFile); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file must not be a symlink") {
		t.Fatalf("symlink capability err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsFIFOCapabilityWithoutBlocking(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	capabilityFile := session["capability_file"].(string)
	if err := os.Remove(capabilityFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(capabilityFile, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file must be a regular file") {
		t.Fatalf("fifo session capability err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsParentSymlinkCapabilityBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	fakeHome, err := os.MkdirTemp("/tmp", "comment-cli-fake-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fakeHome) })
	if err := os.MkdirAll(filepath.Join(fakeHome, "bus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "bus", "capabilities"), filepath.Join(fakeHome, "bus", "capabilities")); err != nil {
		t.Fatal(err)
	}
	fakePaths, err := commentbus.ResolvePaths(fakeHome)
	if err != nil {
		t.Fatal(err)
	}
	capabilityFile := filepath.Join(fakePaths.Capabilities, session["profile"].(string), session["session_id"].(string), session["generation"].(string)+".cap")
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", fakeHome,
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file parent directory") {
		t.Fatalf("parent symlink session capability err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsHardlinkedCapabilityBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	realCapabilityFile := session["capability_file"].(string)
	fakeHome := privateTempHome(t, "comment-cli-fake-hardlink-home-")
	fakePaths, err := commentbus.ResolvePaths(fakeHome)
	if err != nil {
		t.Fatal(err)
	}
	fakeCapabilityFile := filepath.Join(fakePaths.Capabilities, session["profile"].(string), session["session_id"].(string), session["generation"].(string)+".cap")
	if err := os.MkdirAll(filepath.Dir(fakeCapabilityFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(realCapabilityFile, fakeCapabilityFile); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", fakeCapabilityFile)

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", fakeHome,
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file must not be hard-linked") {
		t.Fatalf("hardlinked session capability err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsUncleanCapabilityEnvBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	profile := session["profile"].(string)
	sessionID := session["session_id"].(string)
	generation := session["generation"].(string)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathSeparator)
	uncleanCapFile := filepath.Join(paths.Capabilities, profile, "link") + sep + ".." + sep + sessionID + sep + generation + ".cap"
	t.Setenv("COMMENT_IO_PROFILE", profile)
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", sessionID)
	t.Setenv("COMMENT_IO_SESSION_GENERATION", generation)
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", uncleanCapFile)

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "managed-session capability file does not match selected home") {
		t.Fatalf("unclean session capability env err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsMalformedCapabilityBeforeSending(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	capabilityFile := session["capability_file"].(string)
	if err := os.WriteFile(capabilityFile, []byte("not-a-capability-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid managed-session capability file") {
		t.Fatalf("malformed session capability err = %v output = %s", err, output)
	}
}

func TestManagedSessionCLIRejectsOversizedCapabilityBeforeSending(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()

	startOut, err := captureRun(t, []string{"sessions", "start", "--home", home, "reviewer"})
	if err != nil {
		t.Fatalf("sessions start failed: %v\n%s", err, startOut)
	}
	session := resultMap(t, decodeTestSocketResponse(t, startOut))["session"].(map[string]any)
	capabilityFile := session["capability_file"].(string)
	if err := os.WriteFile(capabilityFile, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", session["profile"].(string))
	t.Setenv("COMMENT_IO_BOT_NAME", session["bot_name"].(string))
	t.Setenv("COMMENT_IO_SESSION_ID", session["session_id"].(string))
	t.Setenv("COMMENT_IO_SESSION_GENERATION", session["generation"].(string))
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", capabilityFile)

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "could not read session capability file") || !strings.Contains(err.Error(), "capability file too large") {
		t.Fatalf("oversized session capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsSymlinkCapabilityBeforeReading(t *testing.T) {
	home := privateTempHome(t, "comment-cli-owner-symlink-")
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "owner-target.cap")
	if err := os.WriteFile(target, []byte("cap_fakeOwnerTokenThatShouldNotBeRead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.OwnerCapability); err != nil {
		t.Fatal(err)
	}

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "owner capability file must not be a symlink") {
		t.Fatalf("symlink owner capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsFIFOCapabilityWithoutBlocking(t *testing.T) {
	home := privateTempHome(t, "comment-cli-owner-fifo-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(paths.OwnerCapability, 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRunWithTimeout(t, []string{
		"messages", "wait",
		"--home", home,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	}, 500*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "owner capability file must be a regular file") {
		t.Fatalf("fifo owner capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsParentSymlinkCapabilityBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	fakeHome, err := os.MkdirTemp("/tmp", "comment-cli-fake-owner-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fakeHome) })
	if err := os.MkdirAll(filepath.Join(fakeHome, "bus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "bus", "capabilities"), filepath.Join(fakeHome, "bus", "capabilities")); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", fakeHome,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "owner capability file parent directory") {
		t.Fatalf("parent symlink owner capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsHardlinkedCapabilityBeforeReading(t *testing.T) {
	home, _, stop := startCLITestDaemon(t)
	defer stop()
	realPaths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	fakeHome := privateTempHome(t, "comment-cli-fake-owner-hardlink-home-")
	fakePaths, err := commentbus.ResolvePaths(fakeHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fakePaths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(realPaths.OwnerCapability, fakePaths.OwnerCapability); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", fakeHome,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "owner capability file must not be hard-linked") {
		t.Fatalf("hardlinked owner capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsMalformedCapabilityBeforeSending(t *testing.T) {
	home := privateTempHome(t, "comment-cli-owner-malformed-")
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("not-a-capability-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid owner capability file") {
		t.Fatalf("malformed owner capability err = %v output = %s", err, output)
	}
}

func TestOwnerCLIRejectsOversizedCapabilityBeforeSending(t *testing.T) {
	home := privateTempHome(t, "comment-cli-owner-oversized-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"messages", "wait",
		"--home", home,
		"--profile", "max.reviewer",
		"--timeout-ms", "0",
	})
	if err == nil || !strings.Contains(err.Error(), "owner capability unavailable") || !strings.Contains(err.Error(), "capability file too large") {
		t.Fatalf("oversized owner capability err = %v output = %s", err, output)
	}
}

func TestMessagesSendBodyFileRejectsOversizedInput(t *testing.T) {
	home := t.TempDir()
	bodyPath := filepath.Join(home, "body.md")
	body := strings.Repeat("x", maxCLIMessageBodyBytes+1)
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := captureRun(t, []string{
		"messages", "send",
		"--home", home,
		"--bot", "reviewer",
		"--to", "reviewer",
		"--body-file", bodyPath,
	}); err == nil || !strings.Contains(err.Error(), "message body is too large") {
		t.Fatalf("oversized body-file err = %v output = %s", err, output)
	}
}

func busStateExists(home string) bool {
	_, err := os.Stat(filepath.Join(home, "bus"))
	return err == nil
}

func startCLITestDaemon(t *testing.T) (string, string, func()) {
	t.Helper()
	return startCLITestDaemonWithNotificationClient(t, nil)
}

func startCLITestDaemonWithNotificationClient(t *testing.T, client commentbus.NotificationClient) (string, string, func()) {
	t.Helper()
	home := privateTempHome(t, "comment-cli-home-")
	botletsHome := privateTempHome(t, "comment-cli-bots-")
	tmuxPath := prependFakeTmux(t)
	commentPath := writeFakeCommentBinary(t)
	agentsDir := filepath.Join(home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(agentsDir, "max.reviewer.json")
	if err := os.WriteFile(profilePath, []byte(`{"agent_secret":"as_testsecret","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runnerProfilePath := filepath.Join(agentsDir, "max.runner.json")
	if err := os.WriteFile(runnerProfilePath, []byte(`{"agent_secret":"as_runnersecret","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := `{"bots":[{"name":"reviewer","handle":"max.reviewer","credential_profile":` + strconvQuote(profilePath) + `,"managed_session":{"enabled":true,"runtime":"claude","host":"tmux"}}]}`
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureRun(t, []string{"bus", "init", "--home", home}); err != nil {
		t.Fatal(err)
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	daemon, err := commentbus.StartDaemon(ctx, commentbus.DaemonOptions{
		Paths:              paths,
		Version:            "test",
		BotletsHome:        botletsHome,
		NotificationClient: client,
		Tmux:               commentbus.ExecTmuxController{Binary: tmuxPath},
		Bmux:               newCLITestBmuxController(paths, filepath.Join(filepath.Dir(tmuxPath), "tmux.log")),
		CommentExecutable:  func() (string, error) { return commentPath, nil },
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return home, botletsHome, func() {
		cancel()
		if err := daemon.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func overrideTransientRuntimeAttachForTest(t *testing.T, fn func(commentbus.TransientRuntimeRecord) error) {
	t.Helper()
	oldAttach := runTransientRuntimeAttach
	runTransientRuntimeAttach = func(_ commentbus.Paths, record commentbus.TransientRuntimeRecord) error {
		return fn(record)
	}
	t.Cleanup(func() { runTransientRuntimeAttach = oldAttach })
}

func overrideTransientRuntimeExitedForTest(t *testing.T, fn func(commentbus.TransientRuntimeRecord) bool) {
	t.Helper()
	oldExited := runTransientRuntimeExited
	runTransientRuntimeExited = func(_ commentbus.Paths, record commentbus.TransientRuntimeRecord) bool {
		return fn(record)
	}
	t.Cleanup(func() { runTransientRuntimeExited = oldExited })
}

type cliTestBmuxController struct {
	mu           sync.Mutex
	paths        commentbus.Paths
	logPath      string
	sessions     map[string]string
	paneCommands map[string]string
}

func newCLITestBmuxController(paths commentbus.Paths, logPath string) *cliTestBmuxController {
	return &cliTestBmuxController{
		paths:        paths,
		logPath:      logPath,
		sessions:     map[string]string{},
		paneCommands: map[string]string{},
	}
}

func (c *cliTestBmuxController) appendLog(parts ...string) {
	if c.logPath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.logPath), 0o700)
	if f, err := os.OpenFile(c.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.WriteString(strings.Join(parts, " ") + "\n")
		_ = f.Close()
	}
}

func (c *cliTestBmuxController) HasSession(_ context.Context, sessionName string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.sessions[sessionName]
	return ok, nil
}

func (c *cliTestBmuxController) NewSession(_ context.Context, options commentbus.TmuxNewSessionOptions) error {
	target, err := commentbus.BmuxSocketPathForSession(c.paths, options.SessionName)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sessions[options.SessionName] = target
	c.paneCommands[target] = "claude"
	c.mu.Unlock()
	c.appendLog("new-session", "-s", options.SessionName, "-c", options.WorkingDir, "-e", "COMMENT_IO_HOME="+options.CommentHome, options.Command)
	if options.OutputPipeCommand != "" {
		c.appendLog("pipe-pane", options.OutputPipeCommand)
	}
	return nil
}

func (c *cliTestBmuxController) PaneTarget(_ context.Context, sessionName string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	target, ok := c.sessions[sessionName]
	if !ok {
		return "", commentbus.ErrTmuxSessionMissing
	}
	return target, nil
}

func (c *cliTestBmuxController) PaneBelongsToSession(_ context.Context, sessionName string, paneTarget string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[sessionName] == paneTarget, nil
}

func (c *cliTestBmuxController) PaneCurrentCommand(_ context.Context, paneTarget string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.paneCommands[paneTarget]; !ok {
		return "", commentbus.ErrTmuxSessionMissing
	}
	return c.paneCommands[paneTarget], nil
}

func (c *cliTestBmuxController) CapturePane(context.Context, string, int) (string, error) {
	return "", commentbus.ErrTmuxSessionMissing
}

func (c *cliTestBmuxController) SendLiteral(_ context.Context, paneTarget string, text string) error {
	c.appendLog("send-keys", "-t", paneTarget, text)
	return nil
}

func (c *cliTestBmuxController) PasteText(_ context.Context, paneTarget string, text string) error {
	c.appendLog("send-keys", "-t", paneTarget, text)
	return nil
}

func (c *cliTestBmuxController) SendEnter(_ context.Context, paneTarget string) error {
	c.appendLog("send-keys", "-t", paneTarget, "Enter")
	return nil
}

func (c *cliTestBmuxController) KillSession(_ context.Context, sessionName string) error {
	c.mu.Lock()
	target := c.sessions[sessionName]
	delete(c.sessions, sessionName)
	delete(c.paneCommands, target)
	c.mu.Unlock()
	c.appendLog("kill-session", "-t", sessionName)
	return nil
}

func writeCLILocalSyncBrainProjectionForTest(t *testing.T, paths commentbus.Paths, relativePath string) string {
	t.Helper()
	root := filepath.Join(paths.Home, "Comment Docs")
	brainRoot := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(brainRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for current := brainRoot; ; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if current == root {
			break
		}
	}
	syncDir := filepath.Join(paths.Home, "sync")
	if err := os.MkdirAll(syncDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"version":1,"root":` + strconvQuote(root) + `,"base_url":"https://comt.dev","scope":"library-sync:read:botlets-brains","scope_label":"Botlets brains","config_generation":1}`
	if err := os.WriteFile(filepath.Join(syncDir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(syncDir, "library.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE placements (
		visible_id TEXT PRIMARY KEY,
		slug TEXT NOT NULL,
		section TEXT NOT NULL,
		path TEXT NOT NULL,
		canonical_path TEXT NOT NULL,
		content_hash TEXT NOT NULL,
		body_content_hash TEXT NOT NULL DEFAULT '',
		rendered_projection_hash TEXT NOT NULL DEFAULT '',
		projection_format_version INTEGER NOT NULL DEFAULT 0,
		frontmatter_flavor TEXT NOT NULL DEFAULT '',
		links_flavor TEXT NOT NULL DEFAULT '',
		etag TEXT NOT NULL DEFAULT '',
		revision INTEGER NOT NULL,
		last_seen_snapshot TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		botlets_owner_handle TEXT NOT NULL DEFAULT '',
		botlets_bot_slug TEXT NOT NULL DEFAULT '',
		botlets_bot_local_name TEXT NOT NULL DEFAULT '',
		botlets_bot_id TEXT NOT NULL DEFAULT '',
		botlets_bot_handle TEXT NOT NULL DEFAULT '',
		botlets_bot_agent_id TEXT NOT NULL DEFAULT '',
		botlets_brain_container_id TEXT NOT NULL DEFAULT '',
		botlets_brain_root_folder_id TEXT NOT NULL DEFAULT '',
		botlets_brain_node_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO placements (
		visible_id,
		slug,
		section,
		path,
		canonical_path,
		content_hash,
		revision,
		last_seen_snapshot,
		updated_at,
		botlets_owner_handle,
		botlets_bot_slug,
		botlets_bot_local_name,
		botlets_bot_id,
		botlets_bot_handle,
		botlets_bot_agent_id,
		botlets_brain_container_id,
		botlets_brain_root_folder_id
	) VALUES (?, 'brain-doc-1', 'botlets-brains', ?, ?, 'hash', 1, 'snapshot-1', '2026-05-22T00:00:00Z', 'max', 'reviewer', 'reviewer', 'ag_bot', 'max.reviewer', 'ag_bot', 'lc_brain', 'lf_brain')`,
		"brain-doc-1",
		filepath.Join(brainRoot, "IDENTITY.md"),
		filepath.Join(brainRoot, "IDENTITY.md"),
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatal(err)
	}
	return brainRoot
}

func prependFakeTmux(t *testing.T) string {
	t.Helper()
	dir := privateTempHome(t, "comment-cli-runtime-")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "tmux.log")
	script := `#!/bin/sh
set -eu
state_dir=` + strconvQuote(stateDir) + `
log_path=` + strconvQuote(logPath) + `
cmd="$1"
if [ "${cmd:-}" = "-L" ]; then
  shift 2
  cmd="$1"
fi
shift
printf '%s' "$cmd" >> "$log_path"
for arg in "$@"; do
  printf ' %s' "$arg" >> "$log_path"
done
printf '\n' >> "$log_path"
session=""
case "$cmd" in
  has-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    [ -f "$state_dir/$session" ]
    ;;
  new-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -s) session="$2"; shift 2 ;;
        --) break ;;
        *) shift ;;
      esac
    done
    [ -n "$session" ]
    printf '%s\n' "$session:0.0" > "$state_dir/$session"
    ;;
  display-message)
    format=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        -p) format="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    case "$format" in
      '#{pane_current_command}')
        printf '%s\n' claude
        ;;
      '#{session_name}')
        printf '%s\n' "${session%%:*}"
        ;;
      *)
        cat "$state_dir/${session%%:*}"
        ;;
    esac
    ;;
  send-keys)
    exit 0
    ;;
  kill-session)
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -t) session="$2"; shift 2 ;;
        *) shift ;;
      esac
    done
    rm -f "$state_dir/$session"
    ;;
  *)
    exit 64
    ;;
esac
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(dir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return path
}

func writeFakeCommentBinary(t *testing.T) string {
	t.Helper()
	dir := privateTempHome(t, "comment-cli-binary-")
	path := filepath.Join(dir, "comment")
	writeExecutableScript(t, path, "#!/bin/sh\nexit 0\n")
	return path
}

func writeExecutableScript(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeTestAgentProfile(t *testing.T, home string, handle string, content string) string {
	t.Helper()
	agentsDir := filepath.Join(home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(agentsDir, handle+".json")
	if err := os.WriteFile(profilePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return profilePath
}

func writeUpgradePackageJSON(t *testing.T, packageRoot string, version string) {
	t.Helper()
	if err := os.MkdirAll(packageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"name":"@comment-io/cli","version":` + strconvQuote(version) + `}` + "\n"
	if err := os.WriteFile(filepath.Join(packageRoot, "package.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

type uninstallTestTmux struct {
	err    error
	killed []string
}

func (t *uninstallTestTmux) KillSession(_ context.Context, sessionName string) error {
	t.killed = append(t.killed, sessionName)
	return t.err
}

type fakeLaunchctl struct {
	loaded               bool
	unsupportedGUIDomain bool
	calls                [][]string
	bootstrapHook        func(attempt int) ([]byte, error)
	bootstrapAttempts    int
}

func installFakeLaunchctl(t *testing.T) *fakeLaunchctl {
	t.Helper()
	fake := &fakeLaunchctl{}
	oldSupported := launchdSupported
	oldLaunchctl := launchctlCombinedOutput
	launchdSupported = func() bool { return true }
	launchctlCombinedOutput = fake.run
	t.Cleanup(func() {
		launchdSupported = oldSupported
		launchctlCombinedOutput = oldLaunchctl
	})
	return fake
}

func (f *fakeLaunchctl) run(ctx context.Context, args ...string) ([]byte, error) {
	copied := append([]string{}, args...)
	f.calls = append(f.calls, copied)
	if len(args) == 0 {
		return nil, errors.New("missing launchctl command")
	}
	switch args[0] {
	case "print":
		if f.unsupportedGUIDomain && len(args) > 1 && strings.HasPrefix(args[1], "gui/") {
			return []byte("Could not print domain: 125: Domain does not support specified action\n"), errors.New("exit status 125")
		}
		if f.loaded {
			return []byte("state = running\n"), nil
		}
		return []byte("Could not find service\n"), errors.New("exit status 113")
	case "bootstrap":
		f.bootstrapAttempts++
		if f.bootstrapHook != nil {
			out, err := f.bootstrapHook(f.bootstrapAttempts)
			if err == nil {
				f.loaded = true
			}
			return out, err
		}
		f.loaded = true
		return nil, nil
	case "kickstart":
		if !f.loaded {
			return []byte("Could not find service\n"), errors.New("exit status 113")
		}
		return nil, nil
	case "bootout":
		if !f.loaded {
			return []byte("Could not find service\n"), errors.New("exit status 113")
		}
		f.loaded = false
		return nil, nil
	default:
		return []byte("unsupported fake launchctl command\n"), errors.New("exit status 64")
	}
}

func (f *fakeLaunchctl) requireCall(t *testing.T, want ...string) {
	t.Helper()
	for _, call := range f.calls {
		if strings.Join(call, "\x00") == strings.Join(want, "\x00") {
			return
		}
	}
	t.Fatalf("launchctl call %v not found in %v", want, f.calls)
}

type fakeSystemctl struct {
	active  bool
	enabled bool
	calls   [][]string
}

func installFakeSystemctl(t *testing.T) *fakeSystemctl {
	t.Helper()
	fake := &fakeSystemctl{}
	oldLaunchdSupported := launchdSupported
	oldSystemdSupported := systemdSupported
	oldSystemctl := systemctlCombinedOutput
	launchdSupported = func() bool { return false }
	systemdSupported = func() bool { return true }
	systemctlCombinedOutput = fake.run
	t.Cleanup(func() {
		launchdSupported = oldLaunchdSupported
		systemdSupported = oldSystemdSupported
		systemctlCombinedOutput = oldSystemctl
	})
	return fake
}

func (f *fakeSystemctl) run(ctx context.Context, args ...string) ([]byte, error) {
	copied := append([]string{}, args...)
	f.calls = append(f.calls, copied)
	if len(args) < 2 || args[0] != "--user" {
		return []byte("missing --user\n"), errors.New("exit status 64")
	}
	switch args[1] {
	case "daemon-reload":
		return nil, nil
	case "enable":
		if len(args) < 4 || args[2] != "--now" {
			return []byte("unsupported enable invocation\n"), errors.New("exit status 64")
		}
		f.enabled = true
		f.active = true
		return nil, nil
	case "disable":
		f.enabled = false
		f.active = false
		return nil, nil
	case "start":
		f.active = true
		return nil, nil
	case "restart":
		f.active = true
		return nil, nil
	case "stop":
		f.active = false
		return nil, nil
	case "is-active":
		if f.active {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errors.New("exit status 3")
	default:
		return []byte("unsupported fake systemctl command\n"), errors.New("exit status 64")
	}
}

func (f *fakeSystemctl) requireCall(t *testing.T, want ...string) {
	t.Helper()
	for _, call := range f.calls {
		if strings.Join(call, "\x00") == strings.Join(want, "\x00") {
			return
		}
	}
	t.Fatalf("systemctl call %v not found in %v", want, f.calls)
}

func privateTempHome(t *testing.T, pattern string) string {
	t.Helper()
	base := ""
	switch runtime.GOOS {
	case "darwin":
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		base = filepath.Join(userHome, ".comment-cli-test-tmp")
		if err := os.MkdirAll(base, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(base, 0o700); err != nil {
			t.Fatal(err)
		}
	case "linux":
		base = "/tmp"
	}
	home, err := os.MkdirTemp(base, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return home
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func installManagedSessionEnv(t *testing.T, paths commentbus.Paths, profile string, botName string) commentbus.SessionRecord {
	t.Helper()
	record, err := commentbus.RegisterSession(commentbus.RegisterSessionOptions{
		Paths:       paths,
		Profile:     profile,
		BotName:     botName,
		ScopeType:   "profile",
		ScopeID:     profile,
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-" + botName + "-offline",
		PaneTarget:  "%1",
		Runtime:     "claude",
		State:       "alive",
		Now:         time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_PROFILE", record.Profile)
	t.Setenv("COMMENT_IO_BOT_NAME", record.BotName)
	t.Setenv("COMMENT_IO_SESSION_ID", record.SessionID)
	t.Setenv("COMMENT_IO_SESSION_GENERATION", record.Generation)
	t.Setenv("COMMENT_IO_SESSION_CAP_FILE", record.CapabilityFile)
	return record
}

func captureRun(t *testing.T, args []string) (string, error) {
	t.Helper()
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	runErr := run(args)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), runErr
}

func captureRunWithStdin(t *testing.T, args []string, input string) (string, error) {
	t.Helper()
	oldStdin := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = reader
	defer func() {
		os.Stdin = oldStdin
		_ = reader.Close()
	}()
	return captureRun(t, args)
}

func captureRunWithTimeout(t *testing.T, args []string, timeout time.Duration) (string, error) {
	t.Helper()
	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		output, err := captureRun(t, args)
		done <- result{output: output, err: err}
	}()
	select {
	case result := <-done:
		return result.output, result.err
	case <-time.After(timeout):
		t.Fatalf("command timed out: %v", args)
		return "", nil
	}
}

type cliFakeNotificationClient struct {
	mu                 sync.Mutex
	leases             []*commentbus.CloudNotificationLease
	timeouts           []time.Duration
	mutations          []cliNotificationMutationCall
	handlingActivities []cliNotificationHandlingCall
	claimNotifications map[string]string
}

type cliNotificationMutationCall struct {
	Operation string
	ClaimID   string
	OpID      string
}

type cliNotificationHandlingCall struct {
	Action          string
	Outcome         string
	ClaimID         string
	OpID            string
	ClaimGeneration string
	ProgressAt      string
}

func (f *cliFakeNotificationClient) LeaseNotification(ctx context.Context, profile commentbus.AgentProfile, leaseTTL time.Duration, leaseHolder string, idempotencyKey string, kinds ...string) (*commentbus.CloudNotificationLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.timeouts = append(f.timeouts, 0)
	if len(f.leases) == 0 {
		return nil, nil
	}
	lease := f.leases[0]
	f.leases = f.leases[1:]
	if lease == nil {
		return nil, nil
	}
	f.recordClaimNotificationLocked(lease.ClaimID, lease.NotificationID)
	return lease, nil
}

func (f *cliFakeNotificationClient) RenewNotification(ctx context.Context, profile commentbus.AgentProfile, claimID string, leaseTTL time.Duration, idempotencyKey string) (*commentbus.CloudNotificationLease, error) {
	return nil, errors.New("unexpected notification renew")
}

func (f *cliFakeNotificationClient) AckNotification(ctx context.Context, profile commentbus.AgentProfile, claimID string, idempotencyKey string) (*commentbus.CloudNotificationClaimMutation, error) {
	return f.finishNotificationClaim("ack", claimID, idempotencyKey)
}

func (f *cliFakeNotificationClient) ReleaseNotification(ctx context.Context, profile commentbus.AgentProfile, claimID string, idempotencyKey string) (*commentbus.CloudNotificationClaimMutation, error) {
	return f.finishNotificationClaim("release", claimID, idempotencyKey)
}

func (f *cliFakeNotificationClient) PublishNotificationHandlingActivity(ctx context.Context, profile commentbus.AgentProfile, claimID string, request commentbus.CloudNotificationHandlingRequest, idempotencyKey string) (*commentbus.CloudNotificationHandlingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlingActivities = append(f.handlingActivities, cliNotificationHandlingCall{Action: request.Action, Outcome: request.Outcome, ClaimID: claimID, OpID: idempotencyKey, ClaimGeneration: request.ClaimGeneration, ProgressAt: request.ProgressAt})
	return &commentbus.CloudNotificationHandlingResult{OK: true, Activity: map[string]any{"status": request.Action}}, nil
}

func (f *cliFakeNotificationClient) finishNotificationClaim(operation string, claimID string, idempotencyKey string) (*commentbus.CloudNotificationClaimMutation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mutations = append(f.mutations, cliNotificationMutationCall{Operation: operation, ClaimID: claimID, OpID: idempotencyKey})
	notificationID := f.claimNotifications[claimID]
	if notificationID == "" {
		notificationID = "ntf_clinotificationmutation1234567890"
	}
	return &commentbus.CloudNotificationClaimMutation{OK: true, ClaimID: claimID, NotificationID: notificationID}, nil
}

func (f *cliFakeNotificationClient) recordClaimNotificationLocked(claimID string, notificationID string) {
	if f.claimNotifications == nil {
		f.claimNotifications = map[string]string{}
	}
	if notificationID != "" {
		f.claimNotifications[claimID] = notificationID
	}
}

func decodeTestSocketResponse(t *testing.T, output string) commentbus.SocketResponse {
	t.Helper()
	var response commentbus.SocketResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("invalid socket response json %q: %v", output, err)
	}
	if !response.OK {
		t.Fatalf("socket response failed: %+v", response.Error)
	}
	return response
}

func decodeFailedSocketResponse(t *testing.T, output string) commentbus.SocketResponse {
	t.Helper()
	var response commentbus.SocketResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		t.Fatalf("invalid socket response json %q: %v", output, err)
	}
	if response.OK {
		t.Fatalf("expected a failed socket response, got ok: %+v", response)
	}
	return response
}

func decodeTestJSONMap(t *testing.T, output string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatalf("invalid json map %q: %v", output, err)
	}
	return decoded
}

func resultMap(t *testing.T, response commentbus.SocketResponse) map[string]any {
	t.Helper()
	result, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("response result = %#v", response.Result)
	}
	return result
}

func firstMessageID(t *testing.T, response commentbus.SocketResponse) string {
	t.Helper()
	result := resultMap(t, response)
	messages := result["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages count = %d", len(messages))
	}
	message := messages[0].(map[string]any)
	id, ok := message["id"].(string)
	if !ok || id == "" {
		t.Fatalf("message id = %#v", message["id"])
	}
	return id
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TestBotletsBrainHasPersonaDocs covers the detection half of bug #531 defense
// (B): a brain dir with persona markdown is "present"; an empty dir (or one
// holding only dotfiles / subfolders) is not.
func TestBotletsBrainHasPersonaDocs(t *testing.T) {
	empty := t.TempDir()
	if botletsBrainHasPersonaDocs(empty) {
		t.Fatalf("empty brain dir should report no persona docs")
	}

	dotsOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(dotsOnly, ".keep"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dotsOnly, "_Comment.io Docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if botletsBrainHasPersonaDocs(dotsOnly) {
		t.Fatalf("brain dir with only dotfiles/subfolders should report no persona docs")
	}

	withDoc := t.TempDir()
	if err := os.WriteFile(filepath.Join(withDoc, "Identity.md"), []byte("# Identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !botletsBrainHasPersonaDocs(withDoc) {
		t.Fatalf("brain dir with a markdown persona doc should report present")
	}

	// A brain emptied down to only generated scaffolding (README.md) is still
	// effectively empty and must NOT suppress the warning.
	readmeOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(readmeOnly, "README.md"), []byte("# Brain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if botletsBrainHasPersonaDocs(readmeOnly) {
		t.Fatalf("brain dir with only README.md should report no persona docs")
	}
}

// TestWarnIfBotletsBrainEmptyFiresOnEmptyBrain covers bug #531 defense (B): a
// Botlets bot launching from an empty brain projection must emit a loud warning
// rather than silently inheriting the operator's personal CLAUDE.md.
func TestWarnIfBotletsBrainEmptyFiresOnEmptyBrain(t *testing.T) {
	brain := t.TempDir()
	options := runtimeRunOptions{Profile: "owner.bot", BotShortcut: "bot"}

	output := captureWarnStderr(t, func() {
		warnIfBotletsBrainEmpty(options, brain, true)
	})
	if !strings.Contains(output, "WARNING") || !strings.Contains(output, "no persona documents") {
		t.Fatalf("expected loud empty-brain warning, got %q", output)
	}

	// A brain with a persona doc must not warn.
	if err := os.WriteFile(filepath.Join(brain, "Identity.md"), []byte("# Identity\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output = captureWarnStderr(t, func() {
		warnIfBotletsBrainEmpty(options, brain, true)
	})
	if strings.TrimSpace(output) != "" {
		t.Fatalf("expected no warning when brain has persona docs, got %q", output)
	}

	// An explicit --cwd must suppress the warning even with an empty brain.
	explicit := t.TempDir()
	output = captureWarnStderr(t, func() {
		warnIfBotletsBrainEmpty(runtimeRunOptions{Profile: "owner.bot", CWD: explicit}, explicit, true)
	})
	if strings.TrimSpace(output) != "" {
		t.Fatalf("explicit --cwd should suppress the empty-brain warning, got %q", output)
	}
}

func captureWarnStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = writer
	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
