package commentbus

import (
	"context"
	"errors"
	"time"
)

type RepairFilter struct {
	MessageID string
	OpID      string
}

func (filter RepairFilter) matchesOperation(op CloudNotificationClaimOperation) bool {
	if filter.MessageID != "" && op.LocalMessageID != filter.MessageID {
		return false
	}
	if filter.OpID != "" && op.OpID != filter.OpID {
		return false
	}
	return true
}

func (filter RepairFilter) matchesMessage(message MessageEnvelope) bool {
	if filter.OpID != "" {
		return false
	}
	if filter.MessageID != "" && message.ID != filter.MessageID {
		return false
	}
	return true
}

func BusRepairDryRun(ctx context.Context, paths Paths, store *Store) ([]RepairAction, error) {
	return BusRepairDryRunWithFilter(ctx, paths, store, RepairFilter{})
}

func BusRepairDryRunWithFilter(ctx context.Context, paths Paths, store *Store, filter RepairFilter) ([]RepairAction, error) {
	actions, err := store.RepairDryRun(ctx)
	if err != nil {
		return nil, err
	}
	journal, err := inspectPendingCloudClaimOperations(ctx, paths, store, filter)
	if err != nil {
		return nil, err
	}
	actions = append(actions, journal...)
	claims, err := inspectClaimedMessages(ctx, paths, store, time.Now().UTC(), filter, blockedRepairMessages(journal), func(message MessageEnvelope, now time.Time) (string, bool) {
		return claimAbandonmentReason(paths, message, now)
	})
	if err != nil {
		return nil, err
	}
	actions = append(actions, claims...)
	return actions, nil
}

func (d *Daemon) repairBus(ctx context.Context, dryRun bool, filter RepairFilter) ([]RepairAction, error) {
	d.lockSessionForContext(ctx)
	defer d.sessionMu.Unlock()
	d.lockBusForContext(ctx)
	defer d.busMu.Unlock()
	now := time.Now().UTC()
	var actions []RepairAction
	stageName := "repair.reconcile"
	if dryRun {
		stageName = "repair.inspect"
	}
	err := d.runSocketStageForContext(ctx, stageName, func() error {
		var stageErr error
		if dryRun {
			actions, stageErr = d.inspectRepairLocked(ctx, now, filter)
		} else {
			actions, stageErr = d.reconcileLocked(ctx, now, filter, true)
		}
		return stageErr
	})
	return actions, err
}

func (d *Daemon) inspectRepairLocked(ctx context.Context, now time.Time, filter RepairFilter) ([]RepairAction, error) {
	actions, err := d.store.RepairDryRun(ctx)
	if err != nil {
		return nil, err
	}
	journal, err := inspectPendingCloudClaimOperations(ctx, d.paths, d.store, filter)
	if err != nil {
		return nil, err
	}
	actions = append(actions, journal...)
	claims, err := inspectClaimedMessages(ctx, d.paths, d.store, now, filter, blockedRepairMessages(journal), func(message MessageEnvelope, now time.Time) (string, bool) {
		reason, shouldRepair, _ := d.claimAbandonmentReasonLocked(message, now, false, false)
		return reason, shouldRepair
	})
	if err != nil {
		return nil, err
	}
	actions = append(actions, claims...)
	return actions, nil
}

func (d *Daemon) reconcileStartup(ctx context.Context) ([]RepairAction, error) {
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("repair.startup", "", ""))
	d.lockSessionForContext(ctx)
	defer d.sessionMu.Unlock()
	d.lockBusForContext(ctx)
	defer d.busMu.Unlock()
	var actions []RepairAction
	err := d.runSocketStageForContext(ctx, "repair.startup_reconcile", func() error {
		var stageErr error
		actions, stageErr = d.reconcileStartupLocked(ctx, time.Now().UTC())
		return stageErr
	})
	return actions, err
}

func (d *Daemon) reconcileStartupLocked(ctx context.Context, now time.Time) ([]RepairAction, error) {
	return d.reconcileLocked(ctx, now, RepairFilter{}, false)
}

func (d *Daemon) reconcileLocked(ctx context.Context, now time.Time, filter RepairFilter, failOnInspectError bool) ([]RepairAction, error) {
	ops, err := ListPendingCloudNotificationClaimOperations(d.paths)
	if err != nil {
		return nil, err
	}
	actions := make([]RepairAction, 0, len(ops))
	blockedMessages := map[string]struct{}{}
	for _, op := range ops {
		if !filter.matchesOperation(op) {
			continue
		}
		action, err := d.replayPendingCloudClaimOperationLocked(ctx, op, now)
		if err != nil {
			return actions, err
		}
		if action.Action != "" {
			actions = append(actions, action)
			if action.Action == "replay_op" && action.MessageID != "" && !isDeclinedDuplicateReleaseOperation(op) {
				blockedMessages[action.MessageID] = struct{}{}
			}
		}
	}
	claimActions, err := d.reconcileClaimedMessagesLocked(ctx, now, filter, blockedMessages, failOnInspectError)
	if err != nil {
		return actions, err
	}
	actions = append(actions, claimActions...)
	return actions, nil
}

func inspectPendingCloudClaimOperations(ctx context.Context, paths Paths, store *Store, filter RepairFilter) ([]RepairAction, error) {
	ops, err := ListPendingCloudNotificationClaimOperations(paths)
	if err != nil {
		return nil, err
	}
	actions := make([]RepairAction, 0, len(ops))
	for _, op := range ops {
		if !filter.matchesOperation(op) {
			continue
		}
		action := RepairAction{
			Action:    "replay_op",
			MessageID: op.LocalMessageID,
			OpID:      op.OpID,
			FromState: op.State,
			Reason:    "pending notification operation requires replay",
		}
		if done, ok, err := ReadDoneCloudNotificationClaimOperation(paths, op.OpID); err != nil {
			return nil, err
		} else if ok && sameCloudNotificationClaimOperation(done, op) {
			action.Action = "complete_op"
			action.ToState = operationJournalStateDone
			action.Reason = "done operation archive exists for pending operation"
			actions = append(actions, action)
			continue
		}
		if isDeclinedDuplicateReleaseOperation(op) {
			action.Reason = "declined duplicate notification lease requires release"
			actions = append(actions, action)
			continue
		}
		metadata, metadataErr := ReadPrivateCloudMessageMetadata(paths, op.Profile, op.LocalMessageID)
		if metadataErr != nil {
			if op.Operation == "renew" {
				action.Action = "complete_op"
				action.ToState = "removed"
				action.Reason = "pending renew cannot replay without private metadata"
			} else if op.Operation == "ack" || op.Operation == "release" {
				message, messageErr := store.GetInboxMessage(ctx, op.Profile, op.LocalMessageID)
				if errors.Is(messageErr, ErrMessageNotFound) {
					action.Reason = "local message is missing"
				} else if messageErr != nil {
					return nil, messageErr
				} else if cloudClaimOperationCompletedHolderMismatch(message, op) {
					action.Reason = "local delivery claim holder does not match pending operation"
				} else if replayedCloudOperationMatchesLocalState(message, op) {
					action.Action = "complete_op"
					action.ToState = operationJournalStateDone
					action.Reason = "local delivery already reflects operation"
				} else if message.Source != "comment.io" || message.Delivery.State != "claimed" || message.Delivery.ClaimHolder == nil {
					action.Reason = "local delivery cannot safely replay operation"
				} else if cloudClaimOperationHolderMismatch(message, op) {
					action.Reason = "local delivery claim holder does not match pending operation"
				} else {
					action.Reason = "terminal notification operation can replay from journal"
				}
			} else {
				action.Reason = "private cloud metadata cannot be read"
			}
			actions = append(actions, action)
			continue
		}
		metadataMatchesOperation := metadata.ClaimID == op.ClaimID && metadata.NotificationID == op.NotificationID
		retargetedAck := cloudAckOperationMatchesRetargetedNotification(op, metadata)
		if !metadataMatchesOperation && !retargetedAck {
			action.Reason = "private cloud metadata no longer matches pending operation"
			actions = append(actions, action)
			continue
		}
		message, storeErr := store.GetInboxMessage(ctx, op.Profile, op.LocalMessageID)
		if errors.Is(storeErr, ErrMessageNotFound) {
			action.Reason = "local message is missing"
			actions = append(actions, action)
			continue
		}
		if storeErr != nil {
			return nil, storeErr
		}
		if cloudClaimOperationCompletedHolderMismatch(message, op) {
			action.Reason = "local delivery claim holder does not match pending operation"
		} else if replayedCloudOperationMatchesLocalState(message, op) {
			action.Action = "complete_op"
			action.ToState = operationJournalStateDone
			action.Reason = "local delivery already reflects operation"
		} else if fresher, err := hasFresherPendingCloudAckOperation(paths, op); err != nil {
			return nil, err
		} else if fresher {
			action.Reason = "newer duplicate notification ack is pending"
		} else if message.Source != "comment.io" || message.Delivery.State != "claimed" || message.Delivery.ClaimHolder == nil {
			action.Reason = "local delivery cannot safely replay operation"
		} else if cloudClaimOperationHolderMismatch(message, op) {
			action.Reason = "local delivery claim holder does not match pending operation"
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func inspectClaimedMessages(ctx context.Context, paths Paths, store *Store, now time.Time, filter RepairFilter, blockedMessages map[string]struct{}, reasonFn func(MessageEnvelope, time.Time) (string, bool)) ([]RepairAction, error) {
	messages, err := store.ListClaimedMessages(ctx)
	if err != nil {
		return nil, err
	}
	var actions []RepairAction
	for _, message := range messages {
		if !filter.matchesMessage(message) {
			continue
		}
		if _, blocked := blockedMessages[message.ID]; blocked {
			continue
		}
		reason, shouldRepair := defaultClaimAbandonmentReason(message, now)
		if reasonFn != nil {
			reason, shouldRepair = reasonFn(message, now)
		}
		if !shouldRepair {
			continue
		}
		action := RepairAction{
			Action:    "requeue_message",
			MessageID: message.ID,
			FromState: message.Delivery.State,
			Reason:    reason,
		}
		if message.Source == "comment.io" {
			if cloudClaimCanRequeueLocally(reason) {
				action.ToState = "unclaimed"
				actions = append(actions, action)
				continue
			}
			action.Action = "replay_op"
			action.Reason = "cloud claim needs release during startup reconciliation"
			if _, err := ReadPrivateCloudMessageMetadata(paths, message.Profile, message.ID); err != nil {
				action.Action = "release_message"
				action.ToState = "released"
				action.Reason = "private_metadata_unavailable"
			}
		} else {
			action.ToState = "unclaimed"
		}
		actions = append(actions, action)
	}
	return actions, nil
}

func (d *Daemon) reconcileClaimedMessagesLocked(ctx context.Context, now time.Time, filter RepairFilter, blockedMessages map[string]struct{}, failOnInspectError bool) ([]RepairAction, error) {
	messages, err := d.store.ListClaimedMessages(ctx)
	if err != nil {
		return nil, err
	}
	var actions []RepairAction
	for _, message := range messages {
		if !filter.matchesMessage(message) {
			continue
		}
		if _, blocked := blockedMessages[message.ID]; blocked {
			continue
		}
		reason, shouldRepair, reasonErr := d.claimAbandonmentReasonLocked(message, now, failOnInspectError, true)
		if reasonErr != nil {
			return actions, reasonErr
		}
		if !shouldRepair {
			continue
		}
		if cloudClaimCanRequeueLocally(reason) {
			if err := d.markDeliverySessionRecordStaleLocked(message, reason); err != nil {
				return actions, err
			}
			if message.Source == "comment.io" {
				updated, err := d.store.RequeueCloudMessageLocally(ctx, message.Profile, message.ID, stringValue(message.Delivery.ClaimHolder), reason, now)
				if err != nil {
					return actions, err
				}
				actions = append(actions, RepairAction{
					Action:    "requeue_message",
					MessageID: message.ID,
					FromState: message.Delivery.State,
					ToState:   updated.Delivery.State,
					Reason:    reason,
				})
				continue
			}
		}
		if message.Source == "comment.io" {
			action, err := d.releaseAbandonedCloudClaimLocked(ctx, message, reason, now)
			if err != nil {
				return actions, err
			}
			if action.Action != "" {
				actions = append(actions, action)
			}
			continue
		}
		updated, err := d.store.RequeueClaimedMessage(ctx, message.Profile, message.ID, reason, now)
		if err != nil {
			return actions, err
		}
		actions = append(actions, RepairAction{
			Action:    "requeue_message",
			MessageID: message.ID,
			FromState: message.Delivery.State,
			ToState:   updated.Delivery.State,
			Reason:    reason,
		})
	}
	return actions, nil
}

func cloudClaimCanRequeueLocally(reason string) bool {
	return reason == "tmux_session_missing" || reason == "runtime_not_running" || reason == "runtime_untrusted" || reason == "session_bot_mismatch"
}

func (d *Daemon) inspectLiveTmuxSessionLocked(record SessionRecord, recoverPane bool) (SessionRecord, bool, *SocketError) {
	if record.SessionName == "" {
		return record, false, nil
	}
	if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
		return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session name", false)
	}
	if record.PaneTarget != "" {
		if err := validatePaneTargetForHost(record.Host, record.PaneTarget); err != nil {
			return SessionRecord{}, false, socketError("VALIDATION_ERROR", "invalid session target", false)
		}
	}
	controller := d.controllerForSession(record)
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		if reader, ok := controller.(bmuxStatusReader); ok {
			status, err := reader.BmuxStatus(context.Background(), record.SessionName)
			if errors.Is(err, ErrTmuxSessionMissing) {
				return record, false, nil
			}
			if err != nil {
				return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not inspect session", true)
			}
			if !recoverPane {
				return record, true, nil
			}
			return record, status.childAlive, nil
		}
	}
	live, err := controller.HasSession(context.Background(), record.SessionName)
	if err != nil {
		return SessionRecord{}, false, socketError("UPSTREAM_ERROR", "could not inspect session", true)
	}
	if !live || !recoverPane {
		return record, live, nil
	}
	return record, true, nil
}

func (d *Daemon) markDeliverySessionRecordStaleLocked(message MessageEnvelope, reason string) error {
	if message.Delivery.SessionID == nil {
		return nil
	}
	record, err := ReadSessionRecord(d.paths, *message.Delivery.SessionID)
	if err != nil {
		return nil
	}
	if record.State != "stale" {
		record.State = "stale"
		if err := WriteSessionRecord(d.paths, record); err != nil {
			return err
		}
	}
	if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, reason); cleanupErr != nil {
		return errors.New(cleanupErr.Message)
	}
	return nil
}

func (d *Daemon) releaseAbandonedCloudClaimLocked(ctx context.Context, message MessageEnvelope, reason string, now time.Time) (RepairAction, error) {
	metadata, err := ReadPrivateCloudMessageMetadata(d.paths, message.Profile, message.ID)
	if err != nil {
		updated, quarantineErr := d.quarantineCloudMessageForMissingMetadata(ctx, message.Profile, message.ID, now)
		if quarantineErr != nil {
			return RepairAction{}, quarantineErr
		}
		return RepairAction{
			Action:    "release_message",
			MessageID: message.ID,
			FromState: message.Delivery.State,
			ToState:   updated.Delivery.State,
			Reason:    "private_metadata_unavailable",
		}, nil
	}
	op, done, err := BeginCloudNotificationClaimOperationForHolder(d.paths, "release", message.Profile, message.ID, metadata.ClaimID, metadata.NotificationID, stringValue(message.Delivery.ClaimHolder), "", 0, now, reason)
	if err != nil {
		return RepairAction{}, err
	}
	if done {
		if replayedCloudOperationMatchesLocalState(message, op) {
			if err := CompleteCloudNotificationClaimOperation(d.paths, op, now); err != nil {
				return RepairAction{}, err
			}
			return RepairAction{Action: "complete_op", MessageID: message.ID, OpID: op.OpID, FromState: operationJournalStatePending, ToState: operationJournalStateDone, Reason: reason}, nil
		}
	}
	action, err := d.replayPendingCloudClaimOperationLocked(ctx, op, now)
	if action.Reason == "pending notification operation replayed" {
		action.Reason = reason
	}
	return action, err
}

func (d *Daemon) replayPendingCloudClaimOperationLocked(ctx context.Context, op CloudNotificationClaimOperation, now time.Time) (RepairAction, error) {
	action := RepairAction{
		Action:    "replay_op",
		MessageID: op.LocalMessageID,
		OpID:      op.OpID,
		FromState: op.State,
		Reason:    "pending notification operation requires replay",
	}
	if done, ok, err := ReadDoneCloudNotificationClaimOperation(d.paths, op.OpID); err != nil {
		return RepairAction{}, err
	} else if ok && sameCloudNotificationClaimOperation(done, op) {
		if err := CompleteCloudNotificationClaimOperation(d.paths, op, now); err != nil {
			return RepairAction{}, err
		}
		action.Action = "complete_op"
		action.ToState = operationJournalStateDone
		action.Reason = "done operation archive exists for pending operation"
		return action, nil
	}
	declinedDuplicateRelease := isDeclinedDuplicateReleaseOperation(op)
	metadata, err := ReadPrivateCloudMessageMetadata(d.paths, op.Profile, op.LocalMessageID)
	if err != nil && op.Operation == "renew" {
		if abandonErr := AbandonCloudNotificationClaimOperation(d.paths, op); abandonErr != nil {
			return RepairAction{}, abandonErr
		}
		action.Action = "complete_op"
		action.ToState = "removed"
		action.Reason = "pending renew cannot replay without private metadata"
		return action, nil
	}
	metadataMatchesOperation := err == nil && metadata.ClaimID == op.ClaimID && metadata.NotificationID == op.NotificationID
	retargetedAck := err == nil && cloudAckOperationMatchesRetargetedNotification(op, metadata)
	if err == nil && !declinedDuplicateRelease && !metadataMatchesOperation && !retargetedAck {
		action.Reason = "private cloud metadata no longer matches pending operation"
		return action, nil
	}
	if declinedDuplicateRelease {
		if d.notificationClient == nil {
			action.Reason = "notification client is not configured"
			return action, nil
		}
		profileConfig, ok := d.cloudNotificationProfile(cloudNotificationClaimOperationCredentialProfile(op))
		if !ok {
			action.Reason = "notification profile is not loaded"
			return action, nil
		}
		attempted, err := RecordCloudNotificationClaimOperationAttempt(d.paths, op, now)
		if err != nil {
			return RepairAction{}, err
		}
		return d.applyDeclinedDuplicateReleaseOperationLocked(ctx, attempted, profileConfig, now)
	}
	message, err := d.store.GetInboxMessage(ctx, op.Profile, op.LocalMessageID)
	if errors.Is(err, ErrMessageNotFound) {
		action.Reason = "local message is missing"
		return action, nil
	}
	if err != nil {
		return RepairAction{}, err
	}
	if cloudClaimOperationCompletedHolderMismatch(message, op) {
		action.Reason = "local delivery claim holder does not match pending operation"
		return action, nil
	}
	if replayedCloudOperationMatchesLocalState(message, op) {
		if err := CompleteCloudNotificationClaimOperation(d.paths, op, now); err != nil {
			return RepairAction{}, err
		}
		action.Action = "complete_op"
		action.ToState = operationJournalStateDone
		action.Reason = "local delivery already reflects operation"
		return action, nil
	}
	if fresher, err := hasFresherPendingCloudAckOperation(d.paths, op); err != nil {
		return RepairAction{}, err
	} else if fresher {
		action.Reason = "newer duplicate notification ack is pending"
		return action, nil
	}
	if message.Source != "comment.io" || message.Delivery.State != "claimed" || message.Delivery.ClaimHolder == nil {
		action.Reason = "local delivery cannot safely replay operation"
		return action, nil
	}
	if cloudClaimOperationHolderMismatch(message, op) {
		action.Reason = "local delivery claim holder does not match pending operation"
		return action, nil
	}
	if d.notificationClient == nil {
		action.Reason = "notification client is not configured"
		return action, nil
	}
	profileConfig, ok := d.cloudNotificationProfile(cloudNotificationClaimOperationCredentialProfile(op))
	if !ok {
		action.Reason = "notification profile is not loaded"
		return action, nil
	}
	attempted, err := RecordCloudNotificationClaimOperationAttempt(d.paths, op, now)
	if err != nil {
		return RepairAction{}, err
	}
	if replayAction, err := d.applyCloudClaimOperationLocked(ctx, attempted, profileConfig, metadata, message, now); err != nil {
		return RepairAction{}, err
	} else if replayAction.Action != "" {
		if op.Operation == "ack" || op.Operation == "release" {
			if replayAction.Action == "complete_op" && replayAction.ToState == "removed" && message.Source == "comment.io" && message.Delivery.State == "claimed" && message.Delivery.ClaimHolder != nil {
				if reason, shouldRepair, reasonErr := d.claimAbandonmentReasonLocked(message, now, true, true); reasonErr != nil {
					return RepairAction{}, reasonErr
				} else if shouldRepair {
					if err := d.markDeliverySessionRecordStaleLocked(message, reason); err != nil {
						return RepairAction{}, err
					}
					if leaseExpired(message.Delivery.LeaseExpiresAt, now) {
						updated, err := d.store.ReleaseCloudMessage(ctx, message.Profile, message.ID, stringValue(message.Delivery.ClaimHolder), reason, now)
						if err != nil {
							return RepairAction{}, err
						}
						replayAction.Action = "release_message"
						replayAction.FromState = message.Delivery.State
						replayAction.ToState = updated.Delivery.State
						replayAction.Reason = "terminal notification operation failed; expired local claim cleared"
					} else {
						updated, err := d.store.RequeueCloudMessageLocally(ctx, message.Profile, message.ID, stringValue(message.Delivery.ClaimHolder), reason, now)
						if err != nil {
							return RepairAction{}, err
						}
						replayAction.Action = "requeue_message"
						replayAction.FromState = message.Delivery.State
						replayAction.ToState = updated.Delivery.State
						replayAction.Reason = "terminal notification operation failed; local claim requeued"
					}
				}
			}
		}
		return replayAction, nil
	}
	action.Reason = "pending notification operation replayed"
	return action, nil
}

func isDeclinedDuplicateReleaseOperation(op CloudNotificationClaimOperation) bool {
	return op.Operation == "release" && op.ReleaseReason == declinedDuplicateReleaseReason
}

func (d *Daemon) applyDeclinedDuplicateReleaseOperationLocked(ctx context.Context, op CloudNotificationClaimOperation, profileConfig AgentProfile, now time.Time) (RepairAction, error) {
	action := RepairAction{
		Action:    "complete_op",
		MessageID: op.LocalMessageID,
		OpID:      op.OpID,
		FromState: operationJournalStatePending,
		ToState:   operationJournalStateDone,
		Reason:    "declined duplicate notification lease released",
	}
	mutation, err := d.notificationClient.ReleaseNotification(ctx, profileConfig, op.ClaimID, op.OpID)
	if err != nil {
		return d.repairActionForCloudMutationError(op, err, now)
	}
	if mutation == nil || mutation.ClaimID != op.ClaimID || mutation.NotificationID != op.NotificationID {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, errors.New("notification claim response mismatch"), now)
		return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "notification operation response mismatch"}, nil
	}
	if err := CompleteCloudNotificationClaimOperation(d.paths, op, now); err != nil {
		return RepairAction{}, err
	}
	return action, nil
}

func (d *Daemon) applyCloudClaimOperationLocked(ctx context.Context, op CloudNotificationClaimOperation, profileConfig AgentProfile, metadata PrivateCloudMessageMetadata, message MessageEnvelope, now time.Time) (RepairAction, error) {
	action := RepairAction{
		Action:    "complete_op",
		MessageID: op.LocalMessageID,
		OpID:      op.OpID,
		FromState: operationJournalStatePending,
		ToState:   operationJournalStateDone,
		Reason:    "pending notification operation replayed",
	}
	claimHolder := stringValue(message.Delivery.ClaimHolder)
	if op.ClaimHolder != "" && op.ClaimHolder != claimHolder {
		return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "local delivery claim holder does not match pending operation"}, nil
	}
	switch op.Operation {
	case "renew":
		ttl := time.Duration(op.LeaseTTLMS) * time.Millisecond
		lease, err := d.notificationClient.RenewNotification(ctx, profileConfig, op.ClaimID, ttl, op.OpID)
		if err != nil {
			return d.repairActionForCloudMutationError(op, err, now)
		}
		if lease == nil || lease.ClaimID != op.ClaimID || lease.NotificationID != op.NotificationID {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, errors.New("notification renew response mismatch"), now)
			return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "notification operation response mismatch"}, nil
		}
		metadata.LeaseExpiresAt = lease.LeaseExpiresAt
		if lease.ClaimedAt != "" {
			metadata.ClaimedAt = lease.ClaimedAt
		}
		if err := WritePrivateCloudMessageMetadata(d.paths, metadata); err != nil {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, err, now)
			return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "private cloud metadata could not be updated"}, nil
		}
		if _, err := d.store.RenewCloudMessage(ctx, op.Profile, op.LocalMessageID, claimHolder, lease.LeaseExpiresAt, now); err != nil {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, err, now)
			return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "local delivery could not be updated"}, nil
		}
	case "ack", "release":
		var mutation *CloudNotificationClaimMutation
		var err error
		if op.Operation == "ack" {
			mutation, err = d.notificationClient.AckNotification(ctx, profileConfig, op.ClaimID, op.OpID)
		} else {
			mutation, err = d.notificationClient.ReleaseNotification(ctx, profileConfig, op.ClaimID, op.OpID)
		}
		if err != nil {
			return d.repairActionForCloudMutationError(op, err, now)
		}
		if mutation == nil || mutation.ClaimID != op.ClaimID || mutation.NotificationID != op.NotificationID {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, errors.New("notification claim response mismatch"), now)
			return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "notification operation response mismatch"}, nil
		}
		var storeErr error
		if op.Operation == "ack" {
			_, storeErr = d.store.AckCloudMessage(ctx, op.Profile, op.LocalMessageID, claimHolder, now)
		} else {
			_, storeErr = d.store.ReleaseCloudMessage(ctx, op.Profile, op.LocalMessageID, claimHolder, op.ReleaseReason, now)
		}
		if storeErr != nil {
			_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, storeErr, now)
			return RepairAction{Action: "replay_op", MessageID: op.LocalMessageID, OpID: op.OpID, FromState: op.State, Reason: "local delivery could not be updated"}, nil
		}
	default:
		return RepairAction{}, errors.New("invalid notification claim operation")
	}
	if err := CompleteCloudNotificationClaimOperation(d.paths, op, now); err != nil {
		return RepairAction{}, err
	}
	return action, nil
}

func cloudClaimOperationHolderMismatch(message MessageEnvelope, op CloudNotificationClaimOperation) bool {
	return op.ClaimHolder != "" && op.ClaimHolder != stringValue(message.Delivery.ClaimHolder)
}

func cloudNotificationClaimOperationCredentialProfile(op CloudNotificationClaimOperation) string {
	if op.CredentialProfile != "" {
		return op.CredentialProfile
	}
	return op.Profile
}

func cloudAckOperationMatchesRetargetedNotification(op CloudNotificationClaimOperation, metadata PrivateCloudMessageMetadata) bool {
	return op.Operation == "ack" &&
		metadata.Source == "comment.io" &&
		metadata.Profile == op.Profile &&
		metadata.LocalMessageID == op.LocalMessageID &&
		metadata.NotificationID == op.NotificationID &&
		metadata.ClaimID != op.ClaimID
}

func hasFresherPendingCloudAckOperation(paths Paths, op CloudNotificationClaimOperation) (bool, error) {
	if op.Operation != "ack" {
		return false, nil
	}
	ops, err := ListPendingCloudNotificationClaimOperationsForLocalMessage(paths, op.Profile, op.LocalMessageID)
	if err != nil {
		return false, err
	}
	for _, candidate := range ops {
		if candidate.OpID == op.OpID || candidate.Operation != "ack" || candidate.NotificationID != op.NotificationID {
			continue
		}
		if cloudNotificationClaimOperationCreatedAfter(candidate, op) {
			return true, nil
		}
	}
	return false, nil
}

func cloudNotificationClaimOperationCreatedAfter(candidate CloudNotificationClaimOperation, op CloudNotificationClaimOperation) bool {
	candidateCreatedAt, candidateErr := time.Parse(time.RFC3339Nano, candidate.CreatedAt)
	opCreatedAt, opErr := time.Parse(time.RFC3339Nano, op.CreatedAt)
	if candidateErr == nil && opErr == nil {
		if !candidateCreatedAt.Equal(opCreatedAt) {
			return candidateCreatedAt.After(opCreatedAt)
		}
	}
	return candidate.OpID > op.OpID
}

func (d *Daemon) repairActionForCloudMutationError(op CloudNotificationClaimOperation, err error, now time.Time) (RepairAction, error) {
	action := RepairAction{
		Action:    "replay_op",
		MessageID: op.LocalMessageID,
		OpID:      op.OpID,
		FromState: op.State,
		Reason:    "notification operation result remains uncertain",
	}
	if errors.Is(err, errNotificationMutationDeadline) || errors.Is(err, errNotificationMutationAmbiguous) {
		_ = RecordCloudNotificationClaimOperationFailure(d.paths, op, err, now)
		return action, nil
	}
	_ = AbandonCloudNotificationClaimOperation(d.paths, op)
	action.Action = "complete_op"
	action.ToState = "removed"
	action.Reason = "notification operation failed definitively"
	return action, nil
}

func (d *Daemon) claimAbandonmentReason(message MessageEnvelope, now time.Time) (string, bool) {
	reason, shouldRepair, _ := d.claimAbandonmentReasonLocked(message, now, false, true)
	return reason, shouldRepair
}

func (d *Daemon) claimAbandonmentReasonLocked(message MessageEnvelope, now time.Time, failOnInspectError bool, persistSessionRecovery bool) (string, bool, error) {
	reason, shouldRepair := claimAbandonmentReason(d.paths, message, now)
	if message.Delivery.SessionID == nil {
		return reason, shouldRepair, nil
	}
	record, err := ReadSessionRecord(d.paths, *message.Delivery.SessionID)
	if err != nil {
		if !sessionRecordMissing(d.paths, *message.Delivery.SessionID) {
			return "", false, err
		}
		if shouldRepair && reason != "session_not_alive" {
			return reason, true, nil
		}
		return "session_unavailable", true, nil
	}
	if shouldRepair && reason != "session_not_alive" {
		if reason == "lease_expired" && message.Source == "comment.io" && shouldRetryStaleBmuxCleanupForClaim(record, message) {
			if !persistSessionRecovery {
				return reason, true, nil
			}
			if cleanupErr := d.cleanupStaleBmuxControlSessionLocked(record, reason); cleanupErr != nil {
				return "", false, errors.New(cleanupErr.Message)
			}
		}
		return reason, true, nil
	}
	if shouldRepair {
		if record.State == "stale" && message.Source == "comment.io" {
			return "tmux_session_missing", true, nil
		}
		if record.State != "alive" {
			return reason, true, nil
		}
	}
	var (
		live    bool
		liveErr *SocketError
	)
	if persistSessionRecovery {
		record, live, liveErr = d.recoverLiveTmuxSessionLockedForRepair(record)
	} else {
		record, live, liveErr = d.inspectLiveTmuxSessionLocked(record, true)
	}
	if liveErr != nil {
		if !failOnInspectError {
			return "", false, nil
		}
		return "", false, errors.New(liveErr.Message)
	}
	if !live {
		return "tmux_session_missing", true, nil
	}
	runtimeReason, runtimeErr := d.sessionRuntimeIssueLocked(record, false)
	if runtimeErr != nil {
		if !failOnInspectError {
			return "", false, nil
		}
		return "", false, errors.New(runtimeErr.Message)
	}
	if runtimeReason != "" {
		return runtimeReason, true, nil
	}
	return "", false, nil
}

func claimAbandonmentReason(paths Paths, message MessageEnvelope, now time.Time) (string, bool) {
	if reason, ok := defaultClaimAbandonmentReason(message, now); ok {
		return reason, true
	}
	if message.Delivery.SessionID == nil {
		return "", false
	}
	record, err := ReadSessionRecord(paths, *message.Delivery.SessionID)
	if err != nil {
		return "session_unavailable", true
	}
	if record.State != "alive" || record.Profile != message.Profile {
		return "session_not_alive", true
	}
	if record.BotName != message.BotName {
		return "session_bot_mismatch", true
	}
	if message.Delivery.SessionGeneration == nil || record.Generation != *message.Delivery.SessionGeneration {
		return "session_generation_mismatch", true
	}
	expectedPath, err := sessionCapabilityPathForRecord(paths, record)
	if err != nil {
		return "session_capability_unavailable", true
	}
	capability, err := ReadPrivateCapability(paths.Home, expectedPath, "managed-session capability file")
	if err != nil || !CapabilityTokenRE.MatchString(capability) {
		return "session_capability_unavailable", true
	}
	return "", false
}

func blockedRepairMessages(actions []RepairAction) map[string]struct{} {
	blocked := map[string]struct{}{}
	for _, action := range actions {
		if action.Action == "replay_op" && action.MessageID != "" && action.Reason != "declined duplicate notification lease requires release" {
			blocked[action.MessageID] = struct{}{}
		}
	}
	return blocked
}

func defaultClaimAbandonmentReason(message MessageEnvelope, now time.Time) (string, bool) {
	if message.Delivery.State != "claimed" {
		return "", false
	}
	if leaseExpired(message.Delivery.LeaseExpiresAt, now) {
		return "lease_expired", true
	}
	return "", false
}

func replayedCloudOperationMatchesLocalState(message MessageEnvelope, op CloudNotificationClaimOperation) bool {
	if message.Source != "comment.io" || message.ID != op.LocalMessageID || message.Profile != op.Profile {
		return false
	}
	switch op.Operation {
	case "ack":
		return message.Delivery.State == "acked" && !cloudClaimOperationCompletedHolderMismatch(message, op)
	case "release":
		return cloudReleaseCompletedLocalState(message)
	default:
		return false
	}
}

func cloudReleaseCompletedLocalState(message MessageEnvelope) bool {
	return (message.Delivery.State == "released" || message.Delivery.State == "unclaimed") &&
		message.Delivery.ClaimHolder == nil &&
		message.Delivery.SessionID == nil &&
		message.Delivery.SessionScope.Type == nil &&
		message.Delivery.SessionScope.ID == nil &&
		message.Delivery.SessionGeneration == nil &&
		message.Delivery.LeaseExpiresAt == nil
}

func cloudClaimOperationCompletedHolderMismatch(message MessageEnvelope, op CloudNotificationClaimOperation) bool {
	if op.ClaimHolder == "" || op.Operation != "ack" {
		return false
	}
	return message.Source == "comment.io" &&
		message.ID == op.LocalMessageID &&
		message.Profile == op.Profile &&
		message.Delivery.State == "acked" &&
		stringValue(message.Delivery.ClaimHolder) != op.ClaimHolder
}
