package commentbus

import (
	"testing"
	"time"
)

func TestMessageSpoolWriteUpdateListRemove(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := busTime(time.Now().UTC())
	message := MessageEnvelope{
		ID:        messageID,
		Source:    "local",
		Kind:      "message",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: createdAt,
		Delivery:  MessageDelivery{State: "unclaimed"},
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	entries, err := ListMessageSpool(paths, "max.reviewer", "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].MessageID != messageID || entries[0].DeliveryState != "unclaimed" {
		t.Fatalf("spool entries = %+v", entries)
	}

	sessionID, err := GenerateLocalID("sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := GenerateLocalID("gen", 0)
	if err != nil {
		t.Fatal(err)
	}
	attemptedAt := busTime(time.Now().UTC())
	succeededAt := busTime(time.Now().UTC())
	paneTarget := "%42"
	record := SessionRecord{
		Profile:    "max.reviewer",
		BotName:    "reviewer",
		SessionID:  sessionID,
		Generation: generation,
		PaneTarget: paneTarget,
		LastNudge: LastNudgeRecord{
			MessageID:   &messageID,
			PaneTarget:  &paneTarget,
			AttemptedAt: &attemptedAt,
			SucceededAt: &succeededAt,
		},
	}
	if err := UpdateMessageSpoolNudge(paths, record, message); err != nil {
		t.Fatal(err)
	}
	spooled, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || spooled.SessionID == nil || *spooled.SessionID != sessionID || spooled.LastNudge.SucceededAt == nil {
		t.Fatalf("updated spool = %+v ok=%v", spooled, ok)
	}
	if err := RemoveMessageSpool(paths, "max.reviewer", messageID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID); err != nil || ok {
		t.Fatalf("removed spool ok=%v err=%v", ok, err)
	}
}

func TestMessageSpoolPreservesNudgeMetadataOnRewrite(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	message := MessageEnvelope{
		ID:        messageID,
		Source:    "local",
		Kind:      "message",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: busTime(time.Now().UTC()),
		Delivery:  MessageDelivery{State: "unclaimed"},
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	sessionID, err := GenerateLocalID("sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := GenerateLocalID("gen", 0)
	if err != nil {
		t.Fatal(err)
	}
	attemptedAt := busTime(time.Now().UTC())
	succeededAt := busTime(time.Now().UTC())
	paneTarget := "%42"
	record := SessionRecord{
		Profile:    "max.reviewer",
		BotName:    "reviewer",
		SessionID:  sessionID,
		Generation: generation,
		PaneTarget: paneTarget,
		LastNudge: LastNudgeRecord{
			MessageID:   &messageID,
			PaneTarget:  &paneTarget,
			AttemptedAt: &attemptedAt,
			SucceededAt: &succeededAt,
		},
	}
	if err := UpdateMessageSpoolNudge(paths, record, message); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	spooled, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || spooled.SessionID == nil || *spooled.SessionID != sessionID || spooled.LastNudge.SucceededAt == nil {
		t.Fatalf("rewritten spool lost nudge metadata: %+v ok=%v", spooled, ok)
	}
}

func TestMessageSpoolAcceptsBmuxPaneTargets(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	message := MessageEnvelope{
		ID:        messageID,
		Source:    "local",
		Kind:      "message",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: busTime(time.Now().UTC()),
		Delivery:  MessageDelivery{State: "unclaimed"},
	}
	socketPath, err := BmuxSocketPathForSession(paths, "comment-reviewer-abc123")
	if err != nil {
		t.Fatal(err)
	}
	attemptedAt := busTime(time.Now().UTC())
	succeededAt := busTime(time.Now().UTC())
	record := SessionRecord{
		Host:       SessionHostBmux,
		Profile:    "max.reviewer",
		BotName:    "reviewer",
		SessionID:  "sess_abcdefghijklmnopqrstuvwx",
		Generation: "gen_abcdefghijklmnopqrst",
		PaneTarget: socketPath,
		LastNudge: LastNudgeRecord{
			MessageID:   &messageID,
			PaneTarget:  &socketPath,
			AttemptedAt: &attemptedAt,
			SucceededAt: &succeededAt,
		},
	}
	if err := UpdateMessageSpoolNudge(paths, record, message); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	spooled, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || spooled.Host != SessionHostBmux || spooled.PaneTarget == nil || *spooled.PaneTarget != socketPath || spooled.LastNudge.PaneTarget == nil || *spooled.LastNudge.PaneTarget != socketPath {
		t.Fatalf("bmux spool entry = %+v ok=%v", spooled, ok)
	}
}

func TestMessageSpoolSkipsCloudMessageWithoutActiveLease(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	message := MessageEnvelope{
		ID:        messageID,
		Source:    "comment.io",
		Kind:      "doc.mention",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: busTime(time.Now().UTC()),
		Delivery:  MessageDelivery{State: "unclaimed"},
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID); err != nil || ok {
		t.Fatalf("cloud message without lease spooled ok=%v err=%v", ok, err)
	}
	leaseExpiresAt := busTime(time.Now().UTC().Add(10 * time.Minute))
	message.Delivery.LeaseExpiresAt = &leaseExpiresAt
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID); err != nil || !ok {
		t.Fatalf("cloud message with active lease not spooled ok=%v err=%v", ok, err)
	}
	spooled, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID)
	if err != nil || !ok || spooled.LeaseExpiresAt == nil || *spooled.LeaseExpiresAt != leaseExpiresAt {
		t.Fatalf("cloud spool lease = %+v ok=%v err=%v", spooled, ok, err)
	}
	if err := RemoveMessageSpool(paths, "max.reviewer", messageID); err != nil {
		t.Fatal(err)
	}
	expiredAt := busTime(time.Now().UTC().Add(-time.Minute))
	message.Delivery.LeaseExpiresAt = &expiredAt
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID); err != nil || ok {
		t.Fatalf("cloud message with expired lease spooled ok=%v err=%v", ok, err)
	}

	localID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	localExpiredAt := busTime(time.Now().UTC().Add(-time.Minute))
	localMessage := MessageEnvelope{
		ID:        localID,
		Source:    "local",
		Kind:      "message",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: busTime(time.Now().UTC()),
		Delivery: MessageDelivery{
			State:          "claimed",
			LeaseExpiresAt: &localExpiredAt,
		},
	}
	if err := WriteMessageSpool(paths, localMessage); err != nil {
		t.Fatal(err)
	}
	spooled, ok, err = ReadMessageSpool(paths, "max.reviewer", localID)
	if err != nil || !ok || spooled.DeliveryState != "claimed" {
		t.Fatalf("local expired claim spool = %+v ok=%v err=%v", spooled, ok, err)
	}
}

func TestMessageSpoolNudgeHydratesMissingCloudEntry(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	messageID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := GenerateLocalID("sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := GenerateLocalID("gen", 0)
	if err != nil {
		t.Fatal(err)
	}
	leaseExpiresAt := busTime(time.Now().UTC().Add(10 * time.Minute))
	paneTarget := "%42"
	record := SessionRecord{
		Profile:    "max.reviewer",
		BotName:    "reviewer",
		SessionID:  sessionID,
		Generation: generation,
		PaneTarget: paneTarget,
		LastNudge: LastNudgeRecord{
			MessageID:  &messageID,
			PaneTarget: &paneTarget,
		},
	}
	message := MessageEnvelope{
		ID:        messageID,
		Source:    "comment.io",
		Kind:      "doc.mention",
		Profile:   "max.reviewer",
		BotName:   "reviewer",
		CreatedAt: busTime(time.Now().UTC()),
		Delivery: MessageDelivery{
			State:          "unclaimed",
			LeaseExpiresAt: &leaseExpiresAt,
		},
	}
	if err := writeMessageSpoolEntry(paths, MessageSpoolEntry{
		Version:       messageSpoolVersion,
		MessageID:     messageID,
		Profile:       "max.reviewer",
		BotName:       "reviewer",
		DeliveryState: "unclaimed",
		CreatedAt:     busTime(time.Now().UTC()),
		UpdatedAt:     busTime(time.Now().UTC()),
	}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateMessageSpoolNudge(paths, record, message); err != nil {
		t.Fatal(err)
	}
	spooled, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || spooled.Source != "comment.io" || spooled.LeaseExpiresAt == nil || *spooled.LeaseExpiresAt != leaseExpiresAt {
		t.Fatalf("hydrated cloud spool = %+v ok=%v", spooled, ok)
	}
	expiredAt := busTime(time.Now().UTC().Add(-time.Minute))
	message.Delivery.LeaseExpiresAt = &expiredAt
	if err := UpdateMessageSpoolNudge(paths, record, message); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadMessageSpool(paths, "max.reviewer", messageID); err != nil || ok {
		t.Fatalf("expired cloud nudge left spool ok=%v err=%v", ok, err)
	}
}
