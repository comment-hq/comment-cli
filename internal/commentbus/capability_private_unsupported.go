//go:build !darwin && !linux

package commentbus

import (
	"errors"
	"os"
)

var errCapabilityPlatformUnsupported = errors.New("capability file trust checks are not supported on this platform")

func openCapabilityFile(path string) (*os.File, error) {
	return nil, capabilityUnsafeError("%s", errCapabilityPlatformUnsupported)
}

func OpenPrivateFile(root string, path string, label string) (*os.File, error) {
	return nil, capabilityUnsafeError("%s", errCapabilityPlatformUnsupported)
}
