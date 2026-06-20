package commentsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var ErrBotletsBrainProjectionNotFound = errors.New("botlets brain projection not found")

type BotletsBrainProjectionQuery struct {
	WorkspaceID  string
	BotID        string
	BotAgentID   string
	ContainerID  string
	RootFolderID string
}

type BotletsBrainProjection struct {
	Root                string    `json:"root"`
	RelativePath        string    `json:"relative_path"`
	WorkspaceID         string    `json:"workspace_id,omitempty"`
	ContainerID         string    `json:"container_id"`
	RootFolderID        string    `json:"root_folder_id"`
	BotID               string    `json:"bot_id,omitempty"`
	BotAgentID          string    `json:"bot_agent_id"`
	BotHandle           string    `json:"bot_handle,omitempty"`
	BotSlug             string    `json:"bot_slug,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitempty"`
	SyncRoot            string    `json:"sync_root"`
	SyncRootFingerprint string    `json:"sync_root_fingerprint"`
}

func FindBotletsBrainProjection(ctx context.Context, opts Options, query BotletsBrainProjectionQuery) (BotletsBrainProjection, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return BotletsBrainProjection{}, err
	}
	cfg, err := readConfig(paths)
	if err != nil {
		return BotletsBrainProjection{}, err
	}
	if err := ensureSafeRoot(cfg.Root); err != nil {
		return BotletsBrainProjection{}, err
	}
	if err := validateRootOwnership(cfg.Root, cfg, paths); err != nil {
		return BotletsBrainProjection{}, err
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return BotletsBrainProjection{}, err
	}
	defer state.Close()
	placements, err := state.listPlacements(ctx)
	if err != nil {
		return BotletsBrainProjection{}, err
	}
	sort.Slice(placements, func(i, j int) bool {
		return placements[i].Path < placements[j].Path
	})
	for _, placement := range placements {
		if placement.Section != "botlets-brains" {
			continue
		}
		if query.BotID != "" && placement.BotletsBotID != "" && placement.BotletsBotID != query.BotID {
			continue
		}
		if query.BotAgentID != "" && placement.BotletsBotAgentID != query.BotAgentID {
			continue
		}
		if query.ContainerID != "" && placement.BotletsBrainContainerID != query.ContainerID {
			continue
		}
		if query.RootFolderID != "" && placement.BotletsBrainRootFolderID != query.RootFolderID {
			continue
		}
		root, rel, ok := botletsBrainRootFromPlacement(cfg.Root, placement.Path)
		if !ok {
			continue
		}
		if !pathWithinRoot(cfg.Root, root) {
			continue
		}
		if err := validateBotletsBrainProjectionPath(cfg.Root, root); err != nil {
			continue
		}
		return BotletsBrainProjection{
			Root:                root,
			RelativePath:        filepath.ToSlash(rel),
			WorkspaceID:         query.WorkspaceID,
			ContainerID:         placement.BotletsBrainContainerID,
			RootFolderID:        placement.BotletsBrainRootFolderID,
			BotID:               placement.BotletsBotID,
			BotAgentID:          placement.BotletsBotAgentID,
			BotHandle:           placement.BotletsBotHandle,
			BotSlug:             placement.BotletsBotSlug,
			UpdatedAt:           placement.UpdatedAt,
			SyncRoot:            cfg.Root,
			SyncRootFingerprint: SyncRootFingerprint(cfg.Root),
		}, nil
	}
	return BotletsBrainProjection{}, ErrBotletsBrainProjectionNotFound
}

func SyncRootFingerprint(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	return "sha256:" + sha256Hex(filepath.Clean(absolute))
}

func botletsBrainRootFromPlacement(root string, placementPath string) (string, string, bool) {
	if root == "" || placementPath == "" {
		return "", "", false
	}
	rel, err := filepath.Rel(root, placementPath)
	if err != nil {
		return "", "", false
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", "", false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || parts[0] != "Botlets" || parts[3] != "brain" {
		return "", "", false
	}
	brainRel := filepath.Join(parts[:4]...)
	return filepath.Join(root, brainRel), brainRel, true
}

func validateBotletsBrainProjectionPath(syncRoot string, brainRoot string) error {
	rel, err := filepath.Rel(syncRoot, brainRoot)
	if err != nil {
		return err
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("botlets brain projection is outside sync root: %s", brainRoot)
	}
	current := filepath.Clean(syncRoot)
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("botlets brain projection refuses symlink: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("botlets brain projection path is not a directory: %s", current)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("botlets brain projection path is group/world writable: %s", current)
		}
		if !fileOwnedByCurrentUser(info) {
			return fmt.Errorf("botlets brain projection path is owned by another user: %s", current)
		}
	}
	return nil
}
