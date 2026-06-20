//go:build !darwin && !linux

package commentbus

import (
	"errors"
	"os"
)

func validateCurrentUserOwner(info os.FileInfo, label string) error {
	return errors.New(label + " owner checks are not supported on this platform")
}

func validateTrustedExecutablePath(path string, label string) error {
	return errors.New(label + " trust checks are not supported on this platform")
}

func validateTrustedExecutableReferencePath(path string, info os.FileInfo, label string) error {
	return errors.New(label + " trust checks are not supported on this platform")
}

func validateTrustedOriginalPathDir(path string, label string) error {
	return errors.New(label + " trust checks are not supported on this platform")
}

func validateTrustedLaunchDirPath(path string, label string) error {
	return errors.New(label + " trust checks are not supported on this platform")
}

func validateTrustedSearchPathDir(path string, label string) error {
	return errors.New(label + " trust checks are not supported on this platform")
}
