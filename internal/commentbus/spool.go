package commentbus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const messageSpoolVersion = 1

type MessageSpoolEntry struct {
	Version        int             `json:"version"`
	Host           string          `json:"host,omitempty"`
	MessageID      string          `json:"message_id"`
	Profile        string          `json:"profile"`
	BotName        string          `json:"bot_name"`
	Source         string          `json:"source"`
	Kind           string          `json:"kind"`
	DeliveryState  string          `json:"delivery_state"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
	LeaseExpiresAt *string         `json:"lease_expires_at,omitempty"`
	SessionID      *string         `json:"session_id,omitempty"`
	Generation     *string         `json:"generation,omitempty"`
	PaneTarget     *string         `json:"pane_target,omitempty"`
	LastNudge      LastNudgeRecord `json:"last_nudge"`
}

func WriteMessageSpool(paths Paths, message MessageEnvelope) error {
	nowTime := time.Now().UTC()
	if !shouldWriteMessageSpool(message, nowTime) {
		return nil
	}
	now := busTime(nowTime)
	entry := MessageSpoolEntry{
		Version:        messageSpoolVersion,
		MessageID:      message.ID,
		Profile:        message.Profile,
		BotName:        message.BotName,
		Source:         message.Source,
		Kind:           message.Kind,
		DeliveryState:  message.Delivery.State,
		CreatedAt:      message.CreatedAt,
		UpdatedAt:      now,
		LeaseExpiresAt: message.Delivery.LeaseExpiresAt,
	}
	if existing, ok, err := ReadMessageSpool(paths, message.Profile, message.ID); err != nil {
		return err
	} else if ok {
		entry.SessionID = existing.SessionID
		entry.Generation = existing.Generation
		entry.Host = normalizeSessionHost(existing.Host)
		entry.PaneTarget = existing.PaneTarget
		entry.LastNudge = existing.LastNudge
	}
	return writeMessageSpoolEntry(paths, entry)
}

func shouldWriteMessageSpool(message MessageEnvelope, now time.Time) bool {
	if message.Source == "comment.io" {
		return message.Delivery.State == "unclaimed" && message.Delivery.LeaseExpiresAt != nil && !leaseExpired(message.Delivery.LeaseExpiresAt, now)
	}
	switch message.Delivery.State {
	case "unclaimed":
		return true
	case "claimed":
		return leaseExpired(message.Delivery.LeaseExpiresAt, now)
	default:
		return false
	}
}

func UpdateMessageSpoolNudge(paths Paths, record SessionRecord, message MessageEnvelope) error {
	if !LocalMessageIDRE.MatchString(message.ID) ||
		!ProfileRE.MatchString(record.Profile) ||
		!isBotName(record.BotName) ||
		message.Profile != record.Profile ||
		message.BotName != record.BotName {
		return ErrMessageConflict
	}
	if !shouldWriteMessageSpool(message, time.Now().UTC()) {
		return RemoveMessageSpool(paths, message.Profile, message.ID)
	}
	entry, ok, err := ReadMessageSpool(paths, record.Profile, message.ID)
	if err != nil {
		return err
	}
	if !ok {
		entry = MessageSpoolEntry{
			Version:        messageSpoolVersion,
			MessageID:      message.ID,
			Profile:        message.Profile,
			BotName:        message.BotName,
			Source:         message.Source,
			Kind:           message.Kind,
			DeliveryState:  message.Delivery.State,
			CreatedAt:      message.CreatedAt,
			LeaseExpiresAt: message.Delivery.LeaseExpiresAt,
		}
	}
	now := busTime(time.Now().UTC())
	entry.Version = messageSpoolVersion
	entry.Source = message.Source
	entry.Kind = message.Kind
	entry.DeliveryState = message.Delivery.State
	entry.LeaseExpiresAt = message.Delivery.LeaseExpiresAt
	entry.UpdatedAt = now
	entry.SessionID = &record.SessionID
	entry.Generation = &record.Generation
	entry.Host = normalizeSessionHost(record.Host)
	entry.PaneTarget = &record.PaneTarget
	entry.LastNudge = record.LastNudge
	return writeMessageSpoolEntry(paths, entry)
}

func RemoveMessageSpool(paths Paths, profile string, messageID string) error {
	path, err := messageSpoolPath(paths, profile, messageID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDir(filepath.Dir(path))
}

func ReadMessageSpool(paths Paths, profile string, messageID string) (MessageSpoolEntry, bool, error) {
	path, err := messageSpoolPath(paths, profile, messageID)
	if err != nil {
		return MessageSpoolEntry{}, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MessageSpoolEntry{}, false, nil
		}
		return MessageSpoolEntry{}, false, err
	}
	var entry MessageSpoolEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return MessageSpoolEntry{}, false, err
	}
	entry.Host = normalizeSessionHost(entry.Host)
	if err := validateMessageSpoolEntry(entry); err != nil {
		return MessageSpoolEntry{}, false, err
	}
	return entry, true, nil
}

func ListMessageSpool(paths Paths, profile string, botName string) ([]MessageSpoolEntry, error) {
	if !ProfileRE.MatchString(profile) || (botName != "" && !isBotName(botName)) {
		return nil, ErrMessageConflict
	}
	dir := filepath.Join(paths.Spool, profile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []MessageSpoolEntry{}, nil
		}
		return nil, err
	}
	out := make([]MessageSpoolEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		messageID := name[:len(name)-len(".json")]
		if !LocalMessageIDRE.MatchString(messageID) {
			continue
		}
		spooled, ok, err := ReadMessageSpool(paths, profile, messageID)
		if err != nil {
			return nil, err
		}
		if !ok || (botName != "" && spooled.BotName != botName) {
			continue
		}
		out = append(out, spooled)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].MessageID < out[j].MessageID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out, nil
}

func writeMessageSpoolEntry(paths Paths, entry MessageSpoolEntry) error {
	if err := validateMessageSpoolEntry(entry); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	path, err := messageSpoolPath(paths, entry.Profile, entry.MessageID)
	if err != nil {
		return err
	}
	return WritePrivateFileAtomic(path, append(data, '\n'), 0o600)
}

func messageSpoolPath(paths Paths, profile string, messageID string) (string, error) {
	if !ProfileRE.MatchString(profile) || !LocalMessageIDRE.MatchString(messageID) {
		return "", ErrMessageConflict
	}
	return filepath.Join(paths.Spool, profile, messageID+".json"), nil
}

func validateMessageSpoolEntry(entry MessageSpoolEntry) error {
	entry.Host = normalizeSessionHost(entry.Host)
	if entry.Version != messageSpoolVersion ||
		!LocalMessageIDRE.MatchString(entry.MessageID) ||
		!ProfileRE.MatchString(entry.Profile) ||
		!isBotName(entry.BotName) ||
		!isSessionHost(entry.Host) ||
		(entry.Kind != "" && !isMessageKind(entry.Kind)) ||
		(entry.Source != "" && entry.Source != "local" && entry.Source != "comment.io") {
		return ErrMessageConflict
	}
	switch entry.DeliveryState {
	case "unclaimed", "claimed", "":
	default:
		return ErrMessageConflict
	}
	if entry.SessionID != nil && !LocalSessionIDRE.MatchString(*entry.SessionID) {
		return ErrInvalidSession
	}
	if entry.Generation != nil && !LocalSessionGenerationIDRE.MatchString(*entry.Generation) {
		return ErrInvalidSession
	}
	if entry.PaneTarget != nil {
		if err := validatePaneTargetForHost(entry.Host, *entry.PaneTarget); err != nil {
			return ErrInvalidSession
		}
	}
	if entry.LastNudge.MessageID != nil && *entry.LastNudge.MessageID != entry.MessageID {
		return ErrMessageConflict
	}
	if entry.LeaseExpiresAt != nil {
		if _, err := time.Parse(time.RFC3339Nano, *entry.LeaseExpiresAt); err != nil {
			return ErrMessageConflict
		}
	}
	if entry.LastNudge.PaneTarget != nil {
		if err := validatePaneTargetForHost(entry.Host, *entry.LastNudge.PaneTarget); err != nil {
			return ErrInvalidSession
		}
	}
	return nil
}
