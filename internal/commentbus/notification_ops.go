package commentbus

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	cloudNotificationWaitOperationKind   = "notification.wait"
	maxCloudNotificationWaitOperationAge = time.Hour
	operationJournalStatePending         = "pending"
	operationJournalStateDone            = "done"
	declinedDuplicateReleaseReason       = "duplicate_notification_declined"
)

type CloudNotificationWaitOperation struct {
	OpID        string   `json:"op_id"`
	Kind        string   `json:"kind"`
	Profile     string   `json:"profile"`
	BotName     string   `json:"bot_name,omitempty"`
	Kinds       []string `json:"kinds,omitempty"`
	TimeoutMS   int64    `json:"timeout_ms"`
	LeaseTTLMS  int64    `json:"lease_ttl_ms"`
	LeaseHolder string   `json:"lease_holder"`
	Attempts    int      `json:"attempts"`
	CreatedAt   string   `json:"created_at"`
	LastAttempt string   `json:"last_attempt_at"`
}

type CloudNotificationClaimOperation struct {
	OpID               string `json:"op_id"`
	IdempotencyKey     string `json:"idempotency_key"`
	Operation          string `json:"operation"`
	Profile            string `json:"profile"`
	CredentialProfile  string `json:"credential_profile,omitempty"`
	LocalMessageID     string `json:"local_message_id"`
	PrivateMetadataRef string `json:"private_metadata_ref"`
	ClaimID            string `json:"claim_id"`
	NotificationID     string `json:"notification_id"`
	ClaimHolder        string `json:"claim_holder,omitempty"`
	LeaseTTLMS         int64  `json:"lease_ttl_ms,omitempty"`
	ReleaseReason      string `json:"release_reason,omitempty"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	State              string `json:"state"`
	Attempts           int    `json:"attempts"`
	LastError          string `json:"last_error,omitempty"`
}

func BeginCloudNotificationWaitOperation(paths Paths, profile string, timeout time.Duration, leaseTTL time.Duration, leaseHolder string, now time.Time) (CloudNotificationWaitOperation, error) {
	return BeginCloudNotificationWaitOperationForBot(paths, profile, "", timeout, leaseTTL, leaseHolder, now)
}

func BeginCloudNotificationWaitOperationForBot(paths Paths, profile string, botName string, timeout time.Duration, leaseTTL time.Duration, leaseHolder string, now time.Time) (CloudNotificationWaitOperation, error) {
	return BeginCloudNotificationWaitOperationForBotAndKinds(paths, profile, botName, timeout, leaseTTL, leaseHolder, nil, now)
}

func BeginCloudNotificationWaitOperationForBotAndKinds(paths Paths, profile string, botName string, timeout time.Duration, leaseTTL time.Duration, leaseHolder string, kinds []string, now time.Time) (CloudNotificationWaitOperation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	canonicalKinds := canonicalCloudNotificationKinds(kinds)
	if existing, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, profile, botName, canonicalKinds); err != nil {
		return CloudNotificationWaitOperation{}, err
	} else if ok {
		if done, doneOK, doneErr := ReadDoneCloudNotificationWaitOperation(paths, existing.OpID); doneErr != nil {
			return CloudNotificationWaitOperation{}, doneErr
		} else if doneOK && sameCloudNotificationWaitOperation(done, existing) {
			if err := removePendingCloudNotificationWaitOperation(paths, existing); err != nil {
				return CloudNotificationWaitOperation{}, err
			}
		} else if shouldRetireCloudNotificationWaitOperation(existing, now) || !sameCloudNotificationWaitOperationRequest(existing, profile, botName, timeout, leaseTTL, leaseHolder, canonicalKinds) {
			if err := CompleteCloudNotificationWaitOperation(paths, existing, now); err != nil {
				return CloudNotificationWaitOperation{}, err
			}
		} else {
			return existing, nil
		}
	}
	return createCloudNotificationWaitOperation(paths, profile, botName, timeout, leaseTTL, leaseHolder, canonicalKinds, now)
}

func createCloudNotificationWaitOperation(paths Paths, profile string, botName string, timeout time.Duration, leaseTTL time.Duration, leaseHolder string, kinds []string, now time.Time) (CloudNotificationWaitOperation, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	canonicalKinds := canonicalCloudNotificationKinds(kinds)
	if existing, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, profile, botName, canonicalKinds); err != nil {
		return CloudNotificationWaitOperation{}, err
	} else if ok {
		if done, doneOK, doneErr := ReadDoneCloudNotificationWaitOperation(paths, existing.OpID); doneErr != nil {
			return CloudNotificationWaitOperation{}, doneErr
		} else if doneOK && sameCloudNotificationWaitOperation(done, existing) {
			if err := removePendingCloudNotificationWaitOperation(paths, existing); err != nil {
				return CloudNotificationWaitOperation{}, err
			}
		} else if !sameCloudNotificationWaitOperationRequest(existing, profile, botName, timeout, leaseTTL, leaseHolder, canonicalKinds) {
			if err := CompleteCloudNotificationWaitOperation(paths, existing, now); err != nil {
				return CloudNotificationWaitOperation{}, err
			}
		} else {
			return existing, nil
		}
	}
	if existing, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, profile, botName, canonicalKinds); err != nil {
		return CloudNotificationWaitOperation{}, err
	} else if ok {
		return existing, nil
	}
	opID, err := GenerateLocalID("op", 0)
	if err != nil {
		return CloudNotificationWaitOperation{}, err
	}
	op := CloudNotificationWaitOperation{
		OpID:        opID,
		Kind:        cloudNotificationWaitOperationKind,
		Profile:     profile,
		BotName:     botName,
		Kinds:       canonicalKinds,
		TimeoutMS:   durationMilliseconds(timeout),
		LeaseTTLMS:  durationMilliseconds(leaseTTL),
		LeaseHolder: leaseHolder,
		Attempts:    0,
		CreatedAt:   busTime(now.UTC()),
	}
	if err := writePendingCloudNotificationWaitOperation(paths, op, false); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, ok, readErr := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, profile, botName, canonicalKinds)
			if readErr != nil {
				return CloudNotificationWaitOperation{}, readErr
			}
			if ok {
				return existing, nil
			}
		}
		return CloudNotificationWaitOperation{}, err
	}
	return op, nil
}

func RecordCloudNotificationWaitOperationAttempt(paths Paths, op CloudNotificationWaitOperation, now time.Time) (CloudNotificationWaitOperation, error) {
	if err := validateCloudNotificationWaitOperation(op); err != nil {
		return CloudNotificationWaitOperation{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existing, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, op.Profile, op.BotName, op.Kinds)
	if err != nil {
		return CloudNotificationWaitOperation{}, err
	}
	if !ok || existing.OpID != op.OpID {
		return CloudNotificationWaitOperation{}, errors.New("notification wait operation mismatch")
	}
	if done, doneOK, doneErr := ReadDoneCloudNotificationWaitOperation(paths, existing.OpID); doneErr != nil {
		return CloudNotificationWaitOperation{}, doneErr
	} else if doneOK && sameCloudNotificationWaitOperation(done, existing) {
		_ = removePendingCloudNotificationWaitOperation(paths, existing)
		return CloudNotificationWaitOperation{}, errors.New("notification wait operation already completed")
	}
	existing.Attempts++
	existing.LastAttempt = busTime(now.UTC())
	if err := writePendingCloudNotificationWaitOperation(paths, existing, true); err != nil {
		return CloudNotificationWaitOperation{}, err
	}
	return existing, nil
}

func writePendingCloudNotificationWaitOperation(paths Paths, op CloudNotificationWaitOperation, replace bool) error {
	if err := validateCloudNotificationWaitOperation(op); err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	path := pendingCloudNotificationWaitOperationPath(paths, op.Profile, op.BotName, op.Kinds)
	if replace {
		return WritePrivateFileAtomic(path, append(data, '\n'), 0o600)
	}
	return WritePrivateFileAtomicNoReplace(path, append(data, '\n'), 0o600)
}

func ReadPendingCloudNotificationWaitOperation(paths Paths, profile string) (CloudNotificationWaitOperation, bool, error) {
	return ReadPendingCloudNotificationWaitOperationForBot(paths, profile, "")
}

func ReadPendingCloudNotificationWaitOperationForBot(paths Paths, profile string, botName string) (CloudNotificationWaitOperation, bool, error) {
	return ReadPendingCloudNotificationWaitOperationForBotAndKinds(paths, profile, botName, nil)
}

func ReadPendingCloudNotificationWaitOperationForBotAndKinds(paths Paths, profile string, botName string, kinds []string) (CloudNotificationWaitOperation, bool, error) {
	if !ProfileRE.MatchString(profile) {
		return CloudNotificationWaitOperation{}, false, errors.New("invalid notification wait operation")
	}
	if botName != "" && !BotNameRE.MatchString(botName) {
		return CloudNotificationWaitOperation{}, false, errors.New("invalid notification wait operation")
	}
	canonicalKinds := canonicalCloudNotificationKinds(kinds)
	path := pendingCloudNotificationWaitOperationPath(paths, profile, botName, canonicalKinds)
	data, err := readPrivateCloudMetadataFile(paths, path, "notification wait operation")
	if errors.Is(err, os.ErrNotExist) {
		return CloudNotificationWaitOperation{}, false, nil
	}
	if err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	var op CloudNotificationWaitOperation
	if err := json.Unmarshal(data, &op); err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	if err := validateCloudNotificationWaitOperation(op); err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	if op.Profile != profile || op.BotName != botName || !sameCloudNotificationKinds(op.Kinds, canonicalKinds) {
		return CloudNotificationWaitOperation{}, false, errors.New("notification wait operation mismatch")
	}
	return op, true, nil
}

func ReadPendingCloudNotificationWaitOperationWithRetry(paths Paths, profile string) (CloudNotificationWaitOperation, bool, error) {
	return ReadPendingCloudNotificationWaitOperationForBotWithRetry(paths, profile, "")
}

func ReadPendingCloudNotificationWaitOperationForBotWithRetry(paths Paths, profile string, botName string) (CloudNotificationWaitOperation, bool, error) {
	return ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, profile, botName, nil)
}

func ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths Paths, profile string, botName string, kinds []string) (CloudNotificationWaitOperation, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		op, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKinds(paths, profile, botName, kinds)
		if err == nil || !errors.Is(err, ErrCapabilityFileUnsafe) {
			return op, ok, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return CloudNotificationWaitOperation{}, false, lastErr
}

func ReadDoneCloudNotificationWaitOperation(paths Paths, opID string) (CloudNotificationWaitOperation, bool, error) {
	if !LocalOperationIDRE.MatchString(opID) {
		return CloudNotificationWaitOperation{}, false, errors.New("invalid notification wait operation")
	}
	data, err := readPrivateCloudMetadataFile(paths, doneCloudNotificationWaitOperationPath(paths, opID), "done notification wait operation")
	if errors.Is(err, os.ErrNotExist) {
		return CloudNotificationWaitOperation{}, false, nil
	}
	if err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	var done struct {
		CloudNotificationWaitOperation
		CompletedAt string `json:"completed_at"`
	}
	if err := json.Unmarshal(data, &done); err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	op := done.CloudNotificationWaitOperation
	if err := validateCloudNotificationWaitOperation(op); err != nil {
		return CloudNotificationWaitOperation{}, false, err
	}
	if op.OpID != opID {
		return CloudNotificationWaitOperation{}, false, errors.New("notification wait operation mismatch")
	}
	if done.CompletedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, done.CompletedAt); err != nil {
			return CloudNotificationWaitOperation{}, false, errors.New("notification wait operation mismatch")
		}
	}
	return op, true, nil
}

func CompleteCloudNotificationWaitOperation(paths Paths, op CloudNotificationWaitOperation, now time.Time) error {
	if err := validateCloudNotificationWaitOperation(op); err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	done := struct {
		CloudNotificationWaitOperation
		CompletedAt string `json:"completed_at"`
	}{
		CloudNotificationWaitOperation: op,
		CompletedAt:                    busTime(now.UTC()),
	}
	data, err := json.MarshalIndent(done, "", "  ")
	if err != nil {
		return err
	}
	if err := WritePrivateFileAtomic(doneCloudNotificationWaitOperationPath(paths, op.OpID), append(data, '\n'), 0o600); err != nil {
		return err
	}
	return removePendingCloudNotificationWaitOperation(paths, op)
}

func removePendingCloudNotificationWaitOperation(paths Paths, op CloudNotificationWaitOperation) error {
	pendingPath := pendingCloudNotificationWaitOperationPath(paths, op.Profile, op.BotName, op.Kinds)
	if existing, ok, err := ReadPendingCloudNotificationWaitOperationForBotAndKindsWithRetry(paths, op.Profile, op.BotName, op.Kinds); err != nil {
		return err
	} else if ok && existing.OpID == op.OpID {
		if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncDir(filepath.Dir(pendingPath)); err != nil {
			return err
		}
	}
	return nil
}

func sameCloudNotificationWaitOperation(a CloudNotificationWaitOperation, b CloudNotificationWaitOperation) bool {
	return a.OpID == b.OpID &&
		a.Kind == b.Kind &&
		a.Profile == b.Profile &&
		a.BotName == b.BotName &&
		sameCloudNotificationKinds(a.Kinds, b.Kinds) &&
		a.TimeoutMS == b.TimeoutMS &&
		a.LeaseTTLMS == b.LeaseTTLMS &&
		a.LeaseHolder == b.LeaseHolder
}

func sameCloudNotificationWaitOperationRequest(op CloudNotificationWaitOperation, profile string, botName string, timeout time.Duration, leaseTTL time.Duration, leaseHolder string, kinds []string) bool {
	return op.Kind == cloudNotificationWaitOperationKind &&
		op.Profile == profile &&
		op.BotName == botName &&
		sameCloudNotificationKinds(op.Kinds, canonicalCloudNotificationKinds(kinds)) &&
		op.TimeoutMS == durationMilliseconds(timeout) &&
		op.LeaseTTLMS == durationMilliseconds(leaseTTL) &&
		op.LeaseHolder == leaseHolder
}

func BeginCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, requestedOpID string, leaseTTL time.Duration, now time.Time, releaseReason ...string) (CloudNotificationClaimOperation, bool, error) {
	return beginCloudNotificationClaimOperation(paths, operation, profile, messageID, claimID, notificationID, "", requestedOpID, leaseTTL, now, false, releaseReason...)
}

func BeginCloudNotificationClaimOperationForHolder(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, claimHolder string, requestedOpID string, leaseTTL time.Duration, now time.Time, releaseReason ...string) (CloudNotificationClaimOperation, bool, error) {
	return beginCloudNotificationClaimOperation(paths, operation, profile, messageID, claimID, notificationID, claimHolder, requestedOpID, leaseTTL, now, false, releaseReason...)
}

func BeginDeclinedDuplicateCloudNotificationReleaseOperation(paths Paths, profile string, messageID string, claimID string, notificationID string, now time.Time) (CloudNotificationClaimOperation, bool, error) {
	return beginCloudNotificationClaimOperation(paths, "release", profile, messageID, claimID, notificationID, "", "", 0, now, true, declinedDuplicateReleaseReason)
}

func beginCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, claimHolder string, requestedOpID string, leaseTTL time.Duration, now time.Time, allowReservedReleaseReason bool, releaseReason ...string) (CloudNotificationClaimOperation, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reason := ""
	if len(releaseReason) > 0 {
		reason = releaseReason[0]
	}
	if err := validateCloudOperationReleaseReason(operation, reason); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if reason == declinedDuplicateReleaseReason && !allowReservedReleaseReason {
		return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
	}
	if requestedOpID == "" {
		if existing, ok, err := findPendingCloudNotificationClaimOperation(paths, operation, profile, messageID, claimID, notificationID, leaseTTL, reason); err != nil {
			return CloudNotificationClaimOperation{}, false, err
		} else if ok {
			if !releaseReasonLookupCompatible(existing, reason) {
				return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
			}
			if existing.ClaimHolder != claimHolder {
				return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
			}
			return existing, false, nil
		}
		var err error
		requestedOpID, err = GenerateLocalID("op", 0)
		if err != nil {
			return CloudNotificationClaimOperation{}, false, err
		}
	} else {
		if done, ok, err := ReadDoneCloudNotificationClaimOperation(paths, requestedOpID); err != nil {
			return CloudNotificationClaimOperation{}, false, err
		} else if ok {
			if sameCloudNotificationClaimOperationSelector(done, operation, profile, messageID, claimID, notificationID, claimHolder, leaseTTL, reason) {
				return done, true, nil
			}
			return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
		}
		if existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, requestedOpID); err != nil {
			return CloudNotificationClaimOperation{}, false, err
		} else if ok {
			if sameCloudNotificationClaimOperationSelector(existing, operation, profile, messageID, claimID, notificationID, claimHolder, leaseTTL, reason) {
				return existing, false, nil
			}
			return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
		}
	}
	op := CloudNotificationClaimOperation{
		OpID:               requestedOpID,
		IdempotencyKey:     requestedOpID,
		Operation:          operation,
		Profile:            profile,
		LocalMessageID:     messageID,
		PrivateMetadataRef: privateCloudMessageRef(profile, messageID),
		ClaimID:            claimID,
		NotificationID:     notificationID,
		ClaimHolder:        claimHolder,
		LeaseTTLMS:         durationMilliseconds(leaseTTL),
		ReleaseReason:      reason,
		CreatedAt:          busTime(now.UTC()),
		UpdatedAt:          busTime(now.UTC()),
		State:              operationJournalStatePending,
		Attempts:           0,
	}
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if err := WritePrivateFileAtomicNoReplace(pendingCloudNotificationClaimOperationPath(paths, requestedOpID), append(data, '\n'), 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, ok, readErr := ReadPendingCloudNotificationClaimOperationWithRetry(paths, requestedOpID)
			if readErr != nil {
				return CloudNotificationClaimOperation{}, false, readErr
			}
			if ok && sameCloudNotificationClaimOperation(existing, op) {
				return existing, false, nil
			}
			return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
		}
		return CloudNotificationClaimOperation{}, false, err
	}
	return op, false, nil
}

func FindDoneCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, leaseTTL time.Duration) (CloudNotificationClaimOperation, bool, error) {
	return findDoneCloudNotificationClaimOperation(paths, operation, profile, messageID, claimID, notificationID, leaseTTL, "")
}

func findDoneCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, leaseTTL time.Duration, releaseReason string) (CloudNotificationClaimOperation, bool, error) {
	return findCloudNotificationClaimOperationByMessage(paths.OpsDone, paths, operation, profile, messageID, claimID, notificationID, leaseTTL, releaseReason, ReadDoneCloudNotificationClaimOperation)
}

func ListPendingCloudNotificationClaimOperations(paths Paths) ([]CloudNotificationClaimOperation, error) {
	pathsToRead, err := filepath.Glob(filepath.Join(paths.OpsPending, "op_*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(pathsToRead)
	ops := make([]CloudNotificationClaimOperation, 0, len(pathsToRead))
	for _, path := range pathsToRead {
		opID := strings.TrimSuffix(filepath.Base(path), ".json")
		if !LocalOperationIDRE.MatchString(opID) {
			continue
		}
		op, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, opID)
		if err != nil {
			return nil, err
		}
		if ok {
			ops = append(ops, op)
		}
	}
	return ops, nil
}

func HasPendingCloudNotificationClaimOperationForMessage(paths Paths, profile string, messageID string, claimID string, notificationID string) (bool, error) {
	ops, err := ListPendingCloudNotificationClaimOperationsForMessage(paths, profile, messageID, claimID, notificationID)
	if err != nil {
		return false, err
	}
	return len(ops) > 0, nil
}

func HasPendingTerminalCloudNotificationClaimOperationForMessage(paths Paths, profile string, messageID string, claimID string, notificationID string) (bool, error) {
	ops, err := ListPendingCloudNotificationClaimOperationsForMessage(paths, profile, messageID, claimID, notificationID)
	if err != nil {
		return false, err
	}
	return HasPendingTerminalCloudNotificationClaimOperation(ops), nil
}

func HasPendingTerminalCloudNotificationClaimOperation(ops []CloudNotificationClaimOperation) bool {
	for _, op := range ops {
		if op.Operation == "ack" || (op.Operation == "release" && !isDeclinedDuplicateReleaseOperation(op)) {
			return true
		}
	}
	return false
}

func AbandonPendingCloudNotificationRenewOperations(paths Paths, ops []CloudNotificationClaimOperation) error {
	for _, op := range ops {
		if op.Operation == "renew" {
			if err := AbandonCloudNotificationClaimOperation(paths, op); err != nil {
				return err
			}
		}
	}
	return nil
}

func ListPendingCloudNotificationClaimOperationsForMessage(paths Paths, profile string, messageID string, claimID string, notificationID string) ([]CloudNotificationClaimOperation, error) {
	ops, err := ListPendingCloudNotificationClaimOperationsForLocalMessage(paths, profile, messageID)
	if err != nil {
		return nil, err
	}
	matching := make([]CloudNotificationClaimOperation, 0, len(ops))
	for _, op := range ops {
		if op.ClaimID == claimID && op.NotificationID == notificationID {
			matching = append(matching, op)
		}
	}
	return matching, nil
}

func ListPendingCloudNotificationClaimOperationsForLocalMessage(paths Paths, profile string, messageID string) ([]CloudNotificationClaimOperation, error) {
	ops, err := ListPendingCloudNotificationClaimOperations(paths)
	if err != nil {
		return nil, err
	}
	matching := make([]CloudNotificationClaimOperation, 0, len(ops))
	for _, op := range ops {
		if op.Profile != profile || op.LocalMessageID != messageID {
			continue
		}
		if done, ok, doneErr := ReadDoneCloudNotificationClaimOperation(paths, op.OpID); doneErr != nil {
			return nil, doneErr
		} else if ok && sameCloudNotificationClaimOperation(done, op) {
			continue
		}
		matching = append(matching, op)
	}
	return matching, nil
}

func FindPendingCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, leaseTTL time.Duration) (CloudNotificationClaimOperation, bool, error) {
	return findPendingCloudNotificationClaimOperation(paths, operation, profile, messageID, claimID, notificationID, leaseTTL, "")
}

func findPendingCloudNotificationClaimOperation(paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, leaseTTL time.Duration, releaseReason string) (CloudNotificationClaimOperation, bool, error) {
	op, ok, err := findCloudNotificationClaimOperationByMessage(paths.OpsPending, paths, operation, profile, messageID, claimID, notificationID, leaseTTL, releaseReason, ReadPendingCloudNotificationClaimOperationWithRetry)
	if err != nil || !ok {
		return op, ok, err
	}
	if done, doneOK, doneErr := ReadDoneCloudNotificationClaimOperation(paths, op.OpID); doneErr != nil {
		return CloudNotificationClaimOperation{}, false, doneErr
	} else if doneOK && sameCloudNotificationClaimOperation(done, op) {
		return CloudNotificationClaimOperation{}, false, nil
	}
	return op, true, nil
}

func findCloudNotificationClaimOperationByMessage(root string, paths Paths, operation string, profile string, messageID string, claimID string, notificationID string, leaseTTL time.Duration, releaseReason string, read func(Paths, string) (CloudNotificationClaimOperation, bool, error)) (CloudNotificationClaimOperation, bool, error) {
	if !isCloudNotificationClaimOperation(operation) || !ProfileRE.MatchString(profile) || !LocalMessageIDRE.MatchString(messageID) || !isSafeCloudID("claim", claimID) || !isSafeCloudID("notification", notificationID) {
		return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
	}
	if err := validateCloudOperationReleaseReason(operation, releaseReason); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	leaseTTLMS := durationMilliseconds(leaseTTL)
	if operation == "renew" {
		if leaseTTLMS < 1000 || leaseTTLMS > 3_600_000 {
			return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
		}
	} else if leaseTTLMS != 0 {
		return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
	}
	pathsToRead, err := filepath.Glob(filepath.Join(root, "op_*.json"))
	if err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	for _, path := range pathsToRead {
		opID := strings.TrimSuffix(filepath.Base(path), ".json")
		if !LocalOperationIDRE.MatchString(opID) {
			continue
		}
		op, ok, err := read(paths, opID)
		if err != nil {
			return CloudNotificationClaimOperation{}, false, err
		}
		if ok &&
			op.Operation == operation &&
			op.Profile == profile &&
			op.LocalMessageID == messageID &&
			op.PrivateMetadataRef == privateCloudMessageRef(profile, messageID) &&
			op.ClaimID == claimID &&
			op.NotificationID == notificationID &&
			op.LeaseTTLMS == leaseTTLMS &&
			releaseReasonLookupCompatible(op, releaseReason) {
			return op, true, nil
		}
	}
	return CloudNotificationClaimOperation{}, false, nil
}

func RecordCloudNotificationClaimOperationAttempt(paths Paths, op CloudNotificationClaimOperation, now time.Time) (CloudNotificationClaimOperation, error) {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, op.OpID)
	if err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	if !ok || !sameCloudNotificationClaimOperation(existing, op) {
		return CloudNotificationClaimOperation{}, errors.New("notification claim operation mismatch")
	}
	existing.Attempts++
	existing.UpdatedAt = busTime(now.UTC())
	existing.LastError = ""
	if err := writePendingCloudNotificationClaimOperation(paths, existing); err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	return existing, nil
}

func EnsureCloudNotificationClaimOperationCredentialProfile(paths Paths, op CloudNotificationClaimOperation, credentialProfile string, now time.Time) (CloudNotificationClaimOperation, error) {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	if credentialProfile == "" || credentialProfile == op.Profile {
		return op, nil
	}
	if !ProfileRE.MatchString(credentialProfile) {
		return CloudNotificationClaimOperation{}, errors.New("invalid notification claim operation")
	}
	existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, op.OpID)
	if err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	if !ok || !sameCloudNotificationClaimOperation(existing, op) {
		return CloudNotificationClaimOperation{}, errors.New("notification claim operation mismatch")
	}
	if existing.CredentialProfile == credentialProfile {
		return existing, nil
	}
	if existing.CredentialProfile != "" {
		return CloudNotificationClaimOperation{}, errors.New("notification claim operation mismatch")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existing.CredentialProfile = credentialProfile
	existing.UpdatedAt = busTime(now.UTC())
	if err := writePendingCloudNotificationClaimOperation(paths, existing); err != nil {
		return CloudNotificationClaimOperation{}, err
	}
	return existing, nil
}

func RecordCloudNotificationClaimOperationFailure(paths Paths, op CloudNotificationClaimOperation, failure error, now time.Time) error {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return err
	}
	if failure == nil {
		failure = errors.New("notification operation failed")
	}
	existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, op.OpID)
	if err != nil {
		return err
	}
	if !ok || !sameCloudNotificationClaimOperation(existing, op) {
		return errors.New("notification claim operation mismatch")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existing.UpdatedAt = busTime(now.UTC())
	lastError := failure.Error()
	if len(lastError) > 512 || strings.ContainsAny(lastError, "\r\n\x00") || containsSecretValue(lastError) {
		lastError = "notification operation failed"
	}
	existing.LastError = lastError
	return writePendingCloudNotificationClaimOperation(paths, existing)
}

func AbandonCloudNotificationClaimOperation(paths Paths, op CloudNotificationClaimOperation) error {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return err
	}
	pendingPath := pendingCloudNotificationClaimOperationPath(paths, op.OpID)
	if existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, op.OpID); err != nil {
		return err
	} else if ok && sameCloudNotificationClaimOperation(existing, op) {
		if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDir(filepath.Dir(pendingPath))
	}
	return nil
}

func writePendingCloudNotificationClaimOperation(paths Paths, op CloudNotificationClaimOperation) error {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return err
	}
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFileAtomic(pendingCloudNotificationClaimOperationPath(paths, op.OpID), append(data, '\n'), 0o600)
}

func ReadPendingCloudNotificationClaimOperation(paths Paths, opID string) (CloudNotificationClaimOperation, bool, error) {
	if !LocalOperationIDRE.MatchString(opID) {
		return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
	}
	data, err := readPrivateCloudMetadataFile(paths, pendingCloudNotificationClaimOperationPath(paths, opID), "notification claim operation")
	if errors.Is(err, os.ErrNotExist) {
		return CloudNotificationClaimOperation{}, false, nil
	}
	if err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	var op CloudNotificationClaimOperation
	if err := json.Unmarshal(data, &op); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if op.OpID != opID || op.State != operationJournalStatePending {
		return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
	}
	return op, true, nil
}

func ReadPendingCloudNotificationClaimOperationWithRetry(paths Paths, opID string) (CloudNotificationClaimOperation, bool, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		op, ok, err := ReadPendingCloudNotificationClaimOperation(paths, opID)
		if err == nil || !errors.Is(err, ErrCapabilityFileUnsafe) {
			return op, ok, err
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return CloudNotificationClaimOperation{}, false, lastErr
}

func ReadDoneCloudNotificationClaimOperation(paths Paths, opID string) (CloudNotificationClaimOperation, bool, error) {
	if !LocalOperationIDRE.MatchString(opID) {
		return CloudNotificationClaimOperation{}, false, errors.New("invalid notification claim operation")
	}
	data, err := readPrivateCloudMetadataFile(paths, doneCloudNotificationClaimOperationPath(paths, opID), "done notification claim operation")
	if errors.Is(err, os.ErrNotExist) {
		return CloudNotificationClaimOperation{}, false, nil
	}
	if err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	var op CloudNotificationClaimOperation
	if err := json.Unmarshal(data, &op); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return CloudNotificationClaimOperation{}, false, err
	}
	if op.OpID != opID || op.State != operationJournalStateDone {
		return CloudNotificationClaimOperation{}, false, errors.New("notification claim operation mismatch")
	}
	return op, true, nil
}

func CompleteCloudNotificationClaimOperation(paths Paths, op CloudNotificationClaimOperation, now time.Time) error {
	if err := validateCloudNotificationClaimOperation(op); err != nil {
		return err
	}
	if existing, ok, err := ReadPendingCloudNotificationClaimOperationWithRetry(paths, op.OpID); err != nil {
		return err
	} else if ok && sameCloudNotificationClaimOperation(existing, op) {
		op = existing
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	op.State = operationJournalStateDone
	op.UpdatedAt = busTime(now.UTC())
	op.LastError = ""
	data, err := json.MarshalIndent(op, "", "  ")
	if err != nil {
		return err
	}
	if err := WritePrivateFileAtomic(doneCloudNotificationClaimOperationPath(paths, op.OpID), append(data, '\n'), 0o600); err != nil {
		return err
	}
	pendingPath := pendingCloudNotificationClaimOperationPath(paths, op.OpID)
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(pendingPath))
}

func validateCloudNotificationClaimOperation(op CloudNotificationClaimOperation) error {
	if !LocalOperationIDRE.MatchString(op.OpID) || op.IdempotencyKey != op.OpID || !isCloudNotificationClaimOperation(op.Operation) {
		return errors.New("invalid notification claim operation")
	}
	if !ProfileRE.MatchString(op.Profile) || !LocalMessageIDRE.MatchString(op.LocalMessageID) || op.PrivateMetadataRef != privateCloudMessageRef(op.Profile, op.LocalMessageID) {
		return errors.New("invalid notification claim operation")
	}
	if op.CredentialProfile != "" && !ProfileRE.MatchString(op.CredentialProfile) {
		return errors.New("invalid notification claim operation")
	}
	if !isSafeCloudID("claim", op.ClaimID) || !isSafeCloudID("notification", op.NotificationID) {
		return errors.New("invalid notification claim operation")
	}
	if op.ClaimHolder != "" && !isLocalClaimHolder(op.ClaimHolder) {
		return errors.New("invalid notification claim operation")
	}
	if op.State != operationJournalStatePending && op.State != operationJournalStateDone {
		return errors.New("invalid notification claim operation")
	}
	if op.Operation == "renew" {
		if op.LeaseTTLMS < 1000 || op.LeaseTTLMS > 3_600_000 {
			return errors.New("invalid notification claim operation")
		}
	} else if op.LeaseTTLMS != 0 {
		return errors.New("invalid notification claim operation")
	}
	if err := validateCloudOperationReleaseReason(op.Operation, op.ReleaseReason); err != nil {
		return err
	}
	if op.Attempts < 0 || op.Attempts > 1_000_000 || len(op.LastError) > 512 || containsSecretValue(op.LastError) || strings.ContainsAny(op.LastError, "\r\n\x00") {
		return errors.New("invalid notification claim operation")
	}
	if _, err := time.Parse(time.RFC3339Nano, op.CreatedAt); err != nil {
		return errors.New("invalid notification claim operation")
	}
	if _, err := time.Parse(time.RFC3339Nano, op.UpdatedAt); err != nil {
		return errors.New("invalid notification claim operation")
	}
	return nil
}

func sameCloudNotificationClaimOperation(a CloudNotificationClaimOperation, b CloudNotificationClaimOperation) bool {
	return a.OpID == b.OpID &&
		a.IdempotencyKey == b.IdempotencyKey &&
		a.Operation == b.Operation &&
		a.Profile == b.Profile &&
		a.CredentialProfile == b.CredentialProfile &&
		a.LocalMessageID == b.LocalMessageID &&
		a.PrivateMetadataRef == b.PrivateMetadataRef &&
		a.ClaimID == b.ClaimID &&
		a.NotificationID == b.NotificationID &&
		a.ClaimHolder == b.ClaimHolder &&
		a.LeaseTTLMS == b.LeaseTTLMS &&
		a.ReleaseReason == b.ReleaseReason
}

func sameCloudNotificationClaimOperationSelector(op CloudNotificationClaimOperation, operation string, profile string, messageID string, claimID string, notificationID string, claimHolder string, leaseTTL time.Duration, releaseReason string) bool {
	return op.Operation == operation &&
		op.Profile == profile &&
		op.LocalMessageID == messageID &&
		op.PrivateMetadataRef == privateCloudMessageRef(profile, messageID) &&
		op.ClaimID == claimID &&
		op.NotificationID == notificationID &&
		op.ClaimHolder == claimHolder &&
		op.LeaseTTLMS == durationMilliseconds(leaseTTL) &&
		releaseReasonMatchesExactly(op, releaseReason)
}

func isLocalClaimHolder(holder string) bool {
	if strings.HasPrefix(holder, "owner:") {
		return ProfileRE.MatchString(strings.TrimPrefix(holder, "owner:"))
	}
	if strings.HasPrefix(holder, "session:") {
		parts := strings.Split(holder, ":")
		return len(parts) == 3 && LocalSessionIDRE.MatchString(parts[1]) && LocalSessionGenerationIDRE.MatchString(parts[2])
	}
	return false
}

func isCloudNotificationClaimOperation(operation string) bool {
	return operation == "renew" || operation == "ack" || operation == "release"
}

func validateCloudOperationReleaseReason(operation string, reason string) error {
	if reason == "" {
		return nil
	}
	if operation != "release" || len(reason) > 512 || strings.ContainsAny(reason, "\r\n\x00") || containsSecretValue(reason) {
		return errors.New("invalid notification claim operation")
	}
	return nil
}

func releaseReasonMatchesExactly(op CloudNotificationClaimOperation, requestedReason string) bool {
	if op.Operation != "release" {
		return requestedReason == "" && op.ReleaseReason == ""
	}
	return op.ReleaseReason == requestedReason
}

func releaseReasonLookupCompatible(op CloudNotificationClaimOperation, requestedReason string) bool {
	if op.Operation != "release" {
		return requestedReason == "" && op.ReleaseReason == ""
	}
	if requestedReason == "" {
		return op.ReleaseReason != declinedDuplicateReleaseReason
	}
	return op.ReleaseReason == requestedReason
}

func privateCloudMessageRef(profile string, messageID string) string {
	return filepath.ToSlash(filepath.Join("private", profile, messageID+".json"))
}

func validateCloudNotificationWaitOperation(op CloudNotificationWaitOperation) error {
	if !LocalOperationIDRE.MatchString(op.OpID) || op.Kind != cloudNotificationWaitOperationKind || !ProfileRE.MatchString(op.Profile) {
		return errors.New("invalid notification wait operation")
	}
	if op.BotName != "" && !BotNameRE.MatchString(op.BotName) {
		return errors.New("invalid notification wait operation")
	}
	if !sameCloudNotificationKinds(op.Kinds, canonicalCloudNotificationKinds(op.Kinds)) {
		return errors.New("invalid notification wait operation")
	}
	for _, kind := range op.Kinds {
		if !isMessageKind(kind) {
			return errors.New("invalid notification wait operation")
		}
	}
	if op.TimeoutMS < 0 || op.TimeoutMS > 65_000 || op.LeaseTTLMS < 1000 || op.LeaseTTLMS > 3_600_000 || op.Attempts < 0 || !isSafeNotificationLeaseHolder(op.LeaseHolder) {
		return errors.New("invalid notification wait operation")
	}
	if _, err := time.Parse(time.RFC3339Nano, op.CreatedAt); err != nil {
		return errors.New("invalid notification wait operation")
	}
	if op.Attempts == 0 {
		if op.LastAttempt != "" {
			return errors.New("invalid notification wait operation")
		}
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, op.LastAttempt); err != nil {
		return errors.New("invalid notification wait operation")
	}
	return nil
}

func shouldRetireCloudNotificationWaitOperation(op CloudNotificationWaitOperation, now time.Time) bool {
	createdAt, err := time.Parse(time.RFC3339Nano, op.CreatedAt)
	if err != nil {
		return true
	}
	return now.Sub(createdAt) > maxCloudNotificationWaitOperationAge
}

func pendingCloudNotificationWaitOperationPath(paths Paths, profile string, botName string, kinds []string) string {
	name := profile
	if botName != "" {
		name += "__" + botName
	}
	if key := cloudNotificationWaitKindsPathKey(kinds); key != "" {
		name += "__k_" + key
	}
	return filepath.Join(paths.OpsPending, "notification-wait", name+".json")
}

func doneCloudNotificationWaitOperationPath(paths Paths, opID string) string {
	return filepath.Join(paths.OpsDone, "notification-wait", opID+".json")
}

func canonicalCloudNotificationKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return nil
	}
	copied := append([]string(nil), kinds...)
	sort.Strings(copied)
	out := copied[:0]
	for _, kind := range copied {
		if len(out) == 0 || out[len(out)-1] != kind {
			out = append(out, kind)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return append([]string(nil), out...)
}

func sameCloudNotificationKinds(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloudNotificationWaitKindsPathKey(kinds []string) string {
	canonicalKinds := canonicalCloudNotificationKinds(kinds)
	if len(canonicalKinds) == 0 {
		return ""
	}
	sum := sha1.Sum([]byte(strings.Join(canonicalKinds, "\x00")))
	return hex.EncodeToString(sum[:])
}

func pendingCloudNotificationClaimOperationPath(paths Paths, opID string) string {
	return filepath.Join(paths.OpsPending, opID+".json")
}

func doneCloudNotificationClaimOperationPath(paths Paths, opID string) string {
	return filepath.Join(paths.OpsDone, opID+".json")
}
