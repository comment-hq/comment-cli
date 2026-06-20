package commentbus

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateSocketRequest(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	messageID := "msg_abcdefghijklmnopqrst"
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_1",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"from_bot":        "writer",
			"to":              []any{"@max.reviewer"},
			"body":            map[string]any{"format": "markdown", "content": "Please review this."},
			"refs":            map[string]any{"doc_slug": "abc123"},
			"idempotency_key": "op_abcdefghijklmnopqrst",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{ID: "req_2", Op: "health", Params: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_reloadhome",
		Op:     "reload-profiles",
		Auth:   &SocketAuth{Mode: "owner", Capability: "x-repeat-owner-capability"},
		Params: map[string]any{"botlets_home": "~/botlets"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_sessionreload",
		Op:     "reload-profiles",
		Auth:   testSessionAuth("max.reviewer"),
		Params: map[string]any{"botlets_home": "~/botlets"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_sessionreloadmissing",
		Op:     "reload-profiles",
		Auth:   testSessionAuth("max.reviewer"),
		Params: map[string]any{},
	}); err == nil || err.Error() != "session reload requires botlets_home" {
		t.Fatalf("session reload without botlets_home error = %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_3",
		Op:     "messages.receive",
		Auth:   auth,
		Params: map[string]any{"message_id": messageID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_runtimestart",
		Op:   "runtime.start",
		Auth: auth,
		Params: map[string]any{
			"profile":         "max.reviewer",
			"cwd":             "~/project",
			"runtime_command": []any{"codex", "--model", "gpt-5"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_runtimetaskstart",
		Op:   "runtime.start",
		Auth: auth,
		Params: map[string]any{
			"profile":         "max.reviewer",
			"cwd":             "~/project",
			"runtime_command": []any{"codex", "--model", "gpt-5"},
			"role":            RuntimeRoleTask,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_runtimestop",
		Op:     "runtime.stop",
		Auth:   auth,
		Params: map[string]any{"run_id": "sess_abcdefghijklmnopqrst"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_runtimestatus",
		Op:     "runtime.status",
		Auth:   auth,
		Params: map[string]any{"profile": "max.reviewer"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_runtimelist",
		Op:     "runtime.list",
		Auth:   auth,
		Params: map[string]any{"profile": "max.reviewer"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_activitycomplete",
		Op:     "activity.complete",
		Auth:   auth,
		Params: map[string]any{"message_id": messageID, "profile": "max.reviewer"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSocketRequestAcceptsRepairDryRunFalse(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	err := ValidateSocketRequest(SocketRequest{
		ID:   "req_repair",
		Op:   "messages.repair",
		Auth: auth,
		Params: map[string]any{
			"dry_run": false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateSocketRequestUsesUTF8ByteLengthLimits(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	messageID := "msg_abcdefghijklmnopqrst"
	fourByte := "😀"
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_maxutfbody",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": strings.Repeat(fourByte, 250_000)},
		},
	}); err != nil {
		t.Fatalf("max UTF-8 body rejected: %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_bigutfbody",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": strings.Repeat(fourByte, 250_001)},
		},
	}); err == nil || err.Error() != "invalid body" {
		t.Fatalf("oversized UTF-8 body error = %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_maxutfthread",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"to":        []any{"reviewer"},
			"body":      map[string]any{"format": "markdown", "content": "thread byte boundary"},
			"thread_id": strings.Repeat(fourByte, 64),
			"refs":      map[string]any{"note": strings.Repeat(fourByte, 256)},
		},
	}); err != nil {
		t.Fatalf("max UTF-8 thread/refs rejected: %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_bigutfthread",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"to":        []any{"reviewer"},
			"body":      map[string]any{"format": "markdown", "content": "thread byte boundary"},
			"thread_id": strings.Repeat(fourByte, 65),
		},
	}); err == nil || err.Error() != "invalid thread_id" {
		t.Fatalf("oversized UTF-8 thread error = %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:   "req_bigutfref",
		Op:   "messages.send",
		Auth: auth,
		Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "ref byte boundary"},
			"refs": map[string]any{"note": strings.Repeat(fourByte, 257)},
		},
	}); err == nil || err.Error() != "invalid refs" {
		t.Fatalf("oversized UTF-8 ref error = %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_maxutfreason",
		Op:     "messages.release",
		Auth:   auth,
		Params: map[string]any{"message_id": messageID, "reason": strings.Repeat(fourByte, 128)},
	}); err != nil {
		t.Fatalf("max UTF-8 reason rejected: %v", err)
	}
	if err := ValidateSocketRequest(SocketRequest{
		ID:     "req_bigutfreason",
		Op:     "messages.release",
		Auth:   auth,
		Params: map[string]any{"message_id": messageID, "reason": strings.Repeat(fourByte, 129)},
	}); err == nil || err.Error() != "invalid reason" {
		t.Fatalf("oversized UTF-8 reason error = %v", err)
	}
}

func TestValidateSocketRequestAcceptsMessageMutationScope(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	err := ValidateSocketRequest(SocketRequest{
		ID:   "req_scopedreceive",
		Op:   "messages.receive",
		Auth: auth,
		Params: map[string]any{
			"message_id": "msg_abcdefghijklmnopqrst",
			"bot":        "reviewer",
			"profile":    "max.reviewer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateSocketRequestRejectsSessionAuthOwnerOnlyOperations(t *testing.T) {
	auth := testSessionAuth("max.reviewer")
	cases := []SocketRequest{
		{ID: "req_sessionsent", Op: "messages.sent", Auth: auth, Params: map[string]any{}},
		{ID: "req_sessionrepair", Op: "messages.repair", Auth: auth, Params: map[string]any{"dry_run": true}},
		{ID: "req_sessionregister", Op: "sessions.register", Auth: auth, Params: map[string]any{
			"profile":    "max.reviewer",
			"bot_name":   "reviewer",
			"scope_type": "profile",
			"scope_id":   "max.reviewer",
			"session_id": "sess_abcdefghijklmnopqrst",
			"generation": "gen_abcdefghijklmnop",
		}},
		{ID: "req_sessionstart", Op: "sessions.start", Auth: auth, Params: map[string]any{
			"profile": "max.reviewer",
			"bot":     "reviewer",
		}},
	}
	for _, tc := range cases {
		err := ValidateSocketRequest(tc)
		if err == nil || err.Error() != "session auth is not allowed for this operation" {
			t.Fatalf("%s: error = %v, want session auth rejection", tc.ID, err)
		}
	}
}

func TestValidateSocketRequestRejectsUnsafeParams(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	unscopedOwnerAuth := &SocketAuth{Mode: "owner", Capability: "x-repeat-owner-capability"}
	messageID := "msg_abcdefghijklmnopqrst"
	cases := []SocketRequest{
		{ID: "req_badmessage", Op: "messages.receive", Auth: auth, Params: map[string]any{"message_id": "clm_bad;echo owned"}},
		{ID: "req_badop", Op: "messages.ack", Auth: auth, Params: map[string]any{"message_id": messageID, "op_id": "op_short"}},
		{ID: "req_badbody", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "leaked as_ag_123_secret"},
		}},
		{ID: "req_badbodyprefix", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "leaked_as_ag_123_secret"},
		}},
		{ID: "req_badcapbody", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "leaked cap_abcdefghijklmnopqrst"},
		}},
		{ID: "req_badrefkey", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "secret ref"},
			"refs": map[string]any{"access_token": "tok_document"},
		}},
		{ID: "req_badrefvalue", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":   []any{"reviewer"},
			"body": map[string]any{"format": "markdown", "content": "secret ref"},
			"refs": map[string]any{"note": "claim clm_abcdef123456"},
		}},
		{ID: "req_badthread", Op: "messages.send", Auth: auth, Params: map[string]any{
			"to":        []any{"reviewer"},
			"body":      map[string]any{"format": "markdown", "content": "secret thread"},
			"thread_id": "ntf_abcdef123456",
		}},
		{ID: "req_badreason", Op: "messages.release", Auth: auth, Params: map[string]any{"message_id": messageID, "reason": "claim clm_abcdef123456"}},
		{ID: "req_notificationopremoved", Op: "notifications.ack", Auth: auth, Params: map[string]any{"claim_id": "clm_abcdefghijklmnopqrst"}},
		{ID: "req_badruntimecommand", Op: "runtime.start", Auth: auth, Params: map[string]any{"profile": "max.reviewer", "cwd": "~/project", "runtime_command": []any{"codex", "leaked as_badsecret"}}},
		{ID: "req_badruntimerole", Op: "runtime.start", Auth: auth, Params: map[string]any{"profile": "max.reviewer", "cwd": "~/project", "runtime_command": []any{"codex"}, "role": "extra"}},
		{ID: "req_mismatchruntime", Op: "runtime.start", Auth: testOwnerAuth("max.sender"), Params: map[string]any{"profile": "max.reviewer", "cwd": "~/project", "runtime_command": []any{"codex"}}},
		{ID: "req_mismatchruntimestatus", Op: "runtime.status", Auth: testOwnerAuth("max.sender"), Params: map[string]any{"profile": "max.reviewer"}},
		{ID: "req_mismatchruntimelist", Op: "runtime.list", Auth: testOwnerAuth("max.sender"), Params: map[string]any{"profile": "max.reviewer"}},
		{ID: "req_mismatchactivitycomplete", Op: "activity.complete", Auth: testOwnerAuth("max.sender"), Params: map[string]any{"message_id": messageID, "profile": "max.reviewer"}},
		{ID: "req_unscopedactivitycomplete", Op: "activity.complete", Auth: unscopedOwnerAuth, Params: map[string]any{"message_id": messageID, "profile": "max.reviewer"}},
		{ID: "req_unscopedruntimestop", Op: "runtime.stop", Auth: unscopedOwnerAuth, Params: map[string]any{"run_id": "sess_abcdefghijklmnopqrst"}},
		{ID: "req_sessionruntimestart", Op: "runtime.start", Auth: testSessionAuth("max.reviewer"), Params: map[string]any{"profile": "max.reviewer", "cwd": "~/project", "runtime_command": []any{"codex"}}},
		{ID: "req_sessionruntimestatus", Op: "runtime.status", Auth: testSessionAuth("max.reviewer"), Params: map[string]any{"profile": "max.reviewer"}},
		{ID: "req_badreloadhome", Op: "reload-profiles", Auth: &SocketAuth{Mode: "owner", Capability: "x-repeat-owner-capability"}, Params: map[string]any{"botlets_home": "/tmp/cap_secretshapedvalue1234567890"}},
		{ID: "req_badscope", Op: "sessions.register", Auth: auth, Params: map[string]any{
			"profile":    "max.reviewer",
			"bot_name":   "reviewer",
			"scope_type": "doc",
			"scope_id":   "bad/slug",
		}},
		{ID: "req_badmessagescope", Op: "sessions.register", Auth: auth, Params: map[string]any{
			"profile":    "max.reviewer",
			"bot_name":   "reviewer",
			"scope_type": "message",
			"scope_id":   messageID,
		}},
		{ID: "req_badstartnullscope", Op: "sessions.start", Auth: auth, Params: map[string]any{
			"scope_id": nil,
		}},
		{ID: "req_badstartscopenoprofile", Op: "sessions.start", Auth: auth, Params: map[string]any{
			"bot":        "reviewer",
			"scope_type": "profile",
			"scope_id":   "max.reviewer",
		}},
		{ID: "req_badstopnoselector", Op: "sessions.stop", Auth: auth, Params: map[string]any{}},
		{ID: "req_badnudgenoselector", Op: "sessions.nudge", Auth: auth, Params: map[string]any{
			"message_id": messageID,
		}},
		{ID: "req_badsessionsent", Op: "messages.sent", Auth: testSessionAuth("max.reviewer"), Params: map[string]any{}},
	}
	for _, tc := range cases {
		if err := ValidateSocketRequest(tc); err == nil {
			t.Fatalf("%s: expected validation error", tc.ID)
		}
	}
}

func TestValidateSocketRequestAcceptsSessionAuthNudgeWithoutSelector(t *testing.T) {
	profile := "max.reviewer"
	sessionID := "sess_abcdefghijklmnopqrst"
	generation := "gen_abcdefghijklmnop"
	err := ValidateSocketRequest(SocketRequest{
		ID: "req_sessionnudge",
		Op: "sessions.nudge",
		Auth: &SocketAuth{
			Mode:              "session",
			Profile:           &profile,
			SessionID:         &sessionID,
			SessionGeneration: &generation,
			Capability:        "x-repeat-session-capability",
		},
		Params: map[string]any{"message_id": "msg_abcdefghijklmnopqrst"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateSocketRequestAcceptsSessionRegister(t *testing.T) {
	auth := testOwnerAuth("max.reviewer")
	err := ValidateSocketRequest(SocketRequest{
		ID:   "req_session",
		Op:   "sessions.register",
		Auth: auth,
		Params: map[string]any{
			"profile":    "max.reviewer",
			"bot_name":   "reviewer",
			"scope_type": "profile",
			"scope_id":   "max.reviewer",
			"session_id": "sess_abcdefghijklmnopqrst",
			"generation": "gen_abcdefghijklmnop",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidateSocketRequestRejectsExplicitEmptyOwnerAuthFields(t *testing.T) {
	empty := ""
	cases := []SocketAuth{
		{Mode: "owner", Capability: "x-repeat-owner-capability", Profile: &empty},
		{Mode: "owner", Capability: "x-repeat-owner-capability", SessionID: &empty},
		{Mode: "owner", Capability: "x-repeat-owner-capability", SessionGeneration: &empty},
	}
	for i, auth := range cases {
		err := ValidateSocketRequest(SocketRequest{
			ID:     "req_emptyowner-auth",
			Op:     "messages.list",
			Auth:   &auth,
			Params: map[string]any{},
		})
		if err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
}

func TestValidateSocketRequestRejectsExplicitNullJSONFields(t *testing.T) {
	cases := []string{
		`{"id":"req_nullhealth-auth","op":"health","auth":null,"params":{}}`,
		`{"id":"req_nullowner-profile","op":"messages.list","auth":{"mode":"owner","capability":"x-repeat-owner-capability","profile":null},"params":{}}`,
		`{"id":"req_nullowner-session","op":"messages.list","auth":{"mode":"owner","capability":"x-repeat-owner-capability","session_id":null},"params":{}}`,
		`{"id":"req_nullowner-generation","op":"messages.list","auth":{"mode":"owner","capability":"x-repeat-owner-capability","session_generation":null},"params":{}}`,
	}
	for _, data := range cases {
		var req SocketRequest
		if err := json.Unmarshal([]byte(data), &req); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSocketRequest(req); err == nil {
			t.Fatalf("expected validation error for %s", data)
		}
	}
}

func TestValidateSocketRequestRejectsMissingAuthBeforeParams(t *testing.T) {
	err := ValidateSocketRequest(SocketRequest{
		ID:     "req_missingauth",
		Op:     "messages.receive",
		Params: map[string]any{"message_id": "clm_bad;echo owned"},
	})
	if err == nil || err.Error() != "missing auth" {
		t.Fatalf("error = %v, want missing auth", err)
	}
}

func TestValidateSocketRequestRejectsMissingAuthBeforeMalformedJSONParams(t *testing.T) {
	var req SocketRequest
	if err := json.Unmarshal([]byte(`{"id":"req_malformedparams","op":"messages.receive","params":[]}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != "req_malformedparams" || req.Op != "messages.receive" {
		t.Fatalf("request envelope was not preserved: %+v", req)
	}
	err := ValidateSocketRequest(req)
	if err == nil || err.Error() != "missing auth" {
		t.Fatalf("error = %v, want missing auth", err)
	}
}

func testOwnerAuth(profile string) *SocketAuth {
	return &SocketAuth{Mode: "owner", Capability: "x-repeat-owner-capability", Profile: &profile}
}

func testSessionAuth(profile string) *SocketAuth {
	sessionID := "sess_abcdefghijklmnopqrst"
	generation := "gen_abcdefghijklmnop"
	return &SocketAuth{
		Mode:              "session",
		Capability:        "x-repeat-session-capability",
		Profile:           &profile,
		SessionID:         &sessionID,
		SessionGeneration: &generation,
	}
}
