package commentbus

import (
	"context"
	"testing"
	"time"
)

// TestRefreshCloudNotificationLeaseCapsRedelivery is the regression test for
// bug #301: when a cloud notification's ack never lands server-side, comt.dev
// keeps re-delivering it, the local lease keeps expiring, and the bot
// reprocesses the same message forever. After cloudRedeliveryCap reopens the
// daemon must stop the cycle: terminally quarantine the message (so the
// claim/ready paths, which require 'unclaimed', never surface it again) and
// signal the caller so it can warn-log the stuck loop.
func TestRefreshCloudNotificationLeaseCapsRedelivery(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	orig := cloudRedeliveryCap
	cloudRedeliveryCap = 3
	t.Cleanup(func() { cloudRedeliveryCap = orig })

	const (
		profile = "max.bar"
		msgID   = "msg_redeliveryloop0000001"
	)
	now := time.Now().UTC()
	lease := now.Add(time.Minute).Format(time.RFC3339Nano)
	if _, err := store.InsertCloudNotificationMessage(ctx, CloudNotificationMessage{
		ID:             msgID,
		Profile:        profile,
		BotName:        "bar",
		Kind:           "doc.mention",
		From:           "someone",
		Body:           MessageBody{Format: "markdown", Content: "ping"},
		LeaseExpiresAt: lease,
		Now:            now,
	}); err != nil {
		t.Fatalf("insert cloud notification: %v", err)
	}

	// Simulate one turn of the loop: claim, let the lease expire, then the cloud
	// re-delivers (RefreshCloudNotificationLease). Returns whether the refresh
	// quarantined the message.
	reopen := func(i int) bool {
		if _, err := store.ClaimMessage(ctx, MessageClaimOptions{
			Profile:     profile,
			MessageID:   msgID,
			ClaimHolder: "owner:" + profile,
			LeaseTTL:    time.Minute,
			Now:         now,
		}); err != nil {
			t.Fatalf("claim (turn %d): %v", i, err)
		}
		if _, err := store.db.Exec(
			`UPDATE message_recipients SET lease_expires_at = ? WHERE message_id = ? AND profile = ?`,
			busTime(now.Add(-time.Minute)), msgID, profile,
		); err != nil {
			t.Fatalf("expire lease (turn %d): %v", i, err)
		}
		quarantined, err := store.RefreshCloudNotificationLease(ctx, profile, msgID, lease, now, false)
		if err != nil {
			t.Fatalf("refresh (turn %d): %v", i, err)
		}
		return quarantined
	}

	// The first `cap` reopens proceed normally (each logs a requeue).
	for i := 0; i < cloudRedeliveryCap; i++ {
		if reopen(i) {
			t.Fatalf("turn %d quarantined before the cap was reached", i)
		}
	}
	// The next reopen exceeds the cap → quarantine.
	if !reopen(cloudRedeliveryCap) {
		t.Fatalf("expected quarantine after %d redeliveries", cloudRedeliveryCap)
	}

	// The message is terminally stopped: 'acked', so the claim path rejects it.
	env, err := store.GetInboxMessage(ctx, profile, msgID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if env.Delivery.State != "acked" {
		t.Fatalf("expected terminal 'acked' after quarantine, got %q", env.Delivery.State)
	}
	if _, err := store.ClaimMessage(ctx, MessageClaimOptions{
		Profile: profile, MessageID: msgID, ClaimHolder: "owner:" + profile, LeaseTTL: time.Minute, Now: now,
	}); err == nil {
		t.Fatalf("quarantined message must not be claimable again")
	}

	// A structured redelivery warning was recorded in the events journal.
	var warnings int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE message_id = ? AND profile = ? AND event_type = 'message.redelivery_warning'`,
		msgID, profile,
	).Scan(&warnings); err != nil {
		t.Fatalf("count warnings: %v", err)
	}
	if warnings != 1 {
		t.Fatalf("expected exactly one message.redelivery_warning event, got %d", warnings)
	}
}

// TestRefreshCloudNotificationLeaseCapsReleasedRedelivery covers bug #165: a
// released cloud notification that the server keeps returning as unread is
// re-offered (released -> reopened) ahead of newer mentions. It must run through
// the same redelivery guard as the claimed loop, so a stale released
// notification is bounded and surfaced instead of blocking fresh mentions
// forever.
func TestRefreshCloudNotificationLeaseCapsReleasedRedelivery(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	orig := cloudRedeliveryCap
	cloudRedeliveryCap = 3
	t.Cleanup(func() { cloudRedeliveryCap = orig })

	const (
		profile = "max.bar"
		msgID   = "msg_releasedredeliveryloop1"
	)
	now := time.Now().UTC()
	lease := now.Add(time.Minute).Format(time.RFC3339Nano)
	if _, err := store.InsertCloudNotificationMessage(ctx, CloudNotificationMessage{
		ID:             msgID,
		Profile:        profile,
		BotName:        "bar",
		Kind:           "doc.mention",
		From:           "someone",
		Body:           MessageBody{Format: "markdown", Content: "ping"},
		LeaseExpiresAt: lease,
		Now:            now,
	}); err != nil {
		t.Fatalf("insert cloud notification: %v", err)
	}

	// Each turn: the bot has released the notification, the server re-offers it,
	// and the refresh reopens it. Returns whether the refresh quarantined it.
	redeliverReleased := func(i int) bool {
		if _, err := store.db.Exec(
			`UPDATE message_recipients SET delivery_state = 'released', claim_holder = NULL, lease_expires_at = NULL WHERE message_id = ? AND profile = ?`,
			msgID, profile,
		); err != nil {
			t.Fatalf("release (turn %d): %v", i, err)
		}
		quarantined, err := store.RefreshCloudNotificationLease(ctx, profile, msgID, lease, now, false)
		if err != nil {
			t.Fatalf("refresh (turn %d): %v", i, err)
		}
		return quarantined
	}

	for i := 0; i < cloudRedeliveryCap; i++ {
		if redeliverReleased(i) {
			t.Fatalf("released turn %d quarantined before the cap", i)
		}
	}
	if !redeliverReleased(cloudRedeliveryCap) {
		t.Fatalf("expected a stale released notification to be quarantined after %d redeliveries", cloudRedeliveryCap)
	}

	env, err := store.GetInboxMessage(ctx, profile, msgID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if env.Delivery.State != "acked" {
		t.Fatalf("expected terminal 'acked' after quarantine, got %q", env.Delivery.State)
	}
}
