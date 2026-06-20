package commentbus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const maxPrivateCloudMetadataBytes = 64 * 1024

var (
	cloudNotificationIDRE = regexp.MustCompile(`^ntf_[A-Za-z0-9_-]{1,252}$`)
	cloudClaimIDRE        = regexp.MustCompile(`^clm_[A-Za-z0-9_-]{1,252}$`)
	cloudBotletsRunIDRE   = regexp.MustCompile(`^blr_[a-f0-9]{32}$`)
)

type NotificationClient interface {
	LeaseNotification(ctx context.Context, profile AgentProfile, leaseTTL time.Duration, leaseHolder string, idempotencyKey string, kinds ...string) (*CloudNotificationLease, error)
	RenewNotification(ctx context.Context, profile AgentProfile, claimID string, leaseTTL time.Duration, idempotencyKey string) (*CloudNotificationLease, error)
	AckNotification(ctx context.Context, profile AgentProfile, claimID string, idempotencyKey string) (*CloudNotificationClaimMutation, error)
	ReleaseNotification(ctx context.Context, profile AgentProfile, claimID string, idempotencyKey string) (*CloudNotificationClaimMutation, error)
	PublishNotificationHandlingActivity(ctx context.Context, profile AgentProfile, claimID string, request CloudNotificationHandlingRequest, idempotencyKey string) (*CloudNotificationHandlingResult, error)
}

type NotificationWakeClient interface {
	WaitNotificationWake(ctx context.Context, profile AgentProfile) (*CloudNotificationWake, error)
}

type CloudNotificationWake struct {
	Type                 string `json:"type"`
	WakeID               string `json:"wake_id"`
	UnreadCount          int    `json:"unread_count"`
	NewestNotificationID string `json:"newest_notification_id,omitempty"`
}

type CloudNotificationLease struct {
	ClaimID        string            `json:"claim_id"`
	NotificationID string            `json:"notification_id"`
	ClaimedAt      string            `json:"claimed_at"`
	LeaseExpiresAt string            `json:"lease_expires_at"`
	Notification   CloudNotification `json:"notification"`
}

type CloudNotification struct {
	ID           string                        `json:"id"`
	Type         string                        `json:"type"`
	DocSlug      string                        `json:"doc_slug"`
	DocTitle     string                        `json:"doc_title"`
	CommentID    *string                       `json:"comment_id"`
	SuggestionID *string                       `json:"suggestion_id"`
	FromHandle   string                        `json:"from_handle"`
	FromName     string                        `json:"from_name"`
	Context      string                        `json:"context"`
	CreatedAt    string                        `json:"created_at"`
	AccessToken  string                        `json:"access_token,omitempty"`
	BotletsTask  *CloudBotletsTaskNotification `json:"botlets_task,omitempty"`
}

type CloudBotletsTaskNotification struct {
	RunID               string `json:"run_id"`
	Kind                string `json:"kind"`
	OwnerAgentID        string `json:"owner_agent_id"`
	BotID               string `json:"bot_id,omitempty"`
	BotAgentID          string `json:"bot_agent_id"`
	BotSlug             string `json:"bot_slug"`
	BotName             string `json:"bot_name"`
	BotHandle           string `json:"bot_handle"`
	ScheduledFor        string `json:"scheduled_for"`
	EnqueuedAt          string `json:"enqueued_at"`
	ScheduleVersion     int    `json:"schedule_version"`
	ExecutionGeneration int    `json:"execution_generation"`
	SetupGeneration     int    `json:"setup_generation"`
	Cron                string `json:"cron"`
	Timezone            string `json:"timezone"`
}

type CloudNotificationClaimMutation struct {
	OK             bool   `json:"ok"`
	ClaimID        string `json:"claim_id"`
	NotificationID string `json:"notification_id"`
	Idempotent     bool   `json:"idempotent,omitempty"`
}

type CloudNotificationHandlingRequest struct {
	Action          string `json:"action"`
	TTLMS           int64  `json:"ttl_ms,omitempty"`
	Outcome         string `json:"outcome,omitempty"`
	ClaimGeneration string `json:"claim_generation,omitempty"`
	ProgressAt      string `json:"progress_at,omitempty"`
}

type CloudNotificationHandlingResult struct {
	OK              bool           `json:"ok"`
	Activity        map[string]any `json:"activity"`
	Ignored         bool           `json:"ignored,omitempty"`
	TerminalOutcome string         `json:"terminal_outcome,omitempty"`
	Idempotent      bool           `json:"idempotent,omitempty"`
}

type PrivateCloudMessageMetadata struct {
	LocalMessageID string `json:"local_message_id"`
	Source         string `json:"source"`
	Profile        string `json:"profile"`
	BaseURL        string `json:"base_url"`
	NotificationID string `json:"notification_id"`
	ClaimID        string `json:"claim_id"`
	ClaimedAt      string `json:"claimed_at"`
	LeaseExpiresAt string `json:"lease_expires_at"`
	AccessToken    string `json:"access_token,omitempty"`
}

type privateNotificationIndex struct {
	LocalMessageID string `json:"local_message_id"`
}

func NormalizeNotificationKind(value string) (string, bool) {
	switch value {
	case "mention", "reply", "comment", "suggestion":
		return "doc.mention", true
	case "review_requested":
		return "doc.review_requested", true
	case "botlets_task":
		return "botlets.task", true
	default:
		return "", false
	}
}

func NormalizeNotificationRefs(notification CloudNotification) map[string]any {
	if notification.Type == "botlets_task" && notification.BotletsTask != nil {
		task := notification.BotletsTask
		refs := map[string]any{
			"notification_type":    notification.Type,
			"task_id":              task.RunID,
			"run_id":               task.RunID,
			"task_kind":            task.Kind,
			"owner_agent_id":       task.OwnerAgentID,
			"bot_agent_id":         task.BotAgentID,
			"bot_slug":             task.BotSlug,
			"bot_name":             task.BotName,
			"bot_handle":           task.BotHandle,
			"scheduled_for":        task.ScheduledFor,
			"enqueued_at":          task.EnqueuedAt,
			"schedule_version":     task.ScheduleVersion,
			"execution_generation": task.ExecutionGeneration,
			"setup_generation":     task.SetupGeneration,
			"cron":                 task.Cron,
			"timezone":             task.Timezone,
		}
		if task.BotID != "" {
			refs["bot_id"] = task.BotID
		}
		return refs
	}
	refs := map[string]any{
		"doc_slug":          notification.DocSlug,
		"doc_title":         notification.DocTitle,
		"notification_type": notification.Type,
	}
	if notification.CommentID != nil {
		refs["comment_id"] = *notification.CommentID
	}
	if notification.SuggestionID != nil {
		refs["suggestion_id"] = *notification.SuggestionID
	}
	if notification.FromName != "" {
		refs["from_name"] = notification.FromName
	}
	return refs
}

func validateCloudBotletsTaskNotification(task *CloudBotletsTaskNotification) bool {
	if task == nil {
		return false
	}
	if !cloudBotletsRunIDRE.MatchString(task.RunID) {
		return false
	}
	if task.Kind != "scheduled" && task.Kind != "manual" {
		return false
	}
	stringFields := []string{
		task.OwnerAgentID,
		task.BotAgentID,
		task.BotSlug,
		task.BotName,
		task.BotHandle,
		task.ScheduledFor,
		task.EnqueuedAt,
		task.Cron,
		task.Timezone,
	}
	for _, field := range stringFields {
		if strings.TrimSpace(field) == "" || strings.ContainsAny(field, "\r\n\x00") {
			return false
		}
	}
	if task.ScheduleVersion < 1 || task.ExecutionGeneration < 1 || task.SetupGeneration < 1 {
		return false
	}
	if task.BotID != "" && !isSafeRegistryIdentity(task.BotID) {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, task.ScheduledFor); err != nil {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, task.EnqueuedAt); err != nil {
		return false
	}
	return true
}

func validateCloudBotletsTaskTarget(task *CloudBotletsTaskNotification, bot BotRegistryEntry, profile AgentProfile) bool {
	if task == nil || bot.BrainRef == nil {
		return false
	}
	if profile.Handle == "" || bot.Handle != profile.Handle || !bot.MatchesProfile(task.BotHandle) {
		return false
	}
	if task.BotID != "" && bot.BotID != "" && task.BotID != bot.BotID {
		return false
	}
	if !bot.MatchesSlug(task.BotSlug) {
		return false
	}
	if bot.BrainRef.OwnerAgentID == "" || task.OwnerAgentID != bot.BrainRef.OwnerAgentID {
		return false
	}
	if bot.BrainRef.BotAgentID == "" || task.BotAgentID != bot.BrainRef.BotAgentID {
		return false
	}
	if bot.BrainRef.SetupGeneration < 1 || task.SetupGeneration != bot.BrainRef.SetupGeneration {
		return false
	}
	return true
}

func NotificationMessageBody(notification CloudNotification) MessageBody {
	return MessageBody{Format: "markdown", Content: notification.Context}
}

func WritePrivateCloudMessageMetadata(paths Paths, metadata PrivateCloudMessageMetadata) error {
	if err := validatePrivateCloudMessageMetadata(metadata); err != nil {
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxPrivateCloudMetadataBytes {
		return errors.New("private cloud metadata is too large")
	}
	if err := WritePrivateFileAtomic(privateCloudMessagePath(paths, metadata.Profile, metadata.LocalMessageID), append(data, '\n'), 0o600); err != nil {
		return err
	}
	index := privateNotificationIndex{LocalMessageID: metadata.LocalMessageID}
	indexData, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := WritePrivateFileAtomic(privateCloudNotificationIndexPath(paths, metadata.Profile, metadata.NotificationID), append(indexData, '\n'), 0o600); err != nil {
		return err
	}
	return nil
}

func ReadPrivateCloudMessageMetadata(paths Paths, profile string, messageID string) (PrivateCloudMessageMetadata, error) {
	if !ProfileRE.MatchString(profile) || !LocalMessageIDRE.MatchString(messageID) {
		return PrivateCloudMessageMetadata{}, errors.New("invalid private cloud metadata selector")
	}
	data, err := readPrivateCloudMetadataFile(paths, privateCloudMessagePath(paths, profile, messageID), "cloud message metadata")
	if err != nil {
		return PrivateCloudMessageMetadata{}, err
	}
	var metadata PrivateCloudMessageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return PrivateCloudMessageMetadata{}, err
	}
	if err := validatePrivateCloudMessageMetadata(metadata); err != nil {
		return PrivateCloudMessageMetadata{}, err
	}
	if metadata.Profile != profile || metadata.LocalMessageID != messageID {
		return PrivateCloudMessageMetadata{}, errors.New("private cloud metadata mismatch")
	}
	return metadata, nil
}

func FindPrivateCloudMessageByNotificationID(paths Paths, profile string, notificationID string) (string, bool, error) {
	if !ProfileRE.MatchString(profile) || !isSafeCloudID("notification", notificationID) {
		return "", false, errors.New("invalid private cloud metadata selector")
	}
	data, err := readPrivateCloudMetadataFile(paths, privateCloudNotificationIndexPath(paths, profile, notificationID), "cloud notification index")
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var index privateNotificationIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return "", false, err
	}
	if !LocalMessageIDRE.MatchString(index.LocalMessageID) {
		return "", false, errors.New("invalid private cloud metadata index")
	}
	return index.LocalMessageID, true, nil
}

func findPrivateCloudMessageByNotificationIDForBot(paths Paths, profile string, bot BotRegistryEntry, notificationID string) (string, string, bool, error) {
	profiles := make([]string, 0, 2+len(bot.HandleAliases))
	seen := map[string]struct{}{}
	addProfile := func(candidate string) {
		if candidate == "" {
			return
		}
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		profiles = append(profiles, candidate)
	}
	addProfile(profile)
	addProfile(bot.Handle)
	for _, alias := range bot.HandleAliases {
		addProfile(alias)
	}
	for _, candidate := range profiles {
		messageID, ok, err := FindPrivateCloudMessageByNotificationID(paths, candidate, notificationID)
		if err != nil {
			return "", "", false, err
		}
		if ok {
			return candidate, messageID, true, nil
		}
	}
	return "", "", false, nil
}

func readPrivateCloudMetadataFile(paths Paths, path string, label string) ([]byte, error) {
	file, err := OpenPrivateFile(paths.Home, path, label)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPrivateCloudMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPrivateCloudMetadataBytes {
		return nil, errors.New("private cloud metadata is too large")
	}
	return data, nil
}

func validatePrivateCloudMessageMetadata(metadata PrivateCloudMessageMetadata) error {
	if !LocalMessageIDRE.MatchString(metadata.LocalMessageID) || metadata.Source != "comment.io" || !ProfileRE.MatchString(metadata.Profile) {
		return errors.New("invalid private cloud metadata")
	}
	if !isSafeCloudID("notification", metadata.NotificationID) || !isSafeCloudID("claim", metadata.ClaimID) {
		return errors.New("invalid private cloud metadata")
	}
	if strings.TrimSpace(metadata.BaseURL) == "" || len(metadata.BaseURL) > 2048 || strings.ContainsAny(metadata.BaseURL, "\r\n\x00") {
		return errors.New("invalid private cloud metadata")
	}
	if metadata.ClaimedAt == "" || metadata.LeaseExpiresAt == "" {
		return errors.New("invalid private cloud metadata")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.ClaimedAt); err != nil {
		return errors.New("invalid private cloud metadata")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.LeaseExpiresAt); err != nil {
		return errors.New("invalid private cloud metadata")
	}
	if len(metadata.AccessToken) > 8192 || strings.ContainsAny(metadata.AccessToken, "\r\n\x00") {
		return errors.New("invalid private cloud metadata")
	}
	return nil
}

func isSafeCloudID(kind string, value string) bool {
	switch kind {
	case "notification":
		return cloudNotificationIDRE.MatchString(value)
	case "claim":
		return cloudClaimIDRE.MatchString(value)
	default:
		return false
	}
}

func privateCloudMessagePath(paths Paths, profile string, messageID string) string {
	return filepath.Join(paths.Private, profile, messageID+".json")
}

func privateCloudNotificationIndexPath(paths Paths, profile string, notificationID string) string {
	return filepath.Join(paths.Private, profile, "notifications", privateCloudIDHash(notificationID)+".json")
}

func privateCloudIDHash(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func CloudMessageFromLease(messageID string, profile string, bot BotRegistryEntry, profileConfig AgentProfile, lease CloudNotificationLease, now time.Time) (CloudNotificationMessage, PrivateCloudMessageMetadata, error) {
	if !LocalMessageIDRE.MatchString(messageID) || !ProfileRE.MatchString(profile) || bot.Handle != profile || profileConfig.Handle != profile {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification target")
	}
	notificationID := lease.NotificationID
	if notificationID == "" {
		notificationID = lease.Notification.ID
	}
	notification := lease.Notification
	if notificationID != "" && notification.ID != "" && notificationID != notification.ID {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("cloud notification id mismatch")
	}
	if notification.ID == "" {
		notification.ID = notificationID
	}
	if !isSafeCloudID("notification", notificationID) || !isSafeCloudID("claim", lease.ClaimID) {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification lease")
	}
	kind, ok := NormalizeNotificationKind(notification.Type)
	if !ok {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification type")
	}
	if notification.Type == "botlets_task" {
		if !validateCloudBotletsTaskNotification(notification.BotletsTask) {
			return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid botlets task notification")
		}
		if !validateCloudBotletsTaskTarget(notification.BotletsTask, bot, profileConfig) {
			return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("botlets task target mismatch")
		}
	}
	from := "@" + strings.TrimPrefix(notification.FromHandle, "@")
	if from == "@" {
		from = "@comment.io"
	}
	if len(from) > 256 || strings.ContainsAny(from, "\r\n\x00") || containsSecretValue(from) {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification sender")
	}
	createdAtRaw := notification.CreatedAt
	if createdAtRaw == "" {
		createdAtRaw = lease.ClaimedAt
	}
	if createdAtRaw == "" {
		createdAtRaw = busTime(now)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdAtRaw)
	if err != nil {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification timestamp")
	}
	claimedAtRaw := lease.ClaimedAt
	if claimedAtRaw == "" {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification timestamp")
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, claimedAtRaw)
	if err != nil {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification timestamp")
	}
	if lease.LeaseExpiresAt == "" {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification lease timestamp")
	}
	leaseExpiresAt, err := time.Parse(time.RFC3339Nano, lease.LeaseExpiresAt)
	if err != nil {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification lease timestamp")
	}
	if !leaseExpiresAt.After(now) {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("expired cloud notification lease")
	}
	body := NotificationMessageBody(notification)
	refs := NormalizeNotificationRefs(notification)
	if body.Format != "markdown" || len(body.Content) > 1_000_000 || strings.ContainsRune(body.Content, '\x00') || containsSecretValue(body.Content) || !isSafeRefs(refs) || notificationVisibleFieldsContainAccessToken(notification.AccessToken, from, body, refs) {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, errors.New("invalid cloud notification content")
	}
	message := CloudNotificationMessage{
		ID:             messageID,
		Profile:        profile,
		BotName:        bot.Name,
		BotID:          bot.BotID,
		BotAgentID:     botAgentID(bot),
		Kind:           kind,
		From:           from,
		Body:           body,
		Refs:           refs,
		NotificationID: notificationID,
		CreatedAt:      busTime(createdAt),
		LeaseExpiresAt: busTime(leaseExpiresAt),
		Now:            now,
	}
	metadata := PrivateCloudMessageMetadata{
		LocalMessageID: messageID,
		Source:         "comment.io",
		Profile:        profile,
		BaseURL:        profileConfig.BaseURL,
		NotificationID: notificationID,
		ClaimID:        lease.ClaimID,
		ClaimedAt:      busTime(claimedAt),
		LeaseExpiresAt: busTime(leaseExpiresAt),
		AccessToken:    notification.AccessToken,
	}
	if err := validatePrivateCloudMessageMetadata(metadata); err != nil {
		return CloudNotificationMessage{}, PrivateCloudMessageMetadata{}, fmt.Errorf("invalid cloud notification metadata: %w", err)
	}
	return message, metadata, nil
}

func notificationVisibleFieldsContainAccessToken(accessToken string, from string, body MessageBody, refs map[string]any) bool {
	if accessToken == "" {
		return false
	}
	if strings.Contains(from, accessToken) {
		return true
	}
	if strings.Contains(body.Content, accessToken) {
		return true
	}
	for _, value := range refs {
		if text, ok := value.(string); ok && strings.Contains(text, accessToken) {
			return true
		}
	}
	return false
}
