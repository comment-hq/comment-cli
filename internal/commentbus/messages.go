package commentbus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const (
	defaultLocalLeaseTTL = 10 * time.Minute
	maxMessageListLimit  = 200
)

// cloudRedeliveryCap bounds how many times a single cloud notification may be
// re-leased (claimed -> lease expired -> reopened) before the daemon stops the
// cycle and quarantines it (#301). The loop happens when the cloud-side ack
// never lands: comt.dev keeps re-delivering, the local lease keeps expiring,
// and the bot reprocesses the same message indefinitely. Package-level so tests
// can shrink it.
var cloudRedeliveryCap = 8

var (
	ErrMessageNotFound = errors.New("message not found")
	ErrMessageConflict = errors.New("message conflict")
)

type MessageEnvelope struct {
	ID         string          `json:"id"`
	Version    int             `json:"version"`
	Kind       string          `json:"kind"`
	Source     string          `json:"source"`
	Profile    string          `json:"profile"`
	BotName    string          `json:"bot_name"`
	BotID      string          `json:"bot_id,omitempty"`
	BotAgentID string          `json:"bot_agent_id,omitempty"`
	From       string          `json:"from"`
	To         []string        `json:"to"`
	ThreadID   *string         `json:"thread_id"`
	CreatedAt  string          `json:"created_at"`
	Body       MessageBody     `json:"body"`
	Refs       map[string]any  `json:"refs"`
	Delivery   MessageDelivery `json:"delivery"`
}

type MessageBody struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

type MessageDelivery struct {
	State             string              `json:"state"`
	ClaimHolder       *string             `json:"claim_holder"`
	SessionID         *string             `json:"session_id"`
	SessionScope      MessageSessionScope `json:"session_scope"`
	SessionGeneration *string             `json:"session_generation"`
	ReadAt            *string             `json:"read_at"`
	LeaseExpiresAt    *string             `json:"lease_expires_at"`
}

type MessageSessionScope struct {
	Type *string `json:"type"`
	ID   *string `json:"id"`
}

type MessageWaitSummary struct {
	MessageID      string         `json:"message_id"`
	Profile        string         `json:"profile"`
	BotName        string         `json:"bot_name"`
	BotID          string         `json:"bot_id,omitempty"`
	BotAgentID     string         `json:"bot_agent_id,omitempty"`
	Kind           string         `json:"kind"`
	Source         string         `json:"source"`
	CreatedAt      string         `json:"created_at"`
	Refs           map[string]any `json:"refs,omitempty"`
	LeaseExpiresAt *string        `json:"lease_expires_at,omitempty"`
}

func messageWaitSummaryFromEnvelope(envelope MessageEnvelope) MessageWaitSummary {
	leaseExpiresAt := envelope.Delivery.LeaseExpiresAt
	if envelope.Source == "comment.io" {
		leaseExpiresAt = nil
	}
	return MessageWaitSummary{
		MessageID:      envelope.ID,
		Profile:        envelope.Profile,
		BotName:        envelope.BotName,
		BotID:          envelope.BotID,
		BotAgentID:     envelope.BotAgentID,
		Kind:           envelope.Kind,
		Source:         envelope.Source,
		CreatedAt:      envelope.CreatedAt,
		Refs:           envelope.Refs,
		LeaseExpiresAt: leaseExpiresAt,
	}
}

type LocalMessageRecipient struct {
	Profile    string
	BotName    string
	BotID      string
	BotAgentID string
}

type LocalMessageSend struct {
	SenderProfile    string
	SenderBotName    string
	SenderBotID      string
	SenderBotAgentID string
	Recipients       []LocalMessageRecipient
	Body             MessageBody
	Refs             map[string]any
	ThreadID         *string
	IdempotencyKey   string
	Now              time.Time
}

type LocalMessageSendResult struct {
	OutboxID       string
	Messages       []MessageEnvelope
	DispatchErrors []MessageDispatchError
	Replayed       bool
}

type MessageDispatchError struct {
	MessageID string `json:"message_id"`
	Profile   string `json:"profile"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type CloudNotificationMessage struct {
	ID             string
	Profile        string
	BotName        string
	BotID          string
	BotAgentID     string
	Kind           string
	From           string
	Body           MessageBody
	Refs           map[string]any
	ThreadID       *string
	NotificationID string
	CreatedAt      string
	LeaseExpiresAt string
	Now            time.Time
}

type MessageListFilter struct {
	Profile                   string
	BotName                   string
	BotID                     string
	BotAgentID                string
	AllowIdentityProfileDrift bool
	Source                    string
	State                     string
	Holder                    string
	ActiveOnly                bool
	Limit                     int
	Cursor                    string
	Kinds                     []string
}

type MessageClaimOptions struct {
	Profile           string
	MessageID         string
	ClaimHolder       string
	SessionID         *string
	SessionScopeType  *string
	SessionScopeID    *string
	SessionGeneration *string
	LeaseTTL          time.Duration
	Now               time.Time
}

type outboxPayload struct {
	Recipients  []string `json:"recipients"`
	MessageIDs  []string `json:"message_ids"`
	Fingerprint string   `json:"fingerprint"`
}

func (s *Store) InsertLocalMessages(ctx context.Context, input LocalMessageSend) (LocalMessageSendResult, error) {
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.Body.Format == "" {
		input.Body.Format = "markdown"
	}
	if input.Refs == nil {
		input.Refs = map[string]any{}
	}
	if !safeOptionalRegistryIdentity(input.SenderBotID) || !safeOptionalRegistryIdentity(input.SenderBotAgentID) {
		return LocalMessageSendResult{}, ErrMessageConflict
	}
	if len(input.Recipients) == 0 {
		return LocalMessageSendResult{}, ErrMessageNotFound
	}
	for _, recipient := range input.Recipients {
		if !safeOptionalRegistryIdentity(recipient.BotID) || !safeOptionalRegistryIdentity(recipient.BotAgentID) {
			return LocalMessageSendResult{}, ErrMessageConflict
		}
	}
	fingerprint, err := localSendFingerprint(input)
	if err != nil {
		return LocalMessageSendResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LocalMessageSendResult{}, err
	}
	defer tx.Rollback()

	if input.IdempotencyKey != "" {
		replayed, ok, err := s.replayOutbox(ctx, tx, input.IdempotencyKey, input, fingerprint)
		if err != nil {
			return LocalMessageSendResult{}, err
		}
		if ok {
			return LocalMessageSendResult{OutboxID: input.IdempotencyKey, Messages: replayed, Replayed: true}, tx.Commit()
		}
	}

	createdAt := busTime(input.Now)
	refsJSON, err := json.Marshal(input.Refs)
	if err != nil {
		return LocalMessageSendResult{}, err
	}
	var out []MessageEnvelope
	for _, recipient := range input.Recipients {
		messageID, err := GenerateLocalID("msg", 0)
		if err != nil {
			return LocalMessageSendResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, source, kind, thread_id, sender, profile, bot_name, bot_id, bot_agent_id, body_format, body_content, refs_json, created_at, retention_bucket)
			VALUES (?, 'local', 'message', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'default')`,
			messageID,
			nullableString(input.ThreadID),
			"@"+input.SenderProfile,
			recipient.Profile,
			recipient.BotName,
			recipient.BotID,
			recipient.BotAgentID,
			input.Body.Format,
			input.Body.Content,
			string(refsJSON),
			createdAt,
		); err != nil {
			return LocalMessageSendResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients (message_id, profile, handle, bot_id, bot_agent_id, delivery_state)
			VALUES (?, ?, ?, ?, ?, 'unclaimed')`, messageID, recipient.Profile, recipient.Profile, recipient.BotID, recipient.BotAgentID); err != nil {
			return LocalMessageSendResult{}, err
		}
		if err := appendMessageEvent(ctx, tx, messageID, recipient.Profile, "message.sent", map[string]any{
			"message_id": messageID,
			"profile":    recipient.Profile,
			"source":     "local",
			"kind":       "message",
		}, input.Now); err != nil {
			return LocalMessageSendResult{}, err
		}
		envelope, err := queryMessageEnvelope(ctx, tx, messageID, recipient.Profile)
		if err != nil {
			return LocalMessageSendResult{}, err
		}
		out = append(out, senderVisibleEnvelope(envelope))
	}
	if input.IdempotencyKey != "" {
		payload := outboxPayload{
			Recipients:  recipientFingerprintKeys(input.Recipients),
			MessageIDs:  messageIDs(out),
			Fingerprint: fingerprint,
		}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			return LocalMessageSendResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox (idempotency_key, sender_profile, sender_bot_id, sender_bot_agent_id, recipient_profiles_json, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'done', ?, ?)`, input.IdempotencyKey, input.SenderProfile, input.SenderBotID, input.SenderBotAgentID, string(payloadJSON), createdAt, createdAt); err != nil {
			return LocalMessageSendResult{}, err
		}
	}
	return LocalMessageSendResult{OutboxID: input.IdempotencyKey, Messages: out}, tx.Commit()
}

func (s *Store) InsertCloudNotificationMessage(ctx context.Context, input CloudNotificationMessage) (MessageEnvelope, error) {
	if !LocalMessageIDRE.MatchString(input.ID) || !ProfileRE.MatchString(input.Profile) || !isBotName(input.BotName) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if input.Now.IsZero() {
		input.Now = time.Now().UTC()
	}
	if input.Body.Format == "" {
		input.Body.Format = "markdown"
	}
	if input.Refs == nil {
		input.Refs = map[string]any{}
	}
	if !safeOptionalRegistryIdentity(input.BotID) || !safeOptionalRegistryIdentity(input.BotAgentID) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if !isMessageKind(input.Kind) || input.Body.Format != "markdown" || len(input.Body.Content) > 1_000_000 || strings.ContainsRune(input.Body.Content, '\x00') || containsSecretValue(input.Body.Content) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if !isSafeRefs(input.Refs) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if input.NotificationID != "" && !isSafeCloudID("notification", input.NotificationID) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if input.LeaseExpiresAt == "" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	leaseExpiresAt, err := time.Parse(time.RFC3339Nano, input.LeaseExpiresAt)
	if err != nil {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if input.From == "" || len(input.From) > 256 || strings.ContainsAny(input.From, "\r\n\x00") || containsSecretValue(input.From) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if input.ThreadID != nil && (len(*input.ThreadID) > 256 || strings.ContainsAny(*input.ThreadID, "\r\n\x00") || containsSecretValue(*input.ThreadID)) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	createdAt := input.CreatedAt
	if createdAt == "" {
		createdAt = busTime(input.Now)
	}
	parsedCreatedAt, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return MessageEnvelope{}, ErrMessageConflict
	}
	createdAt = busTime(parsedCreatedAt)
	canonicalLeaseExpiresAt := busTime(leaseExpiresAt)
	refsJSON, err := json.Marshal(input.Refs)
	if err != nil {
		return MessageEnvelope{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages (id, source, kind, thread_id, sender, profile, bot_name, bot_id, bot_agent_id, body_format, body_content, refs_json, created_at, retention_bucket)
		VALUES (?, 'comment.io', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'default')`,
		input.ID,
		input.Kind,
		nullableString(input.ThreadID),
		input.From,
		input.Profile,
		input.BotName,
		input.BotID,
		input.BotAgentID,
		input.Body.Format,
		input.Body.Content,
		string(refsJSON),
		createdAt,
	); err != nil {
		return MessageEnvelope{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients (message_id, profile, handle, bot_id, bot_agent_id, delivery_state, lease_expires_at)
		VALUES (?, ?, ?, ?, ?, 'unclaimed', ?)`, input.ID, input.Profile, input.Profile, input.BotID, input.BotAgentID, canonicalLeaseExpiresAt); err != nil {
		return MessageEnvelope{}, err
	}
	if err := appendMessageEvent(ctx, tx, input.ID, input.Profile, "message.ingested", map[string]any{
		"message_id": input.ID,
		"profile":    input.Profile,
		"source":     "comment.io",
		"kind":       input.Kind,
	}, input.Now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, input.ID, input.Profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

// RefreshCloudNotificationLease applies a re-delivered cloud notification's
// lease. It returns quarantined=true when the message had been re-delivered too
// many times (the #301 redelivery loop) and was terminally stopped instead of
// being reopened, so the caller can warn-log the stuck loop.
func (s *Store) RefreshCloudNotificationLease(ctx context.Context, profile string, messageID string, leaseExpiresAt string, now time.Time, reopenClaimed bool) (quarantined bool, err error) {
	parsedLeaseExpiresAt, err := time.Parse(time.RFC3339Nano, leaseExpiresAt)
	if err != nil {
		return false, ErrMessageConflict
	}
	leaseExpiresAt = busTime(parsedLeaseExpiresAt)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return false, err
	}
	if envelope.Source != "comment.io" {
		return false, ErrMessageConflict
	}
	switch envelope.Delivery.State {
	case "unclaimed":
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET lease_expires_at = ? WHERE message_id = ? AND profile = ?`, leaseExpiresAt, messageID, profile); err != nil {
			return false, err
		}
	case "released":
		// A released cloud notification the server still returns as unread keeps
		// being re-offered and, once reopened, can sit ahead of newer mentions —
		// the stale-backlog blocker from #165. Route it through the same
		// redelivery guard as the claimed loop so it is bounded and surfaced.
		quarantined, err := applyCloudRedeliveryGuard(ctx, tx, messageID, profile, leaseExpiresAt, now)
		if err != nil {
			return false, err
		}
		if quarantined {
			return true, tx.Commit()
		}
	case "claimed":
		if reopenClaimed || leaseExpired(envelope.Delivery.LeaseExpiresAt, now) {
			quarantined, err := applyCloudRedeliveryGuard(ctx, tx, messageID, profile, leaseExpiresAt, now)
			if err != nil {
				return false, err
			}
			if quarantined {
				return true, tx.Commit()
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET lease_expires_at = ? WHERE message_id = ? AND profile = ?`, leaseExpiresAt, messageID, profile); err != nil {
				return false, err
			}
		}
	}
	return false, tx.Commit()
}

// applyCloudRedeliveryGuard reopens a re-delivered cloud notification to
// 'unclaimed' so it can be processed again — unless it has already been requeued
// cloudRedeliveryCap times. The redelivery loop happens when the cloud-side ack
// never lands (#301) or a released notification stays unread server-side (#165):
// comt.dev keeps re-delivering it and it keeps reopening, so the bot reprocesses
// it forever and it blocks newer mentions. Past the cap, stop the cycle
// instead: mark it terminally 'acked' (inert to the claim/ready/refresh paths,
// which all require 'unclaimed') and record a structured `message.redelivery_warning`
// so the previously-silent loop is visible. Returns quarantined.
func applyCloudRedeliveryGuard(ctx context.Context, tx queryer, messageID string, profile string, leaseExpiresAt string, now time.Time) (bool, error) {
	priorRequeues, err := countCloudRequeues(ctx, tx, messageID, profile)
	if err != nil {
		return false, err
	}
	if priorRequeues >= cloudRedeliveryCap {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
			SET delivery_state = 'acked', claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
			WHERE message_id = ? AND profile = ?`, messageID, profile); err != nil {
			return false, err
		}
		if err := appendMessageEvent(ctx, tx, messageID, profile, "message.redelivery_warning", map[string]any{
			"message_id":   messageID,
			"profile":      profile,
			"source":       "comment.io",
			"redeliveries": priorRequeues,
			"cap":          cloudRedeliveryCap,
			"action":       "quarantined",
			"reason":       "redelivery_cap_exceeded",
		}, now); err != nil {
			return false, err
		}
		return true, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
		SET delivery_state = 'unclaimed', claim_holder = NULL, lease_expires_at = ?, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
		WHERE message_id = ? AND profile = ?`, leaseExpiresAt, messageID, profile); err != nil {
		return false, err
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, "message.requeued", map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     "comment.io",
		"reason":     "cloud_lease_refreshed",
	}, now); err != nil {
		return false, err
	}
	return false, nil
}

// countCloudRequeues returns how many times a cloud notification has already
// been requeued (re-leased from claimed/released -> unclaimed) for this profile,
// read from the durable events journal so no extra schema is needed.
func countCloudRequeues(ctx context.Context, q queryer, messageID string, profile string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE message_id = ? AND profile = ? AND event_type = 'message.requeued'`,
		messageID, profile,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) QuarantineCloudMessage(ctx context.Context, profile string, messageID string, reason string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Source != "comment.io" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if envelope.Delivery.State == "released" {
		return envelope, tx.Commit()
	}
	if envelope.Delivery.State != "unclaimed" && envelope.Delivery.State != "claimed" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
		SET delivery_state = 'released', claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
		WHERE message_id = ? AND profile = ?`, messageID, profile); err != nil {
		return MessageEnvelope{}, err
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, "message.released", map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     "comment.io",
		"reason":     reason,
	}, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) replayOutbox(ctx context.Context, tx *sql.Tx, idempotencyKey string, input LocalMessageSend, fingerprint string) ([]MessageEnvelope, bool, error) {
	var storedSender, storedBotID, storedBotAgentID, payloadJSON string
	err := tx.QueryRowContext(ctx, `SELECT sender_profile, sender_bot_id, sender_bot_agent_id, recipient_profiles_json FROM outbox WHERE idempotency_key = ?`, idempotencyKey).Scan(&storedSender, &storedBotID, &storedBotAgentID, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if stableBotIdentityConflict(storedBotID, storedBotAgentID, input.SenderBotID, input.SenderBotAgentID) {
		return nil, true, ErrMessageConflict
	}
	if storedSender != input.SenderProfile && !sameStableBotIdentity(storedBotID, storedBotAgentID, input.SenderBotID, input.SenderBotAgentID) {
		return nil, true, ErrMessageConflict
	}
	var payload outboxPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return nil, true, err
	}
	if payload.Fingerprint != fingerprint {
		matches, err := outboxMessagesMatchInput(ctx, tx, payload.MessageIDs, input)
		if err != nil {
			return nil, true, err
		}
		if !matches {
			return nil, true, ErrMessageConflict
		}
	}
	out := make([]MessageEnvelope, 0, len(payload.MessageIDs))
	for _, messageID := range payload.MessageIDs {
		envelope, err := queryMessageEnvelopeAnyProfile(ctx, tx, messageID)
		if err != nil {
			return nil, true, err
		}
		out = append(out, senderVisibleEnvelope(envelope))
	}
	return out, true, nil
}

func (s *Store) WaitMessageSummary(ctx context.Context, filter MessageListFilter) (*MessageWaitSummary, error) {
	filter.normalize()
	envelopes, err := s.listInboxMessages(ctx, filter, true)
	if err != nil {
		return nil, err
	}
	if len(envelopes) == 0 {
		return nil, nil
	}
	envelope := envelopes[0]
	summary := messageWaitSummaryFromEnvelope(envelope)
	return &summary, nil
}

func (s *Store) WaitMessageSummaries(ctx context.Context, filter MessageListFilter, limit int) ([]MessageWaitSummary, error) {
	if limit <= 0 || limit > maxMessageListLimit {
		limit = 50
	}
	filter.Limit = limit
	filter.normalize()
	envelopes, err := s.listInboxMessages(ctx, filter, true)
	if err != nil {
		return nil, err
	}
	summaries := make([]MessageWaitSummary, 0, len(envelopes))
	for _, envelope := range envelopes {
		summaries = append(summaries, messageWaitSummaryFromEnvelope(envelope))
	}
	return summaries, nil
}

func (s *Store) WaitCloudNotificationMessage(ctx context.Context, profile string, botName string) (*MessageEnvelope, error) {
	now := busTime(time.Now())
	args := []any{profile, now}
	where := `WHERE r.profile = ? AND m.source = 'comment.io' AND r.delivery_state = 'unclaimed' AND r.lease_expires_at IS NOT NULL AND r.lease_expires_at > ?`
	if botName != "" {
		where += ` AND m.bot_name = ?`
		args = append(args, botName)
	}
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL(where+` ORDER BY m.created_at ASC, m.id ASC LIMIT 1`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	envelopes, err := scanMessageEnvelopes(rows)
	if err != nil {
		return nil, err
	}
	if len(envelopes) == 0 {
		return nil, nil
	}
	return &envelopes[0], nil
}

func (s *Store) ListInboxMessages(ctx context.Context, filter MessageListFilter) ([]MessageEnvelope, error) {
	filter.normalize()
	return s.listInboxMessages(ctx, filter, false)
}

func (s *Store) GetInboxMessage(ctx context.Context, profile string, messageID string) (MessageEnvelope, error) {
	return queryMessageEnvelope(ctx, s.db, messageID, profile)
}

func (s *Store) GetInboxMessageByID(ctx context.Context, messageID string) (MessageEnvelope, error) {
	return queryMessageEnvelopeAnyProfile(ctx, s.db, messageID)
}

func (s *Store) ListInboxMessagesByBotIdentity(ctx context.Context, botID string, botAgentID string, kinds []string, limit int, cursor string) ([]MessageEnvelope, error) {
	if limit <= 0 || limit > maxMessageListLimit {
		limit = 50
	}
	botID = strings.TrimSpace(botID)
	botAgentID = strings.TrimSpace(botAgentID)
	if botID == "" && botAgentID == "" {
		return nil, nil
	}
	var args []any
	var where []string
	if !appendMessageBotIdentityWhere(&where, &args, botID, botAgentID) {
		return nil, nil
	}
	if len(kinds) > 0 {
		placeholders := make([]string, len(kinds))
		for i, kind := range kinds {
			placeholders[i] = "?"
			args = append(args, kind)
		}
		where = append(where, "m.kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if cursor != "" {
		where = append(where, "(m.created_at, m.id) > (SELECT created_at, id FROM messages WHERE id = ?)")
		args = append(args, cursor)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL("WHERE "+strings.Join(where, " AND ")+" ORDER BY m.created_at ASC, m.id ASC LIMIT ?"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageEnvelopes(rows)
}

func (s *Store) ListClaimedMessages(ctx context.Context) ([]MessageEnvelope, error) {
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL("WHERE r.delivery_state = 'claimed' ORDER BY m.created_at ASC, m.id ASC"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageEnvelopes(rows)
}

// UnclaimedCountsByProfile returns, per profile, the number of unclaimed
// (queued, not-yet-picked-up) message recipients. Profiles with zero unclaimed
// messages are omitted. Surfaced in `comment daemon health` so an operator can
// tell at a glance whether a profile has a backlog piling up — not just whether
// the daemon is connected (bug #95). A non-empty count on a profile with no
// active notification poller is the silent-queue case the daemon used to hide.
func (s *Store) UnclaimedCountsByProfile(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT profile, COUNT(*) FROM message_recipients WHERE delivery_state = 'unclaimed' GROUP BY profile`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var profile string
		var count int
		if err := rows.Scan(&profile, &count); err != nil {
			return nil, err
		}
		counts[profile] = count
	}
	return counts, rows.Err()
}

func (s *Store) listInboxMessages(ctx context.Context, filter MessageListFilter, waitOnly bool) ([]MessageEnvelope, error) {
	var args []any
	var where []string
	matchedByStableIdentity := appendMessageBotIdentityWhere(&where, &args, filter.BotID, filter.BotAgentID)
	if filter.Profile != "" && (!matchedByStableIdentity || !filter.AllowIdentityProfileDrift) {
		where = append(where, "r.profile = ?")
		args = append(args, filter.Profile)
	}
	if !matchedByStableIdentity && filter.BotName != "" {
		where = append(where, "m.bot_name = ?")
		args = append(args, filter.BotName)
	}
	if filter.Source != "" {
		where = append(where, "m.source = ?")
		args = append(args, filter.Source)
	}
	if filter.State != "" {
		where = append(where, "r.delivery_state = ?")
		args = append(args, filter.State)
	}
	if len(filter.Kinds) > 0 {
		placeholders := make([]string, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			placeholders[i] = "?"
			args = append(args, kind)
		}
		where = append(where, "m.kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if filter.Cursor != "" {
		where = append(where, "(m.created_at, m.id) > (SELECT created_at, id FROM messages WHERE id = ?)")
		args = append(args, filter.Cursor)
	}
	if filter.Holder != "" {
		where = append(where, "r.claim_holder = ?")
		args = append(args, filter.Holder)
	}
	if filter.ActiveOnly {
		where = append(where, "r.lease_expires_at IS NOT NULL AND r.lease_expires_at > ?")
		args = append(args, busTime(time.Now()))
	}
	if waitOnly {
		now := busTime(time.Now())
		where = append(where, `(
			(m.source = 'comment.io' AND r.delivery_state = 'unclaimed' AND r.lease_expires_at IS NOT NULL AND r.lease_expires_at > ?)
			OR (m.source != 'comment.io' AND (r.delivery_state = 'unclaimed' OR (r.delivery_state = 'claimed' AND r.lease_expires_at IS NOT NULL AND r.lease_expires_at <= ?)))
		)`)
		args = append(args, now, now)
	}
	if len(where) == 0 {
		return nil, nil
	}
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL("WHERE "+strings.Join(where, " AND ")+" ORDER BY m.created_at ASC, m.id ASC LIMIT ?"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageEnvelopes(rows)
}

func (s *Store) ListSentMessages(ctx context.Context, senderProfile string, limit int, cursor string) ([]MessageEnvelope, error) {
	if limit <= 0 || limit > maxMessageListLimit {
		limit = 50
	}
	args := []any{"@" + senderProfile}
	where := []string{"m.sender = ?", "m.source = 'local'"}
	if cursor != "" {
		where = append(where, "(m.created_at, m.id) > (SELECT created_at, id FROM messages WHERE id = ?)")
		args = append(args, cursor)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL("WHERE "+strings.Join(where, " AND ")+" ORDER BY m.created_at ASC, m.id ASC LIMIT ?"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	envelopes, err := scanMessageEnvelopes(rows)
	if err != nil {
		return nil, err
	}
	for i := range envelopes {
		envelopes[i] = senderVisibleEnvelope(envelopes[i])
	}
	return envelopes, nil
}

func (s *Store) HasActiveSessionClaim(ctx context.Context, claimHolder string, botName string, now time.Time) (bool, error) {
	return s.HasActiveSessionClaimForBot(ctx, claimHolder, botName, "", "", now)
}

func (s *Store) HasActiveSessionClaimForBot(ctx context.Context, claimHolder string, botName string, botID string, botAgentID string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return hasActiveSessionClaim(ctx, s.db, claimHolder, "", botName, botID, botAgentID, now)
}

func (s *Store) HasActiveSessionClaimExcluding(ctx context.Context, claimHolder string, botName string, excludeMessageID string, now time.Time) (bool, error) {
	return s.HasActiveSessionClaimForBotExcluding(ctx, claimHolder, botName, "", "", excludeMessageID, now)
}

func (s *Store) HasActiveSessionClaimForBotExcluding(ctx context.Context, claimHolder string, botName string, botID string, botAgentID string, excludeMessageID string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return hasActiveSessionClaim(ctx, s.db, claimHolder, excludeMessageID, botName, botID, botAgentID, now)
}

func (s *Store) ReleaseClaimsForHolder(ctx context.Context, claimHolder string, reason string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT r.message_id, r.profile
		FROM message_recipients r
		JOIN messages m ON m.id = r.message_id AND m.profile = r.profile
		WHERE r.claim_holder = ? AND r.delivery_state = 'claimed' AND m.source != 'comment.io'
		ORDER BY r.message_id ASC`, claimHolder)
	if err != nil {
		return nil, err
	}
	var claims []struct {
		messageID string
		profile   string
	}
	for rows.Next() {
		var claim struct {
			messageID string
			profile   string
		}
		if err := rows.Scan(&claim.messageID, &claim.profile); err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	released := make([]string, 0, len(claims))
	for _, claim := range claims {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
			SET delivery_state = 'unclaimed', claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
			WHERE message_id = ? AND profile = ? AND claim_holder = ? AND delivery_state = 'claimed'`,
			claim.messageID,
			claim.profile,
			claimHolder,
		); err != nil {
			return nil, err
		}
		if err := appendMessageEvent(ctx, tx, claim.messageID, claim.profile, "message.released", map[string]any{
			"message_id": claim.messageID,
			"profile":    claim.profile,
			"state":      "unclaimed",
			"reason":     reason,
		}, now); err != nil {
			return nil, err
		}
		released = append(released, claim.messageID)
	}
	return released, tx.Commit()
}

func (s *Store) RequeueCloudClaimsForHolder(ctx context.Context, claimHolder string, reason string, now time.Time) ([]string, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `SELECT r.message_id, r.profile
		FROM message_recipients r
		JOIN messages m ON m.id = r.message_id AND m.profile = r.profile
		WHERE r.claim_holder = ? AND r.delivery_state = 'claimed' AND m.source = 'comment.io' AND r.lease_expires_at IS NOT NULL AND r.lease_expires_at > ?
		ORDER BY r.message_id ASC`, claimHolder, busTime(now))
	if err != nil {
		return nil, err
	}
	var claims []struct {
		messageID string
		profile   string
	}
	for rows.Next() {
		var claim struct {
			messageID string
			profile   string
		}
		if err := rows.Scan(&claim.messageID, &claim.profile); err != nil {
			rows.Close()
			return nil, err
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	requeued := make([]string, 0, len(claims))
	for _, claim := range claims {
		if err := requeueCloudMessageLocallyTx(ctx, tx, claim.profile, claim.messageID, claimHolder, reason, now); err != nil {
			return nil, err
		}
		requeued = append(requeued, claim.messageID)
	}
	return requeued, tx.Commit()
}

func (s *Store) ListCloudClaimsForHolder(ctx context.Context, claimHolder string) ([]MessageEnvelope, error) {
	rows, err := s.db.QueryContext(ctx, messageEnvelopeSelectSQL(`WHERE r.claim_holder = ? AND r.delivery_state = 'claimed' AND m.source = 'comment.io' ORDER BY m.created_at ASC, m.id ASC`), claimHolder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessageEnvelopes(rows)
}

func (s *Store) RequeueCloudMessageLocally(ctx context.Context, profile string, messageID string, claimHolder string, reason string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	if err := requeueCloudMessageLocallyTx(ctx, tx, profile, messageID, claimHolder, reason, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func requeueCloudMessageLocallyTx(ctx context.Context, tx *sql.Tx, profile string, messageID string, claimHolder string, reason string, now time.Time) error {
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return err
	}
	if envelope.Source != "comment.io" || envelope.Delivery.State != "claimed" || stringValue(envelope.Delivery.ClaimHolder) != claimHolder {
		return ErrMessageConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
		SET delivery_state = 'unclaimed', claim_holder = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
		WHERE message_id = ? AND profile = ? AND claim_holder = ? AND delivery_state = 'claimed'`,
		messageID,
		profile,
		claimHolder,
	); err != nil {
		return err
	}
	return appendMessageEvent(ctx, tx, messageID, profile, "message.released", map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     "comment.io",
		"state":      "unclaimed",
		"reason":     reason,
	}, now)
}

func (s *Store) RequeueClaimedMessage(ctx context.Context, profile string, messageID string, reason string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Source == "comment.io" || envelope.Delivery.State != "claimed" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
		SET delivery_state = 'unclaimed', claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
		WHERE message_id = ? AND profile = ? AND delivery_state = 'claimed'`,
		messageID,
		profile,
	); err != nil {
		return MessageEnvelope{}, err
	}
	if reason == "" {
		reason = "startup_reconciliation"
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, "message.requeued", map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     envelope.Source,
		"reason":     reason,
	}, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) ClaimMessage(ctx context.Context, options MessageClaimOptions) (MessageEnvelope, error) {
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if options.LeaseTTL <= 0 {
		options.LeaseTTL = defaultLocalLeaseTTL
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()

	envelope, err := queryMessageEnvelope(ctx, tx, options.MessageID, options.Profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Delivery.State == "acked" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if envelope.Source == "comment.io" {
		if envelope.Delivery.State != "unclaimed" || envelope.Delivery.LeaseExpiresAt == nil || leaseExpired(envelope.Delivery.LeaseExpiresAt, options.Now) {
			return MessageEnvelope{}, ErrMessageConflict
		}
	}
	if envelope.Delivery.State == "claimed" {
		expired := leaseExpired(envelope.Delivery.LeaseExpiresAt, options.Now)
		if !expired && stringValue(envelope.Delivery.ClaimHolder) != options.ClaimHolder {
			return MessageEnvelope{}, ErrMessageConflict
		}
	}
	if options.SessionID != nil {
		active, err := hasActiveSessionClaim(ctx, tx, options.ClaimHolder, options.MessageID, envelope.BotName, envelope.BotID, envelope.BotAgentID, options.Now)
		if err != nil {
			return MessageEnvelope{}, err
		}
		if active {
			return MessageEnvelope{}, ErrMessageConflict
		}
	}
	readAt := busTime(options.Now)
	leaseExpiresAt := busTime(options.Now.Add(options.LeaseTTL))
	if envelope.Source == "comment.io" && envelope.Delivery.LeaseExpiresAt != nil {
		leaseExpiresAt = *envelope.Delivery.LeaseExpiresAt
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
		SET delivery_state = 'claimed', claim_holder = ?, lease_expires_at = ?, session_id = ?, session_scope_type = ?, session_scope_id = ?, session_generation = ?, read_at = ?
		WHERE message_id = ? AND profile = ?`,
		options.ClaimHolder,
		leaseExpiresAt,
		nullableString(options.SessionID),
		nullableString(options.SessionScopeType),
		nullableString(options.SessionScopeID),
		nullableString(options.SessionGeneration),
		readAt,
		options.MessageID,
		options.Profile,
	); err != nil {
		return MessageEnvelope{}, err
	}
	if err := appendMessageEvent(ctx, tx, options.MessageID, options.Profile, "message.received", map[string]any{
		"message_id": options.MessageID,
		"profile":    options.Profile,
		"state":      "claimed",
	}, options.Now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, options.MessageID, options.Profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) RenewMessage(ctx context.Context, profile string, messageID string, claimHolder string, ttl time.Duration, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if ttl <= 0 {
		ttl = defaultLocalLeaseTTL
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Delivery.State != "claimed" || stringValue(envelope.Delivery.ClaimHolder) != claimHolder || leaseExpired(envelope.Delivery.LeaseExpiresAt, now) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	leaseExpiresAt := busTime(now.Add(ttl))
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET lease_expires_at = ? WHERE message_id = ? AND profile = ?`, leaseExpiresAt, messageID, profile); err != nil {
		return MessageEnvelope{}, err
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, "message.renewed", map[string]any{
		"message_id": messageID,
		"profile":    profile,
	}, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) RenewCloudMessage(ctx context.Context, profile string, messageID string, claimHolder string, leaseExpiresAt string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	parsedLeaseExpiresAt, err := time.Parse(time.RFC3339Nano, leaseExpiresAt)
	if err != nil {
		return MessageEnvelope{}, ErrMessageConflict
	}
	leaseExpiresAt = busTime(parsedLeaseExpiresAt)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Source != "comment.io" || envelope.Delivery.State != "claimed" || stringValue(envelope.Delivery.ClaimHolder) != claimHolder {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET lease_expires_at = ? WHERE message_id = ? AND profile = ?`, leaseExpiresAt, messageID, profile); err != nil {
		return MessageEnvelope{}, err
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, "message.renewed", map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     "comment.io",
	}, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) AckMessage(ctx context.Context, profile string, messageID string, claimHolder string, now time.Time) (MessageEnvelope, error) {
	return s.finishClaimedMessage(ctx, profile, messageID, claimHolder, "acked", "", now)
}

func (s *Store) ReleaseMessage(ctx context.Context, profile string, messageID string, claimHolder string, reason string, now time.Time) (MessageEnvelope, error) {
	return s.finishClaimedMessage(ctx, profile, messageID, claimHolder, "unclaimed", reason, now)
}

func (s *Store) AckCloudMessage(ctx context.Context, profile string, messageID string, claimHolder string, now time.Time) (MessageEnvelope, error) {
	return s.finishCloudClaimedMessage(ctx, profile, messageID, claimHolder, "acked", "", now)
}

func (s *Store) ReleaseCloudMessage(ctx context.Context, profile string, messageID string, claimHolder string, reason string, now time.Time) (MessageEnvelope, error) {
	return s.finishCloudClaimedMessage(ctx, profile, messageID, claimHolder, "released", reason, now)
}

func (s *Store) finishCloudClaimedMessage(ctx context.Context, profile string, messageID string, claimHolder string, nextState string, reason string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Source != "comment.io" {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if envelope.Delivery.State == nextState {
		return envelope, tx.Commit()
	}
	if envelope.Delivery.State != "claimed" || stringValue(envelope.Delivery.ClaimHolder) != claimHolder {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if nextState == "acked" {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET delivery_state = 'acked' WHERE message_id = ? AND profile = ?`, messageID, profile); err != nil {
			return MessageEnvelope{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
			SET delivery_state = ?, claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
			WHERE message_id = ? AND profile = ?`, nextState, messageID, profile); err != nil {
			return MessageEnvelope{}, err
		}
	}
	eventType := "message.acked"
	if nextState == "released" {
		eventType = "message.released"
	}
	eventData := map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"source":     "comment.io",
		"state":      nextState,
	}
	if nextState == "released" && reason != "" {
		eventData["reason"] = reason
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, eventType, eventData, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func (s *Store) finishClaimedMessage(ctx context.Context, profile string, messageID string, claimHolder string, nextState string, reason string, now time.Time) (MessageEnvelope, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MessageEnvelope{}, err
	}
	defer tx.Rollback()
	envelope, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Delivery.State == "acked" && nextState == "acked" {
		if stringValue(envelope.Delivery.ClaimHolder) != claimHolder {
			return MessageEnvelope{}, ErrMessageConflict
		}
		return envelope, tx.Commit()
	}
	if envelope.Delivery.State == "unclaimed" && nextState == "unclaimed" {
		if !strings.HasPrefix(claimHolder, "owner:") {
			return MessageEnvelope{}, ErrMessageConflict
		}
		return envelope, tx.Commit()
	}
	if envelope.Delivery.State != "claimed" || stringValue(envelope.Delivery.ClaimHolder) != claimHolder || leaseExpired(envelope.Delivery.LeaseExpiresAt, now) {
		return MessageEnvelope{}, ErrMessageConflict
	}
	if nextState == "acked" {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients SET delivery_state = 'acked' WHERE message_id = ? AND profile = ?`, messageID, profile); err != nil {
			return MessageEnvelope{}, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
			SET delivery_state = 'unclaimed', claim_holder = NULL, lease_expires_at = NULL, session_id = NULL, session_scope_type = NULL, session_scope_id = NULL, session_generation = NULL, read_at = NULL
			WHERE message_id = ? AND profile = ?`, messageID, profile); err != nil {
			return MessageEnvelope{}, err
		}
	}
	eventType := "message.acked"
	if nextState == "unclaimed" {
		eventType = "message.released"
	}
	eventData := map[string]any{
		"message_id": messageID,
		"profile":    profile,
		"state":      nextState,
	}
	if nextState == "unclaimed" && reason != "" {
		eventData["reason"] = reason
	}
	if err := appendMessageEvent(ctx, tx, messageID, profile, eventType, eventData, now); err != nil {
		return MessageEnvelope{}, err
	}
	out, err := queryMessageEnvelope(ctx, tx, messageID, profile)
	if err != nil {
		return MessageEnvelope{}, err
	}
	return out, tx.Commit()
}

func hasActiveSessionClaim(ctx context.Context, q queryer, claimHolder string, excludeMessageID string, botName string, botID string, botAgentID string, now time.Time) (bool, error) {
	var count int
	args := []any{claimHolder, excludeMessageID, busTime(now)}
	where := `WHERE r.claim_holder = ? AND r.message_id != ? AND r.delivery_state = 'claimed' AND r.lease_expires_at IS NOT NULL AND r.lease_expires_at > ?`
	var identityArgs []any
	var identityWhere []string
	if appendMessageBotIdentityWhere(&identityWhere, &identityArgs, botID, botAgentID) {
		where += ` AND ` + identityWhere[0]
		args = append(args, identityArgs...)
	} else if botName != "" {
		where += ` AND m.bot_name = ?`
		args = append(args, botName)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_recipients r JOIN messages m ON m.id = r.message_id AND m.profile = r.profile `+where, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func appendMessageBotIdentityWhere(where *[]string, args *[]any, botID string, botAgentID string) bool {
	botID = strings.TrimSpace(botID)
	botAgentID = strings.TrimSpace(botAgentID)
	var identity []string
	if botID != "" {
		identity = append(identity, "m.bot_id = ?", "r.bot_id = ?")
		*args = append(*args, botID, botID)
	}
	if botAgentID != "" {
		identity = append(identity, "m.bot_agent_id = ?", "r.bot_agent_id = ?")
		*args = append(*args, botAgentID, botAgentID)
	}
	if len(identity) == 0 {
		return false
	}
	*where = append(*where, "("+strings.Join(identity, " OR ")+")")
	return true
}

func queryMessageEnvelopeAnyProfile(ctx context.Context, q queryer, messageID string) (MessageEnvelope, error) {
	return scanMessageEnvelope(q.QueryRowContext(ctx, messageEnvelopeSelectSQL("WHERE m.id = ?"), messageID))
}

func queryMessageEnvelope(ctx context.Context, q queryer, messageID string, profile string) (MessageEnvelope, error) {
	return scanMessageEnvelope(q.QueryRowContext(ctx, messageEnvelopeSelectSQL("WHERE m.id = ? AND r.profile = ?"), messageID, profile))
}

type queryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func messageEnvelopeSelectSQL(where string) string {
	return `SELECT m.id, m.source, m.kind, m.thread_id, m.sender, m.profile, m.bot_name, m.bot_id, m.bot_agent_id, m.body_format, m.body_content, m.refs_json, m.created_at,
		r.delivery_state, r.claim_holder, r.lease_expires_at, r.session_id, r.session_scope_type, r.session_scope_id, r.session_generation, r.read_at
		FROM messages m JOIN message_recipients r ON r.message_id = m.id AND r.profile = m.profile ` + where
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMessageEnvelope(row rowScanner) (MessageEnvelope, error) {
	var threadID sql.NullString
	var claimHolder sql.NullString
	var leaseExpiresAt sql.NullString
	var sessionID sql.NullString
	var sessionScopeType sql.NullString
	var sessionScopeID sql.NullString
	var sessionGeneration sql.NullString
	var readAt sql.NullString
	var refsJSON string
	var envelope MessageEnvelope
	if err := row.Scan(
		&envelope.ID,
		&envelope.Source,
		&envelope.Kind,
		&threadID,
		&envelope.From,
		&envelope.Profile,
		&envelope.BotName,
		&envelope.BotID,
		&envelope.BotAgentID,
		&envelope.Body.Format,
		&envelope.Body.Content,
		&refsJSON,
		&envelope.CreatedAt,
		&envelope.Delivery.State,
		&claimHolder,
		&leaseExpiresAt,
		&sessionID,
		&sessionScopeType,
		&sessionScopeID,
		&sessionGeneration,
		&readAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MessageEnvelope{}, ErrMessageNotFound
		}
		return MessageEnvelope{}, err
	}
	envelope.Version = BusProtocolVersion
	envelope.ThreadID = stringPtrFromNull(threadID)
	envelope.To = []string{"@" + envelope.Profile}
	envelope.Delivery.ClaimHolder = stringPtrFromNull(claimHolder)
	envelope.Delivery.LeaseExpiresAt = stringPtrFromNull(leaseExpiresAt)
	envelope.Delivery.SessionID = stringPtrFromNull(sessionID)
	envelope.Delivery.SessionScope = MessageSessionScope{
		Type: stringPtrFromNull(sessionScopeType),
		ID:   stringPtrFromNull(sessionScopeID),
	}
	envelope.Delivery.SessionGeneration = stringPtrFromNull(sessionGeneration)
	envelope.Delivery.ReadAt = stringPtrFromNull(readAt)
	if err := json.Unmarshal([]byte(refsJSON), &envelope.Refs); err != nil {
		return MessageEnvelope{}, err
	}
	if envelope.Refs == nil {
		envelope.Refs = map[string]any{}
	}
	return envelope, nil
}

func scanMessageEnvelopes(rows *sql.Rows) ([]MessageEnvelope, error) {
	var out []MessageEnvelope
	for rows.Next() {
		envelope, err := scanMessageEnvelope(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, envelope)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []MessageEnvelope{}
	}
	return out, nil
}

func appendMessageEvent(ctx context.Context, q queryer, messageID string, profile string, eventType string, redacted map[string]any, now time.Time) error {
	eventID, err := GenerateLocalID("evt", 0)
	if err != nil {
		return err
	}
	data, err := json.Marshal(redacted)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO events (id, message_id, profile, event_type, redacted_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, eventID, messageID, profile, eventType, string(data), busTime(now))
	return err
}

func outboxMessagesMatchInput(ctx context.Context, q queryer, messageIDs []string, input LocalMessageSend) (bool, error) {
	if len(messageIDs) != len(input.Recipients) || len(messageIDs) == 0 {
		return false, nil
	}
	expectedRecipients := make([]string, 0, len(input.Recipients))
	for _, recipient := range input.Recipients {
		expectedRecipients = append(expectedRecipients, localSendRecipientKey(recipient))
	}
	sort.Strings(expectedRecipients)
	var actualRecipients []string
	for _, messageID := range messageIDs {
		envelope, err := queryMessageEnvelopeAnyProfile(ctx, q, messageID)
		if err != nil {
			return false, err
		}
		if envelope.Body != input.Body || !threadIDsEqual(envelope.ThreadID, input.ThreadID) {
			return false, nil
		}
		if refsEqual, err := refsJSONEqual(envelope.Refs, input.Refs); err != nil || !refsEqual {
			return false, err
		}
		actualRecipients = append(actualRecipients, localSendRecipientKey(LocalMessageRecipient{
			Profile:    envelope.Profile,
			BotName:    envelope.BotName,
			BotID:      envelope.BotID,
			BotAgentID: envelope.BotAgentID,
		}))
	}
	sort.Strings(actualRecipients)
	if len(actualRecipients) != len(expectedRecipients) {
		return false, nil
	}
	for i := range actualRecipients {
		if actualRecipients[i] != expectedRecipients[i] {
			return false, nil
		}
	}
	return true, nil
}

func threadIDsEqual(left *string, right *string) bool {
	return stringValue(left) == stringValue(right)
}

func refsJSONEqual(left map[string]any, right map[string]any) (bool, error) {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return string(leftJSON) == string(rightJSON), nil
}

func localSendFingerprint(input LocalMessageSend) (string, error) {
	type recipientFingerprint struct {
		Key string `json:"key"`
	}
	type fingerprintInput struct {
		SenderKey  string                 `json:"sender_key"`
		Recipients []recipientFingerprint `json:"recipients"`
		Body       MessageBody            `json:"body"`
		Refs       map[string]any         `json:"refs"`
		ThreadID   *string                `json:"thread_id"`
	}
	recipients := make([]recipientFingerprint, 0, len(input.Recipients))
	for _, recipient := range input.Recipients {
		recipients = append(recipients, recipientFingerprint{
			Key: localSendRecipientKey(recipient),
		})
	}
	clone := fingerprintInput{
		SenderKey:  localSendSenderKey(input),
		Recipients: recipients,
		Body:       input.Body,
		Refs:       input.Refs,
		ThreadID:   input.ThreadID,
	}
	sort.Slice(clone.Recipients, func(i, j int) bool {
		return clone.Recipients[i].Key < clone.Recipients[j].Key
	})
	data, err := json.Marshal(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func localSendSenderKey(input LocalMessageSend) string {
	if input.SenderBotID != "" {
		return "bot:" + input.SenderBotID
	}
	if input.SenderBotAgentID != "" {
		return "bot-agent:" + input.SenderBotAgentID
	}
	return "profile:" + input.SenderProfile
}

func localSendRecipientKey(recipient LocalMessageRecipient) string {
	if recipient.BotID != "" {
		return "bot:" + recipient.BotID
	}
	if recipient.BotAgentID != "" {
		return "bot-agent:" + recipient.BotAgentID
	}
	return "profile:" + recipient.Profile
}

func recipientFingerprintKeys(recipients []LocalMessageRecipient) []string {
	out := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		out = append(out, localSendRecipientKey(recipient))
	}
	sort.Strings(out)
	return out
}

func sameStableBotIdentity(leftBotID string, leftBotAgentID string, rightBotID string, rightBotAgentID string) bool {
	if leftBotID != "" && rightBotID != "" && leftBotID == rightBotID {
		return true
	}
	if leftBotAgentID != "" && rightBotAgentID != "" && leftBotAgentID == rightBotAgentID {
		return true
	}
	return false
}

func stableBotIdentityConflict(leftBotID string, leftBotAgentID string, rightBotID string, rightBotAgentID string) bool {
	leftHasStable := leftBotID != "" || leftBotAgentID != ""
	rightHasStable := rightBotID != "" || rightBotAgentID != ""
	return leftHasStable && rightHasStable && !sameStableBotIdentity(leftBotID, leftBotAgentID, rightBotID, rightBotAgentID)
}

func safeOptionalRegistryIdentity(value string) bool {
	return value == "" || isSafeRegistryIdentity(value)
}

func (filter *MessageListFilter) normalize() {
	if filter.Limit <= 0 || filter.Limit > maxMessageListLimit {
		filter.Limit = 50
	}
}

func nullableString(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

func stringPtrFromNull(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func leaseExpired(expiresAt *string, now time.Time) bool {
	if expiresAt == nil {
		return true
	}
	parsed, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		return true
	}
	return !parsed.After(now.UTC())
}

func busTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func recipientProfiles(recipients []LocalMessageRecipient) []string {
	out := make([]string, 0, len(recipients))
	for _, recipient := range recipients {
		out = append(out, recipient.Profile)
	}
	return out
}

func messageIDs(messages []MessageEnvelope) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.ID)
	}
	return out
}

func senderVisibleEnvelope(envelope MessageEnvelope) MessageEnvelope {
	envelope.Delivery = MessageDelivery{State: "sent"}
	return envelope
}
