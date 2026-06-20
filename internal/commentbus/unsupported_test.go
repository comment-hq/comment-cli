//go:build !darwin && !linux

package commentbus

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformsFailClosedForPrivateFileTrust(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "owner.cap")
	if err := os.WriteFile(path, []byte("cap_validLookingOwnerToken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapability(path); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("ReadCapability unsupported err = %v", err)
	}
	if _, err := OpenPrivateFile(home, path, "owner capability file"); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("OpenPrivateFile unsupported err = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCurrentUserOwner(info, "owner capability file"); err == nil {
		t.Fatal("expected unsupported owner validation error")
	}
}
