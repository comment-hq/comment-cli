package commentbus

import (
	"testing"
	"time"
)

func testNotificationOperationPaths(t *testing.T) Paths {
	t.Helper()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestBeginCloudNotificationClaimOperationRejectsReservedReleaseReason(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	messageID := "msg_reservedReasonCheck1"
	claimID := "clm_reservedReasonCheck1"
	notificationID := "ntf_reservedReasonCheck1"
	opID := "op_reservedReasonCheck12"

	if _, _, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), declinedDuplicateReleaseReason); err == nil {
		t.Fatal("public release op accepted the duplicate-decline sentinel reason")
	}
	op, done, err := BeginDeclinedDuplicateCloudNotificationReleaseOperation(paths, "max.reviewer", messageID, claimID, notificationID, time.Now().UTC())
	if err != nil || done || op.ReleaseReason != declinedDuplicateReleaseReason {
		t.Fatalf("internal duplicate-decline release op = %+v done=%v err=%v", op, done, err)
	}
}

func TestBeginCloudNotificationClaimOperationWildcardLookupSkipsReservedReleaseReason(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	messageID := "msg_reservedLookupCheck1"
	claimID := "clm_reservedLookupCheck1"
	notificationID := "ntf_reservedLookupCheck1"

	reserved, done, err := BeginDeclinedDuplicateCloudNotificationReleaseOperation(paths, "max.reviewer", messageID, claimID, notificationID, time.Now().UTC())
	if err != nil || done {
		t.Fatalf("internal duplicate-decline release op = %+v done=%v err=%v", reserved, done, err)
	}
	lookup, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, "", 0, time.Now().UTC())
	if err != nil || done {
		t.Fatalf("public release lookup = %+v done=%v err=%v", lookup, done, err)
	}
	if lookup.OpID == reserved.OpID || lookup.ReleaseReason == declinedDuplicateReleaseReason {
		t.Fatalf("public wildcard lookup reused reserved operation: reserved=%+v lookup=%+v", reserved, lookup)
	}
}

func TestHasPendingTerminalCloudNotificationClaimOperationIgnoresDeclinedDuplicateRelease(t *testing.T) {
	ops := []CloudNotificationClaimOperation{{
		Operation:     "release",
		ReleaseReason: declinedDuplicateReleaseReason,
	}}
	if HasPendingTerminalCloudNotificationClaimOperation(ops) {
		t.Fatal("declined duplicate release counted as a terminal local operation")
	}
	ops = append(ops, CloudNotificationClaimOperation{Operation: "release", ReleaseReason: "user_release"})
	if !HasPendingTerminalCloudNotificationClaimOperation(ops) {
		t.Fatal("ordinary release was not counted as a terminal local operation")
	}
}

func TestBeginCloudNotificationClaimOperationRejectsExplicitReleaseReasonMismatch(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	messageID := "msg_releaseReasonCheck123"
	claimID := "clm_releaseReasonCheck123"
	notificationID := "ntf_releaseReasonCheck123"
	opID := "op_releaseReasonCheck1234"

	op, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), "original reason")
	if err != nil || done {
		t.Fatalf("begin release op = %+v done=%v err=%v", op, done, err)
	}
	if _, _, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC()); err == nil {
		t.Fatal("explicit release op-id retry without the journaled reason succeeded")
	}
	if _, _, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), "different reason"); err == nil {
		t.Fatal("explicit release op-id retry with a different reason succeeded")
	}
	retry, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), "original reason")
	if err != nil || done || retry.OpID != opID || retry.ReleaseReason != "original reason" {
		t.Fatalf("matching pending retry = %+v done=%v err=%v", retry, done, err)
	}
	if err := CompleteCloudNotificationClaimOperation(paths, op, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC()); err == nil {
		t.Fatal("explicit completed release op-id retry without the journaled reason succeeded")
	}
	replay, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), "original reason")
	if err != nil || !done || replay.OpID != opID || replay.ReleaseReason != "original reason" {
		t.Fatalf("matching done retry = %+v done=%v err=%v", replay, done, err)
	}
}

func TestBeginCloudNotificationClaimOperationKeepsLegacyReasonLookupWithoutOpID(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	messageID := "msg_releaseReasonLegacy12"
	claimID := "clm_releaseReasonLegacy12"
	notificationID := "ntf_releaseReasonLegacy12"
	opID := "op_releaseReasonLegacy123"

	op, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, opID, 0, time.Now().UTC(), "legacy reason")
	if err != nil || done {
		t.Fatalf("begin release op = %+v done=%v err=%v", op, done, err)
	}
	lookup, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, "", 0, time.Now().UTC())
	if err != nil || done || lookup.OpID != opID || lookup.ReleaseReason != "legacy reason" {
		t.Fatalf("legacy release lookup = %+v done=%v err=%v", lookup, done, err)
	}
}

func TestBeginCloudNotificationClaimOperationNoOpIDFindsMatchingReleaseReason(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	messageID := "msg_releaseReasonMulti123"
	claimID := "clm_releaseReasonMulti123"
	notificationID := "ntf_releaseReasonMulti123"
	firstOpID := "op_releaseReasonMulti0001"
	secondOpID := "op_releaseReasonMulti0002"

	first, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, firstOpID, 0, time.Now().UTC(), "first reason")
	if err != nil || done {
		t.Fatalf("begin first release op = %+v done=%v err=%v", first, done, err)
	}
	second, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, secondOpID, 0, time.Now().UTC(), "second reason")
	if err != nil || done {
		t.Fatalf("begin second release op = %+v done=%v err=%v", second, done, err)
	}
	lookup, done, err := BeginCloudNotificationClaimOperation(paths, "release", "max.reviewer", messageID, claimID, notificationID, "", 0, time.Now().UTC(), "second reason")
	if err != nil || done || lookup.OpID != secondOpID || lookup.ReleaseReason != "second reason" {
		t.Fatalf("matching release reason lookup = %+v done=%v err=%v", lookup, done, err)
	}
}

func TestBeginCloudNotificationWaitOperationRetiresPendingWhenDoneArchiveExists(t *testing.T) {
	paths := testNotificationOperationPaths(t)
	now := time.Now().UTC()
	op, err := BeginCloudNotificationWaitOperation(paths, "max.reviewer", time.Second, time.Minute, "owner:max.reviewer", now)
	if err != nil {
		t.Fatal(err)
	}
	op, err = RecordCloudNotificationWaitOperationAttempt(paths, op, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompleteCloudNotificationWaitOperation(paths, op, now); err != nil {
		t.Fatal(err)
	}
	if err := writePendingCloudNotificationWaitOperation(paths, op, false); err != nil {
		t.Fatal(err)
	}
	next, err := BeginCloudNotificationWaitOperation(paths, "max.reviewer", time.Second, time.Minute, "owner:max.reviewer", now)
	if err != nil {
		t.Fatal(err)
	}
	if next.OpID == op.OpID {
		t.Fatalf("reused completed wait op: %+v", next)
	}
	pending, ok, err := ReadPendingCloudNotificationWaitOperation(paths, "max.reviewer")
	if err != nil || !ok || pending.OpID != next.OpID {
		t.Fatalf("pending wait after done cleanup = %+v ok=%v err=%v", pending, ok, err)
	}
	done, ok, err := ReadDoneCloudNotificationWaitOperation(paths, op.OpID)
	if err != nil || !ok || done.OpID != op.OpID {
		t.Fatalf("done wait archive = %+v ok=%v err=%v", done, ok, err)
	}
}
