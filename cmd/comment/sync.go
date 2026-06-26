package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

func runSync(args []string) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Println(syncUsage())
		return nil
	}
	command := "once"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "login":
		return runSyncLogin(args)
	case "once":
		return runSyncOnce(args)
	case "status":
		return runSyncStatus(args)
	case "logout":
		return runSyncLogout(args)
	case "add":
		return errors.New("comment sync add is deprecated for library sync; add or share the document in Comment.io, then run comment sync once")
	case "repair":
		return errors.New("comment sync repair is not available for read-only library sync; run comment sync once to reconcile projections")
	case "doctor":
		return runSyncDoctor(args)
	case "conflicts":
		return runSyncConflicts(args)
	case "recover":
		return runSyncRecover(args)
	case "enable", "disable", "start", "stop":
		return runSyncToggle(command, args)
	case "live":
		return runSyncLive(args)
	case "watch":
		return runSyncWatch(args)
	default:
		return fmt.Errorf("unknown sync command %q\n\n%s", command, syncUsage())
	}
}

func runSyncLogin(args []string) error {
	fs := flag.NewFlagSet("comment sync login", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	root := fs.String("root", "", "sync root (default ~/Comment Docs)")
	baseURL := fs.String("base-url", commentsync.DefaultBaseURL(), "Comment.io base URL")
	apiKeyStdin := fs.Bool("api-key-stdin", false, "read scoped library sync API key from stdin")
	apiKeyEnv := fs.Bool("api-key-env", false, "read scoped library sync API key from COMMENT_IO_LIBRARY_SYNC_KEY")
	frontmatterFlavor := fs.String("frontmatter-flavor", "", "sync-root frontmatter flavor: plain (default) or okf; empty preserves the existing root")
	linksFlavor := fs.String("links-flavor", "", "sync-root links flavor: plain (default) or obsidian; empty preserves the existing root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := commentsync.ValidateLoginFlavorFlags(*frontmatterFlavor, *linksFlavor); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("sync login does not accept positional arguments")
	}
	var key string
	switch {
	case *apiKeyStdin:
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
		if err != nil {
			return err
		}
		key = strings.TrimSpace(string(data))
	case *apiKeyEnv:
		key = strings.TrimSpace(os.Getenv("COMMENT_IO_LIBRARY_SYNC_KEY"))
	default:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		// Sync rides on pairing: if this computer is already paired to the same
		// origin, provision the read-scoped key over its daemon token — no second
		// browser approval, because pairing already authorized this machine (the
		// strictly more powerful agent-install capability). Fall back to the
		// browser device flow only when unpaired or paired to a different origin.
		mintedKey, mintErr := syncKeyViaPairing(ctx, *home, *baseURL)
		if mintErr != nil {
			// A definitive, actionable pairing state (sync turned off) — surface it
			// rather than falling through to a browser flow that would mint an
			// unrelated standalone key.
			return mintErr
		}
		if mintedKey != "" {
			key = mintedKey
			fmt.Fprintln(os.Stderr, "This computer is already paired — provisioned local sync over the pairing (no browser approval needed).")
		} else {
			session, err := commentsync.StartDeviceLogin(ctx, commentsync.Options{
				Home:    *home,
				Root:    *root,
				BaseURL: *baseURL,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Open this URL to approve local sync:\n%s\n\nCode: %s\n", session.VerificationURI, session.UserCode)
			key, err = commentsync.PollDeviceLogin(ctx, commentsync.Options{
				Home:    *home,
				Root:    *root,
				BaseURL: *baseURL,
			}, session)
			if err != nil {
				return err
			}
		}
	}
	cfg, err := commentsync.Login(context.Background(), commentsync.Options{
		Home:              *home,
		Root:              *root,
		BaseURL:           *baseURL,
		APIKey:            key,
		ValidateKey:       true,
		FrontmatterFlavor: *frontmatterFlavor,
		LinksFlavor:       *linksFlavor,
	})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":                 true,
		"configured":         true,
		"root":               cfg.Root,
		"base_url":           cfg.BaseURL,
		"scope":              cfg.Scope,
		"scope_label":        cfg.ScopeLabel,
		"frontmatter_flavor": commentsync.ConfigFrontmatterFlavor(cfg),
		"links_flavor":       commentsync.ConfigLinksFlavor(cfg),
		"backgroundSync":     cfg.BackgroundSync,
		"manualOnly":         cfg.ManualOnly,
	})
}

// syncKeyViaPairing mints a read-scoped library-sync key over the daemon pairing
// token when this computer is already paired to the SAME origin as baseURL, so
// `comment sync login` needs no second browser approval (pairing already
// authorized the machine). The caller still runs commentsync.Login with this
// key, so --root / --frontmatter-flavor / --links-flavor and key validation all
// apply as usual — only the key's source changes from the browser device flow to
// the pairing token. Returns ("", false) when unpaired, paired to a different
// origin. Three outcomes:
//   - ("<key>", nil): minted over the pairing — use it.
//   - ("", nil): not applicable (unpaired, different origin, or a transient mint
//     error) — the caller falls back to the browser device flow.
//   - ("", err): a definitive, actionable pairing state — sync is turned off for
//     this computer — so the caller should surface err, NOT mint an unrelated
//     standalone key via the browser flow.
func syncKeyViaPairing(ctx context.Context, home, baseURL string) (string, error) {
	// Resolve the home exactly as commentsync does (--home flag → COMMENT_IO_HOME
	// → default) so the pairing we read belongs to the SAME home commentsync.Login
	// will write the key into. Skipping the COMMENT_IO_HOME step would, in a
	// multi-home setup, read the default home's pairing and could mint a key from
	// the wrong account into the COMMENT_IO_HOME sync config.
	if home == "" {
		home = os.Getenv("COMMENT_IO_HOME")
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		return "", nil
	}
	auth, paired, err := commentbus.LoadDaemonAuth(paths)
	if err != nil || !paired {
		return "", nil
	}
	pairedBase := strings.TrimRight(strings.TrimSpace(auth.BaseURL), "/")
	targetBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if pairedBase == "" || !strings.EqualFold(pairedBase, targetBase) {
		return "", nil
	}
	key, err := fetchDaemonSyncCredential(ctx, pairedBase, auth.Token)
	if errors.Is(err, errDaemonSyncCapabilityDisabled) {
		return "", fmt.Errorf("local sync is turned off for this computer — re-enable it in Settings → Paired computers, then run `comment sync login` again")
	}
	if err != nil || strings.TrimSpace(key) == "" {
		return "", nil
	}
	return key, nil
}

func runSyncOnce(args []string) error {
	fs := flag.NewFlagSet("comment sync once", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("sync once does not accept positional arguments")
	}
	result, err := commentsync.Once(context.Background(), commentsync.Options{Home: *home})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runSyncStatus(args []string) error {
	fs := flag.NewFlagSet("comment sync status", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("sync status does not accept positional arguments")
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: *home})
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(status)
	}
	if !status.Configured {
		fmt.Println("Library sync is not configured.")
		fmt.Println("Run comment sync login. If this computer is already paired to the same Comment.io origin, sync is provisioned over that pairing with no second browser approval; otherwise follow the browser approval link it prints.")
		return nil
	}
	fmt.Printf("root: %s\n", status.Root)
	fmt.Printf("base_url: %s\n", status.BaseURL)
	fmt.Printf("scope: %s\n", status.ScopeLabel)
	fmt.Printf("flavor: frontmatter=%s links=%s\n", status.FrontmatterFlavor, status.LinksFlavor)
	fmt.Printf("backgroundSync: %v\n", status.BackgroundSync)
	fmt.Printf("manualOnly: %v\n", status.ManualOnly)
	fmt.Printf("documents: %d\n", status.Documents)
	fmt.Printf("conflicts: %d\n", status.Conflicts)
	if status.LastSyncAt != "" {
		fmt.Printf("lastSyncAt: %s\n", status.LastSyncAt)
	}
	for _, section := range status.Unsupported {
		fmt.Printf("unsupported: %s\n", section)
	}
	return nil
}

func runSyncLogout(args []string) error {
	fs := flag.NewFlagSet("comment sync logout", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	purgeLocal := fs.Bool("purge-local", false, "remove verified clean local projections and sync placement state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := commentsync.Logout(context.Background(), commentsync.Options{Home: *home, PurgeLocal: *purgeLocal})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":                  true,
		"removed":             result.Removed,
		"serverRevoked":       result.ServerRevoked,
		"purgedLocal":         result.PurgedLocal,
		"projectionsRemoved":  result.ProjectionsRemoved,
		"recoveriesPreserved": result.RecoveriesPreserved,
	})
}

func runSyncDoctor(args []string) error {
	fs := flag.NewFlagSet("comment sync doctor", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	fix := fs.Bool("fix", false, "deprecated; no automatic repair is available")
	if err := fs.Parse(args); err != nil {
		return err
	}
	status, statusErr := commentsync.ReadStatus(commentsync.Options{Home: *home})
	result := map[string]any{
		"ok":                true,
		"fuse_required":     false,
		"legacy_mount":      false,
		"fix_supported":     false,
		"fix_requested":     *fix,
		"fix_applied":       false,
		"fix_message":       "",
		"backgroundSync":    status.BackgroundSync,
		"manualOnly":        status.ManualOnly,
		"daemonIntegration": status.BackgroundSync,
		"configured":        status.Configured,
		"documents":         status.Documents,
		"conflicts":         status.Conflicts,
		"status_error":      "",
	}
	if statusErr != nil {
		result["ok"] = false
		result["status_error"] = statusErr.Error()
	}
	if *fix {
		result["fix_message"] = "No automatic repair is currently available; run comment sync once to reconcile projections."
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Println("ok: library sync does not require FUSE, macFUSE, libfuse, Node, npm, or tsx.")
	fmt.Printf("configured: %v\n", status.Configured)
	fmt.Printf("backgroundSync: %v\n", status.BackgroundSync)
	fmt.Printf("manualOnly: %v\n", status.ManualOnly)
	fmt.Printf("documents: %d\n", status.Documents)
	fmt.Printf("conflicts: %d\n", status.Conflicts)
	if *fix {
		fmt.Println("repair: no automatic repair is currently available; run comment sync once to reconcile projections.")
	}
	if statusErr != nil {
		fmt.Printf("status_error: %s\n", statusErr)
	}
	return nil
}

func runSyncConflicts(args []string) error {
	fs := flag.NewFlagSet("comment sync conflicts", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	items, err := commentsync.ListRecoveries(context.Background(), commentsync.Options{Home: *home})
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(map[string]any{"conflicts": items})
	}
	fmt.Printf("conflicts: %d\n", len(items))
	for _, item := range items {
		fmt.Printf("%s\t%s\t%s\n", item.ID, item.Reason, item.OriginalPath)
	}
	return nil
}

func runSyncRecover(args []string) error {
	fs := flag.NewFlagSet("comment sync recover", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	diff := fs.Bool("diff", false, "print a simple diff against the current projection")
	copyNext := fs.Bool("copy-next-to-original", false, "copy the recovery artifact next to the original projection")
	discard := fs.Bool("discard", false, "discard the recovery artifact")
	open := fs.Bool("open", false, "print the recovery artifact path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("sync recover requires a recovery id or path")
	}
	action := commentsync.RecoverActionShow
	selected := 0
	for _, enabled := range []bool{*diff, *copyNext, *discard, *open} {
		if enabled {
			selected++
		}
	}
	if selected > 1 {
		return errors.New("choose only one recover action")
	}
	switch {
	case *diff:
		action = commentsync.RecoverActionDiff
	case *copyNext:
		action = commentsync.RecoverActionCopyNextToOriginal
	case *discard:
		action = commentsync.RecoverActionDiscard
	case *open:
		action = commentsync.RecoverActionShow
	}
	result, err := commentsync.Recover(context.Background(), commentsync.Options{Home: *home}, fs.Args()[0], action)
	if err != nil {
		return err
	}
	if action == commentsync.RecoverActionDiff {
		fmt.Print(result.Diff)
		return nil
	}
	return printJSON(result)
}

func runSyncToggle(command string, args []string) error {
	fs := flag.NewFlagSet("comment sync "+command, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	live := fs.Bool("live", false, "also enable live WebSocket sync")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("sync %s does not accept positional arguments", command)
	}
	switch command {
	case "enable", "start":
		if err := commentsync.SetBackgroundSync(context.Background(), commentsync.Options{Home: *home}, true); err != nil {
			return err
		}
		if *live {
			return commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: *home}, true)
		}
		return nil
	case "disable", "stop":
		if *live {
			if err := commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: *home}, false); err != nil {
				return err
			}
		}
		return commentsync.SetBackgroundSync(context.Background(), commentsync.Options{Home: *home}, false)
	default:
		return fmt.Errorf("unknown sync toggle %q", command)
	}
}

func runSyncLive(args []string) error {
	command := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("comment sync live "+command, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("sync live %s does not accept positional arguments", command)
	}
	switch command {
	case "enable", "start":
		if err := commentsync.SetBackgroundSync(context.Background(), commentsync.Options{Home: *home}, true); err != nil {
			return err
		}
		return commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: *home}, true)
	case "disable", "stop":
		return commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: *home}, false)
	case "status":
		status, err := commentsync.ReadStatus(commentsync.Options{Home: *home})
		if err != nil {
			return err
		}
		payload := map[string]any{
			"configured":      status.Configured,
			"backgroundSync":  status.BackgroundSync,
			"liveSyncEnabled": status.LiveSyncEnabled,
		}
		if *jsonOut {
			return printJSON(payload)
		}
		if status.LiveSyncEnabled {
			fmt.Println("live sync enabled")
		} else {
			fmt.Println("live sync disabled")
		}
		return nil
	default:
		return fmt.Errorf("unknown sync live command %q", command)
	}
}

func runSyncWatch(args []string) error {
	fs := flag.NewFlagSet("comment sync watch", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	once := fs.Bool("once", false, "run one foreground iteration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("sync watch does not accept positional arguments")
	}
	if !*once {
		fmt.Fprintln(os.Stderr, "foreground watch currently runs one local recovery scan per invocation; persistent polling is owned by the daemon")
	}
	result, err := commentsync.RecoverDirtyProjections(context.Background(), commentsync.Options{Home: *home})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func syncUsage() string {
	return strings.Join([]string{
		"Usage:",
		"  comment sync login [--base-url https://comment.io] [--root ~/Comment\\ Docs]",
		"  comment sync login --api-key-stdin [--base-url https://comment.io] [--root ~/Comment\\ Docs]",
		"  COMMENT_IO_LIBRARY_SYNC_KEY=... comment sync login --api-key-env [--base-url https://comment.io] [--root ~/Comment\\ Docs]",
		"  comment sync once",
		"  comment sync status [--json]",
		"  comment sync doctor [--json]",
		"  comment sync conflicts [--json]",
		"  comment sync recover <path|recovery-id> [--diff|--copy-next-to-original|--discard]",
		"  comment sync enable|start [--live]",
		"  comment sync disable|stop [--live]",
		"  comment sync live enable|disable|status [--json]",
		"  comment sync watch [--once]",
		"  comment sync logout [--purge-local]",
	}, "\n")
}
