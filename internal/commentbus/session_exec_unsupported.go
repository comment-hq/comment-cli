//go:build !darwin && !linux

package commentbus

import (
	"errors"
	"path/filepath"
)

type runtimeCommandResolution struct {
	CommandPath string
	RuntimePath string
}

func ExecManagedSession(paths Paths, sessionID string, generation string) error {
	return errors.New("managed sessions are not supported on this platform")
}

func validateRuntimeCommand(record SessionRecord) error {
	if !isManagedSessionRuntime(record.Runtime) {
		return errors.New("unsupported runtime")
	}
	if !managedSessionRuntimeCommandMatches(record) {
		return errors.New("invalid runtime command")
	}
	if !isBotName(record.BotName) {
		return errors.New("invalid runtime bot")
	}
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		// Shell mode resolves the runtime name through the login shell at
		// launch; the path fields carry no meaning and must be empty.
		if record.RuntimePath != "" || record.RuntimeCommandPath != "" {
			return errors.New("invalid runtime command")
		}
		return nil
	}
	if record.RuntimePath != "" {
		if !isSafeAbsoluteLocalPath(record.RuntimePath) {
			return errors.New("invalid runtime command")
		}
		if !isSafeAbsoluteLocalPath(record.RuntimeCommandPath) || filepath.Base(record.RuntimeCommandPath) != record.Runtime {
			return errors.New("invalid runtime command")
		}
	} else if record.RuntimeCommandPath != "" {
		return errors.New("invalid runtime command")
	}
	return nil
}

func resolveRuntimeCommandExecutable(record SessionRecord, lookPath func(string) (string, error)) (string, error) {
	return "", errors.New("managed sessions are not supported on this platform")
}

func resolveRuntimeCommandReference(record SessionRecord, lookPath func(string) (string, error)) (runtimeCommandResolution, error) {
	return runtimeCommandResolution{}, errors.New("managed sessions are not supported on this platform")
}
