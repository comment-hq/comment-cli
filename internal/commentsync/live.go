package commentsync

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const (
	liveEventsEndpoint       = "/auth/library-sync/events"
	liveEventVersion         = "1"
	liveDebounceWindow       = 2 * time.Second
	livePingInterval         = 30 * time.Second
	liveWriteTimeout         = 30 * time.Second
	livePongTimeout          = 90 * time.Second
	liveMaxTargetedRefreshes = 60
	liveTargetedRateWindow   = time.Minute
	liveMaxTargetedPerWindow = 120
	liveMaxFrameBytes        = 64 * 1024
	websocketAcceptGUID      = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	liveProjectionSnapshotID = "live"
	liveStatusFileName       = "live-status.json"
)

type LiveEvent struct {
	Type        string `json:"type"`
	EventID     string `json:"event_id"`
	Cursor      string `json:"cursor"`
	Slug        string `json:"slug,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type LiveSyncResult struct {
	EventsProcessed     int `json:"eventsProcessed"`
	ProjectionRefreshes int `json:"projectionRefreshes"`
	SnapshotRefreshes   int `json:"snapshotRefreshes"`
	NotModified         int `json:"notModified"`
}

type TargetedProjectionResult struct {
	Refreshed     int  `json:"refreshed"`
	NotModified   int  `json:"notModified"`
	NeedsSnapshot bool `json:"needsSnapshot"`
}

type liveProjectionWork struct {
	event LiveEvent
}

type liveConnectionStatus struct {
	State     string `json:"state"`
	Root      string `json:"root"`
	BaseURL   string `json:"base_url"`
	KeyID     string `json:"key_id,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func RunLiveSync(ctx context.Context, opts Options) (LiveSyncResult, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return LiveSyncResult{}, err
	}
	cfg, err := readConfig(paths)
	if err != nil {
		return LiveSyncResult{}, err
	}
	creds, err := readCredentials(paths)
	if err != nil {
		return LiveSyncResult{}, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	cursor, _ := readLiveCursor(paths, cfg)
	wsURL, err := buildLiveEventsURL(cfg.BaseURL, cursor)
	if err != nil {
		return LiveSyncResult{}, err
	}
	conn, reader, err := dialLiveWebSocket(ctx, client, wsURL, creds.APIKey)
	if err != nil {
		return LiveSyncResult{}, err
	}
	defer conn.Close()
	defer func() {
		_ = writeLiveConnectionStatus(paths, cfg, "disconnected")
	}()
	ackedCursor := cursor
	var writeMu sync.Mutex
	writeFrame := func(opcode byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(liveWriteTimeout))
		defer conn.SetWriteDeadline(time.Time{})
		return writeWebSocketClientFrame(conn, opcode, payload)
	}
	writeCursor := func(nextCursor string) error {
		if nextCursor == "" {
			return nil
		}
		if err := writeLiveCursor(paths, cfg, nextCursor); err != nil {
			return err
		}
		ackedCursor = maxLiveCursor(ackedCursor, nextCursor)
		return nil
	}
	if err := writeFrame(1, livePingPayload(ackedCursor)); err != nil {
		return LiveSyncResult{}, err
	}

	events := make(chan LiveEvent)
	readErr := make(chan error, 1)
	go func() {
		defer close(events)
		for {
			opcode, payload, err := readWebSocketServerFrame(reader)
			if err != nil {
				readErr <- err
				return
			}
			switch opcode {
			case 1:
				var event LiveEvent
				if err := json.Unmarshal(payload, &event); err == nil && event.Type != "" {
					events <- event
				}
			case 8:
				readErr <- io.EOF
				return
			case 9:
				_ = writeFrame(10, payload)
			}
		}
	}()

	var result LiveSyncResult
	pending := map[string]liveProjectionWork{}
	fullSnapshotDue := false
	var fullSnapshotCursor string
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	pingTicker := time.NewTicker(livePingInterval)
	defer pingTicker.Stop()
	lastPong := time.Now()
	var deferredHeartbeatCursor string
	targetedWindowStartedAt := time.Now()
	targetedRefreshesInWindow := 0
	reserveTargetedRefreshes := func(count int) bool {
		if count <= 0 {
			return true
		}
		now := time.Now()
		if now.Sub(targetedWindowStartedAt) >= liveTargetedRateWindow {
			targetedWindowStartedAt = now
			targetedRefreshesInWindow = 0
		}
		if targetedRefreshesInWindow+count > liveMaxTargetedPerWindow {
			return false
		}
		targetedRefreshesInWindow += count
		return true
	}
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(liveDebounceWindow)
	}
	flush := func() error {
		if fullSnapshotDue {
			once, err := Once(ctx, opts)
			if err != nil {
				return err
			}
			_ = once
			result.SnapshotRefreshes++
			fullSnapshotDue = false
			cursorToWrite := maxLiveCursor(fullSnapshotCursor, deferredHeartbeatCursor)
			for _, work := range pending {
				cursorToWrite = maxLiveCursor(cursorToWrite, work.event.Cursor)
			}
			pending = map[string]liveProjectionWork{}
			deferredHeartbeatCursor = ""
			if cursorToWrite != "" {
				if err := writeCursor(cursorToWrite); err != nil {
					return err
				}
			}
			return nil
		}
		slugs := make([]string, 0, len(pending))
		for slug := range pending {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		if len(slugs) > liveMaxTargetedRefreshes || !reserveTargetedRefreshes(len(slugs)) {
			_, err := Once(ctx, opts)
			if err != nil {
				return err
			}
			result.SnapshotRefreshes++
			cursorToWrite := deferredHeartbeatCursor
			for _, slug := range slugs {
				cursorToWrite = maxLiveCursor(cursorToWrite, pending[slug].event.Cursor)
			}
			pending = map[string]liveProjectionWork{}
			deferredHeartbeatCursor = ""
			if cursorToWrite != "" {
				if err := writeCursor(cursorToWrite); err != nil {
					return err
				}
			}
			return nil
		}
		cursorToWrite := deferredHeartbeatCursor
		for _, slug := range slugs {
			work := pending[slug]
			refresh, err := RefreshProjection(ctx, opts, work.event)
			if err != nil {
				return err
			}
			if refresh.NeedsSnapshot {
				_, err := Once(ctx, opts)
				if err != nil {
					return err
				}
				result.SnapshotRefreshes++
			}
			result.ProjectionRefreshes += refresh.Refreshed
			result.NotModified += refresh.NotModified
			if work.event.Cursor != "" {
				cursorToWrite = maxLiveCursor(cursorToWrite, work.event.Cursor)
			}
			delete(pending, slug)
		}
		if cursorToWrite != "" {
			if err := writeCursor(cursorToWrite); err != nil {
				return err
			}
		}
		deferredHeartbeatCursor = ""
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-pingTicker.C:
			if time.Since(lastPong) > livePongTimeout {
				return result, errors.New("live sync websocket pong timeout")
			}
			if err := writeFrame(1, livePingPayload(ackedCursor)); err != nil {
				return result, err
			}
		case err := <-readErr:
			if len(pending) > 0 || fullSnapshotDue {
				if flushErr := flush(); flushErr != nil {
					return result, flushErr
				}
			}
			return result, err
		case event, ok := <-events:
			if !ok {
				if len(pending) > 0 || fullSnapshotDue {
					if flushErr := flush(); flushErr != nil {
						return result, flushErr
					}
				}
				return result, io.EOF
			}
			switch event.Type {
			case "pong":
				lastPong = time.Now()
				_ = writeLiveConnectionStatus(paths, cfg, "connected")
				continue
			case "heartbeat":
				if event.Cursor != "" {
					if len(pending) > 0 || fullSnapshotDue {
						deferredHeartbeatCursor = maxLiveCursor(deferredHeartbeatCursor, event.Cursor)
					} else {
						if err := writeCursor(event.Cursor); err != nil {
							return result, err
						}
					}
				}
			case "reset_required", "snapshot_invalidated", "document_removed":
				writeLiveEventReceivedLog(paths, event)
				fullSnapshotDue = true
				fullSnapshotCursor = maxLiveCursor(fullSnapshotCursor, event.Cursor)
				result.EventsProcessed++
				resetTimer()
			case "document_projection_changed":
				if event.Slug != "" {
					writeLiveEventReceivedLog(paths, event)
					current, ok := pending[event.Slug]
					if !ok || event.Revision >= current.event.Revision {
						pending[event.Slug] = liveProjectionWork{event: event}
					}
					result.EventsProcessed++
					resetTimer()
				}
			}
		case <-timer.C:
			if err := flush(); err != nil {
				return result, err
			}
		}
	}
}

func livePingPayload(cursor string) []byte {
	payload := struct {
		Type   string `json:"type"`
		Cursor string `json:"cursor,omitempty"`
	}{Type: "ping", Cursor: cursor}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"ping"}`)
	}
	return data
}

func writeLiveConnectionStatus(paths commentbus.Paths, cfg Config, state string) error {
	if state != "connected" && state != "disconnected" {
		return nil
	}
	status := liveConnectionStatus{
		State:     state,
		Root:      cfg.Root,
		BaseURL:   cfg.BaseURL,
		KeyID:     cfg.KeyID,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	dir := filepath.Join(paths.Home, "sync")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, liveStatusFileName), append(data, '\n'), 0o600)
}

func writeLiveEventReceivedLog(paths commentbus.Paths, event LiveEvent) {
	data := map[string]any{
		"type":   event.Type,
		"cursor": event.Cursor,
	}
	if event.Slug != "" {
		data["slug"] = event.Slug
	}
	if event.Reason != "" {
		data["reason"] = event.Reason
	}
	writeSyncRuntimeLog(paths, "info", "sync.live.event_received", data)
}

func writeSyncRuntimeLog(paths commentbus.Paths, level string, msg string, data map[string]any) {
	if err := os.MkdirAll(paths.Logs, 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(paths.Logs, "commentd.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"level":     level,
		"component": "sync.library",
		"msg":       msg,
		"data":      data,
	})
}

func maxLiveCursor(a, b string) string {
	if b == "" {
		return a
	}
	if a == "" || b > a {
		return b
	}
	return a
}

func placementNeedsLocalRepair(root string, placement placementMeta, flavor syncFlavor) (bool, error) {
	target := placement.Path
	if target == "" {
		target = placement.CanonicalPath
	}
	if target == "" {
		return true, nil
	}
	// A flavor switch on an existing root leaves the on-disk file in the old
	// presentation; it must be re-rendered even though it is "clean" against its
	// old-flavor stored hash. The dirty-check below can't see this (the stored
	// body hash matches the old-flavor body), so trigger a rewrite explicitly.
	if placementFlavor(placement) != flavor {
		return true, nil
	}
	if err := validateManagedPath(root, target); err != nil {
		return false, err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	if placement.RenderedProjectionHash != "" && sha256Hex(string(data)) != placement.RenderedProjectionHash {
		return true, nil
	}
	if info, err := os.Stat(target); err == nil && info.Mode().Perm() != 0o444 {
		return true, nil
	} else if err != nil {
		return false, err
	}
	expected := placementBodyContentHash(placement)
	return expected == "" || !localProjectionMatchesHash(data, expected), nil
}

func RefreshProjection(ctx context.Context, opts Options, event LiveEvent) (TargetedProjectionResult, error) {
	if event.Slug == "" {
		return TargetedProjectionResult{NeedsSnapshot: true}, nil
	}
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	defer lock.Close()
	cfg, err := readConfig(paths)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	creds, err := readCredentials(paths)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	defer state.Close()
	if err := ensureSafeRoot(cfg.Root); err != nil {
		return TargetedProjectionResult{}, err
	}
	if err := validateRootOwnership(cfg.Root, cfg, paths); err != nil {
		return TargetedProjectionResult{}, err
	}
	if err := state.replayIncompleteOps(ctx, cfg.Root); err != nil {
		return TargetedProjectionResult{}, err
	}
	placements, err := state.listPlacements(ctx)
	if err != nil {
		return TargetedProjectionResult{}, err
	}
	flavor := configFlavor(cfg)
	// Wikilinks (links=obsidian) resolve against every placement's on-disk
	// basename across the whole vault, not just the doc being refreshed.
	resolve := slugBasenameResolver(placementsSlugToPath(placements))
	var matches []placementMeta
	for _, placement := range placements {
		if placement.Slug == event.Slug {
			matches = append(matches, placement)
		}
	}
	if len(matches) == 0 {
		return TargetedProjectionResult{NeedsSnapshot: true}, nil
	}
	result := TargetedProjectionResult{}
	for _, placement := range matches {
		projection, notModified, err := fetchProjectionConditional(ctx, client, cfg.BaseURL, creds.APIKey, event.Slug, placement.ETag)
		if err != nil {
			var projectionErr *projectionFetchError
			if errors.As(err, &projectionErr) && (projectionErr.Status == http.StatusForbidden || projectionErr.Status == http.StatusNotFound) {
				result.NeedsSnapshot = true
				continue
			}
			return TargetedProjectionResult{}, err
		}
		if notModified {
			needsRepair, err := placementNeedsLocalRepair(cfg.Root, placement, flavor)
			if err != nil {
				return TargetedProjectionResult{}, err
			}
			if !needsRepair {
				result.NotModified++
				continue
			}
			projection, _, err = fetchProjectionConditional(ctx, client, cfg.BaseURL, creds.APIKey, event.Slug, "")
			if err != nil {
				return TargetedProjectionResult{}, err
			}
		}
		if event.Revision > 0 && projection.Revision < event.Revision {
			return TargetedProjectionResult{}, fmt.Errorf("projection fetch for %s returned stale revision %d, expected at least %d", event.Slug, projection.Revision, event.Revision)
		}
		if event.ContentHash != "" && projection.Revision <= event.Revision && projection.ContentHash != event.ContentHash {
			return TargetedProjectionResult{}, fmt.Errorf("projection fetch for %s returned content hash %s, expected event hash %s", event.Slug, projection.ContentHash, event.ContentHash)
		}
		target := placement.CanonicalPath
		if target == "" {
			target = placement.Path
		}
		row := snapshotRow{
			VisibleInstanceID:        placement.VisibleInstanceID,
			Section:                  placement.Section,
			Kind:                     "document",
			DocSlug:                  event.Slug,
			BotletsOwnerHandle:       placement.BotletsOwnerHandle,
			BotletsBotSlug:           placement.BotletsBotSlug,
			BotletsBotLocalName:      placement.BotletsBotLocalName,
			BotletsBotID:             placement.BotletsBotID,
			BotletsBotHandle:         placement.BotletsBotHandle,
			BotletsBotAgentID:        placement.BotletsBotAgentID,
			BotletsBrainContainerID:  placement.BotletsBrainContainerID,
			BotletsBrainRootFolderID: placement.BotletsBrainRootFolderID,
			BotletsBrainNodeID:       placement.BotletsBrainNodeID,
		}
		snapshotID := liveProjectionSnapshotID
		if event.Cursor != "" {
			snapshotID = liveProjectionSnapshotID + ":" + event.Cursor
		}
		fr, err := buildFlavorRender(ctx, client, cfg.BaseURL, creds.APIKey, event.Slug, flavor, resolve)
		if err != nil {
			return TargetedProjectionResult{}, err
		}
		if _, err := writeProjection(ctx, paths, state, cfg.Root, cfg.BaseURL, target, row, projection, snapshotID, fr); err != nil {
			return TargetedProjectionResult{}, err
		}
		result.Refreshed++
	}
	return result, nil
}

type projectionFetchError struct {
	Slug   string
	Status int
	Body   string
}

func (e *projectionFetchError) Error() string {
	return fmt.Sprintf("projection fetch for %s failed: HTTP %d %s", e.Slug, e.Status, strings.TrimSpace(e.Body))
}

func fetchProjectionConditional(ctx context.Context, client *http.Client, baseURL, key, slug, etag string) (projectionResponse, bool, error) {
	var out projectionResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/docs/"+slug+"?projection=library-sync", nil)
	if err != nil {
		return out, false, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return out, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return out, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, false, &projectionFetchError{Slug: slug, Status: resp.StatusCode, Body: string(body)}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, false, err
	}
	if out.ContentHash == "" {
		out.ContentHash = sha256Hex(out.Markdown)
	}
	if out.ETag == "" {
		out.ETag = resp.Header.Get("ETag")
	}
	return out, false, nil
}

func readLiveCursor(paths commentbus.Paths, cfg Config) (string, error) {
	data, err := os.ReadFile(liveCursorFile(paths, cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	cursor := strings.TrimSpace(string(data))
	if cursor == "" || strings.ContainsAny(cursor, "\r\n\x00") {
		return "", nil
	}
	return cursor, nil
}

func writeLiveCursor(paths commentbus.Paths, cfg Config, cursor string) error {
	if cursor == "" || strings.ContainsAny(cursor, "\r\n\x00") {
		return nil
	}
	return os.WriteFile(liveCursorFile(paths, cfg), []byte(cursor+"\n"), 0o600)
}

func liveCursorFile(paths commentbus.Paths, cfg Config) string {
	keyID := cfg.KeyID
	if keyID == "" {
		keyID = "default"
	}
	keyID = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, keyID)
	return filepath.Join(paths.Home, "sync", "live-cursor-"+keyID)
}

func buildLiveEventsURL(baseURL, cursor string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/") + liveEventsEndpoint)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", errors.New("invalid live sync base url")
	}
	query := parsed.Query()
	query.Set("v", liveEventVersion)
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func dialLiveWebSocket(ctx context.Context, client *http.Client, rawURL, apiKey string) (net.Conn, *bufio.Reader, error) {
	if client != nil && client.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	addr := parsed.Host
	if !strings.Contains(addr, ":") {
		if parsed.Scheme == "wss" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	if parsed.Scheme == "wss" {
		host := parsed.Hostname()
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
		if client != nil {
			if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
				tlsCfg = transport.TLSClientConfig.Clone()
				if tlsCfg.ServerName == "" {
					tlsCfg.ServerName = host
				}
			}
		}
		conn = tls.Client(conn, tlsCfg)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stopClose := closeConnOnContextDone(ctx, conn)
	defer stopClose()
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	requestURI := parsed.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nAuthorization: Bearer %s\r\nUser-Agent: comment-sync/live\r\n\r\n", requestURI, parsed.Host, key, apiKey)
	if _, err := io.WriteString(conn, b.String()); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("live sync websocket failed: HTTP %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !headerHasToken(resp.Header.Values("Connection"), "upgrade") {
		_ = conn.Close()
		return nil, nil, errors.New("invalid live sync websocket upgrade response")
	}
	acceptHash := sha1.Sum([]byte(key + websocketAcceptGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(acceptHash[:])
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		_ = conn.Close()
		return nil, nil, errors.New("invalid live sync websocket accept response")
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reader, nil
}

func closeConnOnContextDone(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func writeWebSocketClientFrame(w io.Writer, opcode byte, payload []byte) error {
	if len(payload) > liveMaxFrameBytes {
		return errors.New("websocket payload is too large")
	}
	header := []byte{0x80 | (opcode & 0x0F)}
	switch {
	case len(payload) < 126:
		header = append(header, 0x80|byte(len(payload)))
	case len(payload) <= 0xFFFF:
		header = append(header, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		return errors.New("websocket payload is too large")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(mask); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

func readWebSocketServerFrame(r *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0F
	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(r, extended); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(r, extended); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended))
	}
	if length < 0 || length > liveMaxFrameBytes {
		return 0, nil, errors.New("websocket frame is too large")
	}
	var mask []byte
	if header[1]&0x80 != 0 {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(r, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if mask != nil {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}
