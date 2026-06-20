package commentbus

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeNotificationKind(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		kind string
	}{
		{raw: "mention", kind: "doc.mention"},
		{raw: "reply", kind: "doc.mention"},
		{raw: "comment", kind: "doc.mention"},
		{raw: "suggestion", kind: "doc.mention"},
		{raw: "review_requested", kind: "doc.review_requested"},
		{raw: "botlets_task", kind: "botlets.task"},
	} {
		kind, ok := NormalizeNotificationKind(tc.raw)
		if !ok || kind != tc.kind {
			t.Fatalf("NormalizeNotificationKind(%q) = %q %v, want %q true", tc.raw, kind, ok, tc.kind)
		}
	}
	if kind, ok := NormalizeNotificationKind("future_type"); ok || kind != "" {
		t.Fatalf("unknown notification kind = %q %v", kind, ok)
	}
}

func TestCloudMessageFromBotletsTaskLease(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.Notification.Type = "botlets_task"
	lease.Notification.DocSlug = ""
	lease.Notification.DocTitle = ""
	lease.Notification.FromHandle = "botlets.ai"
	lease.Notification.FromName = "Botlets Scheduler"
	lease.Notification.Context = "Run the configured bot task."
	lease.Notification.BotletsTask = validBotletsTaskNotification()

	message, _, err := cloudMessageFromTestLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind != "botlets.task" || message.From != "@botlets.ai" || message.BotID != "ag_bot_stable" || message.BotAgentID != "ag_bot" {
		t.Fatalf("task message = %+v", message)
	}
	if message.Body.Content != "Run the configured bot task." {
		t.Fatalf("task body = %#v", message.Body)
	}
	if message.Refs["run_id"] != "blr_0123456789abcdef0123456789abcdef" || message.Refs["bot_slug"] != "reviewer" || message.Refs["schedule_version"] != 3 {
		t.Fatalf("task refs = %#v", message.Refs)
	}
}

func TestCloudMessageFromLeaseRejectsMismatchedNotificationID(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.NotificationID = "ntf_outer1234567890"
	lease.Notification.ID = "ntf_inner1234567890"
	_, _, err := cloudMessageFromTestLease(lease)
	if err == nil {
		t.Fatal("expected mismatched notification ids to be rejected")
	}
}

func TestCloudMessageFromLeaseRejectsInvalidLease(t *testing.T) {
	cases := []struct {
		name string
		edit func(*CloudNotificationLease)
	}{
		{name: "unknown_type", edit: func(lease *CloudNotificationLease) {
			lease.Notification.Type = "future_type"
		}},
		{name: "botlets_task_missing_payload", edit: func(lease *CloudNotificationLease) {
			lease.Notification.Type = "botlets_task"
			lease.Notification.BotletsTask = nil
		}},
		{name: "botlets_task_bad_run_id", edit: func(lease *CloudNotificationLease) {
			lease.Notification.Type = "botlets_task"
			lease.Notification.BotletsTask = &CloudBotletsTaskNotification{
				RunID:               "bad",
				Kind:                "scheduled",
				OwnerAgentID:        "ag_owner",
				BotAgentID:          "ag_bot",
				BotSlug:             "daily-summary",
				BotName:             "Daily Summary",
				BotHandle:           "max.daily-summary",
				ScheduledFor:        "2026-05-07T00:00:00Z",
				EnqueuedAt:          "2026-05-07T00:00:10Z",
				ScheduleVersion:     1,
				ExecutionGeneration: 1,
				SetupGeneration:     1,
				Cron:                "*/15 * * * *",
				Timezone:            "UTC",
			}
		}},
		{name: "missing_lease_expiry", edit: func(lease *CloudNotificationLease) {
			lease.LeaseExpiresAt = ""
		}},
		{name: "missing_claimed_at", edit: func(lease *CloudNotificationLease) {
			lease.ClaimedAt = ""
		}},
		{name: "malformed_claim_id", edit: func(lease *CloudNotificationLease) {
			lease.ClaimID = "bad_claim"
		}},
		{name: "prefix_only_claim_id", edit: func(lease *CloudNotificationLease) {
			lease.ClaimID = "clm_"
		}},
		{name: "whitespace_claim_id", edit: func(lease *CloudNotificationLease) {
			lease.ClaimID = "clm_bad id"
		}},
		{name: "prefix_only_notification_id", edit: func(lease *CloudNotificationLease) {
			lease.NotificationID = "ntf_"
			lease.Notification.ID = "ntf_"
		}},
		{name: "whitespace_notification_id", edit: func(lease *CloudNotificationLease) {
			lease.NotificationID = "ntf_bad id"
			lease.Notification.ID = "ntf_bad id"
		}},
		{name: "malformed_expiry", edit: func(lease *CloudNotificationLease) {
			lease.LeaseExpiresAt = "tomorrow"
		}},
		{name: "expired_lease", edit: func(lease *CloudNotificationLease) {
			lease.LeaseExpiresAt = "2026-05-06T23:59:00Z"
		}},
		{name: "access_token_in_context", edit: func(lease *CloudNotificationLease) {
			lease.Notification.AccessToken = "550e8400-e29b-41d4-a716-446655440000"
			lease.Notification.Context = "Leaked 550e8400-e29b-41d4-a716-446655440000"
		}},
		{name: "access_token_in_from_handle", edit: func(lease *CloudNotificationLease) {
			token := "550e8400-e29b-41d4-a716-446655440000"
			lease.Notification.AccessToken = token
			lease.Notification.FromHandle = token
		}},
		{name: "access_token_in_doc_title", edit: func(lease *CloudNotificationLease) {
			lease.Notification.AccessToken = "550e8400-e29b-41d4-a716-446655440000"
			lease.Notification.DocTitle = "Token 550e8400-e29b-41d4-a716-446655440000"
		}},
		{name: "access_token_in_comment_id", edit: func(lease *CloudNotificationLease) {
			token := "550e8400-e29b-41d4-a716-446655440000"
			lease.Notification.AccessToken = token
			lease.Notification.CommentID = &token
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lease := validCloudNotificationLease()
			tc.edit(&lease)
			if _, _, err := cloudMessageFromTestLease(lease); err == nil {
				t.Fatal("expected invalid lease rejection")
			}
		})
	}
}

func TestCloudMessageFromBotletsTaskLeaseRejectsRegistryMismatch(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.Notification.Type = "botlets_task"
	lease.Notification.BotletsTask = validBotletsTaskNotification()
	cases := []struct {
		name string
		edit func(*CloudBotletsTaskNotification)
	}{
		{name: "bot_slug", edit: func(task *CloudBotletsTaskNotification) { task.BotSlug = "other" }},
		{name: "bot_handle", edit: func(task *CloudBotletsTaskNotification) { task.BotHandle = "max.other" }},
		{name: "bot_agent_id", edit: func(task *CloudBotletsTaskNotification) { task.BotAgentID = "ag_other" }},
		{name: "bot_id", edit: func(task *CloudBotletsTaskNotification) { task.BotID = "ag_other_bot" }},
		{name: "owner_agent_id", edit: func(task *CloudBotletsTaskNotification) { task.OwnerAgentID = "ag_other_owner" }},
		{name: "setup_generation", edit: func(task *CloudBotletsTaskNotification) { task.SetupGeneration = 6 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := lease
			task := *lease.Notification.BotletsTask
			tc.edit(&task)
			next.Notification.BotletsTask = &task
			if _, _, err := cloudMessageFromTestLease(next); err == nil {
				t.Fatal("expected botlets task mismatch rejection")
			}
		})
	}
}

func TestCloudMessageFromBotletsTaskLeaseRejectsHandleSuffixAsSlug(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.Notification.Type = "botlets_task"
	lease.Notification.BotletsTask = validBotletsTaskNotification()
	_, _, err := CloudMessageFromLease(
		"msg_abcdefghijklmnopqrst",
		"max.reviewer",
		BotRegistryEntry{
			Name:   "research-reader",
			Handle: "max.reviewer",
			BrainRef: &BotBrainRef{
				WorkspaceID:     "ws_brain",
				OwnerAgentID:    "ag_owner",
				BotAgentID:      "ag_bot",
				ContainerID:     "lc_brain",
				RootFolderID:    "lf_brain",
				RelativePath:    "Botlets/max/research-reader/brain",
				SetupGeneration: 5,
			},
		},
		AgentProfile{Handle: "max.reviewer", BaseURL: "https://comment.example"},
		lease,
		time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	)
	if err == nil {
		t.Fatal("expected handle suffix to be rejected as bot_slug")
	}
}

func TestCloudMessageFromBotletsTaskLeaseRejectsIncompleteBrainRef(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.Notification.Type = "botlets_task"
	lease.Notification.BotletsTask = validBotletsTaskNotification()
	_, _, err := CloudMessageFromLease(
		"msg_abcdefghijklmnopqrst",
		"max.reviewer",
		BotRegistryEntry{
			Name:   "reviewer",
			BotID:  "ag_bot_stable",
			Handle: "max.reviewer",
			BrainRef: &BotBrainRef{
				WorkspaceID:  "ws_brain",
				ContainerID:  "lc_brain",
				RootFolderID: "lf_brain",
				RelativePath: "Botlets/max/reviewer/brain",
			},
		},
		AgentProfile{Handle: "max.reviewer", BaseURL: "https://comment.example"},
		lease,
		time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	)
	if err == nil {
		t.Fatal("expected incomplete brain ref to reject botlets task")
	}
}

func TestCloudMessageFromLeaseCanonicalizesTimestamps(t *testing.T) {
	lease := validCloudNotificationLease()
	lease.ClaimedAt = "2026-05-06T17:00:00-07:00"
	lease.LeaseExpiresAt = "2026-05-06T17:10:00.123456789-07:00"
	lease.Notification.CreatedAt = "2026-05-06T17:01:02.5-07:00"

	message, metadata, err := cloudMessageFromTestLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	if message.CreatedAt != "2026-05-07T00:01:02.500000000Z" {
		t.Fatalf("message created_at = %q", message.CreatedAt)
	}
	if message.LeaseExpiresAt != "2026-05-07T00:10:00.123456789Z" {
		t.Fatalf("message lease_expires_at = %q", message.LeaseExpiresAt)
	}
	if metadata.ClaimedAt != "2026-05-07T00:00:00.000000000Z" || metadata.LeaseExpiresAt != message.LeaseExpiresAt {
		t.Fatalf("metadata timestamps = %+v", metadata)
	}
}

func TestPrivateCloudMessageMetadataValidation(t *testing.T) {
	metadata := PrivateCloudMessageMetadata{
		LocalMessageID: "msg_abcdefghijklmnopqrst",
		Source:         "comment.io",
		Profile:        "max.reviewer",
		BaseURL:        "https://comment.example",
		NotificationID: "ntf_cloudnotification1234567890",
		ClaimID:        "clm_cloudclaim1234567890",
		ClaimedAt:      "2026-05-07T00:00:00Z",
		LeaseExpiresAt: "2026-05-07T00:10:00Z",
	}
	if err := validatePrivateCloudMessageMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*PrivateCloudMessageMetadata)
	}{
		{name: "missing_claimed_at", edit: func(m *PrivateCloudMessageMetadata) { m.ClaimedAt = "" }},
		{name: "missing_lease_expiry", edit: func(m *PrivateCloudMessageMetadata) { m.LeaseExpiresAt = "" }},
		{name: "oversized_base_url", edit: func(m *PrivateCloudMessageMetadata) { m.BaseURL = "https://" + strings.Repeat("a", 2050) }},
		{name: "base_url_control", edit: func(m *PrivateCloudMessageMetadata) { m.BaseURL = "https://comment.example\nbad" }},
		{name: "oversized_access_token", edit: func(m *PrivateCloudMessageMetadata) { m.AccessToken = strings.Repeat("a", 8193) }},
		{name: "access_token_control", edit: func(m *PrivateCloudMessageMetadata) { m.AccessToken = "private\nbad" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := metadata
			tc.edit(&candidate)
			if err := validatePrivateCloudMessageMetadata(candidate); err == nil {
				t.Fatal("expected invalid private metadata rejection")
			}
		})
	}
}

func TestWritePrivateCloudMessageMetadataRejectsInvalidPrivateMetadata(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	metadata := PrivateCloudMessageMetadata{
		LocalMessageID: "msg_abcdefghijklmnopqrst",
		Source:         "comment.io",
		Profile:        "max.reviewer",
		BaseURL:        "https://comment.example",
		NotificationID: "ntf_cloudnotification1234567890",
		ClaimID:        "clm_cloudclaim1234567890",
		ClaimedAt:      "2026-05-07T00:00:00Z",
		LeaseExpiresAt: "2026-05-07T00:10:00Z",
		AccessToken:    strings.Repeat("a", 8193),
	}
	if err := WritePrivateCloudMessageMetadata(paths, metadata); err == nil || err.Error() != "invalid private cloud metadata" {
		t.Fatalf("invalid private metadata err = %v", err)
	}
}

func TestReadPrivateCloudMessageMetadataRejectsOversizedFile(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	path := privateCloudMessagePath(paths, "max.reviewer", "msg_abcdefghijklmnopqrst")
	if err := WritePrivateFileAtomic(path, []byte(strings.Repeat("x", maxPrivateCloudMetadataBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateCloudMessageMetadata(paths, "max.reviewer", "msg_abcdefghijklmnopqrst"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized private metadata read err = %v", err)
	}
}

func cloudMessageFromTestLease(lease CloudNotificationLease) (CloudNotificationMessage, PrivateCloudMessageMetadata, error) {
	return CloudMessageFromLease(
		"msg_abcdefghijklmnopqrst",
		"max.reviewer",
		BotRegistryEntry{
			Name:   "reviewer",
			BotID:  "ag_bot_stable",
			Handle: "max.reviewer",
			BrainRef: &BotBrainRef{
				WorkspaceID:     "ws_brain",
				OwnerAgentID:    "ag_owner",
				BotAgentID:      "ag_bot",
				ContainerID:     "lc_brain",
				RootFolderID:    "lf_brain",
				RelativePath:    "Botlets/max/reviewer/brain",
				SetupGeneration: 5,
			},
		},
		AgentProfile{Handle: "max.reviewer", BaseURL: "https://comment.example"},
		lease,
		time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	)
}

func validBotletsTaskNotification() *CloudBotletsTaskNotification {
	return &CloudBotletsTaskNotification{
		RunID:               "blr_0123456789abcdef0123456789abcdef",
		Kind:                "scheduled",
		OwnerAgentID:        "ag_owner",
		BotID:               "ag_bot_stable",
		BotAgentID:          "ag_bot",
		BotSlug:             "reviewer",
		BotName:             "Reviewer",
		BotHandle:           "max.reviewer",
		ScheduledFor:        "2026-05-07T00:00:00Z",
		EnqueuedAt:          "2026-05-07T00:00:10Z",
		ScheduleVersion:     3,
		ExecutionGeneration: 4,
		SetupGeneration:     5,
		Cron:                "0 9 * * 1-5",
		Timezone:            "America/Los_Angeles",
	}
}

func validCloudNotificationLease() CloudNotificationLease {
	return CloudNotificationLease{
		ClaimID:        "clm_cloudclaim1234567890",
		NotificationID: "ntf_cloudnotification1234567890",
		ClaimedAt:      "2026-05-07T00:00:00Z",
		LeaseExpiresAt: "2026-05-07T00:10:00Z",
		Notification: CloudNotification{
			ID:         "ntf_cloudnotification1234567890",
			Type:       "mention",
			DocSlug:    "abc123",
			DocTitle:   "Design Review",
			FromHandle: "max.sender",
			Context:    "Please review.",
			CreatedAt:  "2026-05-07T00:00:00Z",
		},
	}
}
