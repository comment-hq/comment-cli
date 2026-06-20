//go:build darwin || linux

package commentbus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestEnsureOwnerCapability(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := EnsureOwnerCapability(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatal("expected first capability call to create file")
	}
	info, err := os.Stat(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("owner capability mode = %o, want 0600", got)
	}
	token, err := ReadCapability(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) < 20 {
		t.Fatalf("capability token too short: %q", token)
	}
	if !CapabilityTokenRE.MatchString(token) {
		t.Fatalf("capability token has invalid grammar: %q", token)
	}
	second, err := EnsureOwnerCapability(paths)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Fatal("expected second capability call to reuse file")
	}
	tokenAgain, err := ReadCapability(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	if tokenAgain != token {
		t.Fatal("capability token changed")
	}
}

func TestEnsureOwnerCapabilityRotatesInvalidExistingToken(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.OwnerCapability), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("legacy-local-secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := EnsureOwnerCapability(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !file.Created {
		t.Fatal("expected invalid existing capability to be rotated")
	}
	token, err := ReadCapability(file.Path)
	if err != nil {
		t.Fatal(err)
	}
	if token == "legacy-local-secret-token" || !CapabilityTokenRE.MatchString(token) {
		t.Fatalf("rotated token = %q, want generated cap token", token)
	}
}

func TestEnsureOwnerCapabilityChmodsExistingToken(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	token := "cap_validLookingOwnerToken"
	if err := os.WriteFile(paths.OwnerCapability, []byte(token+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	file, err := EnsureOwnerCapability(paths)
	if err != nil {
		t.Fatal(err)
	}
	if file.Created {
		t.Fatal("expected existing capability to be reused")
	}
	info, err := os.Stat(paths.OwnerCapability)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("owner capability mode = %o, want 0600", got)
	}
}

func TestEnsureOwnerCapabilityRejectsUnsafeExistingToken(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Home, "target.cap")
	if err := os.WriteFile(target, []byte("cap_validLookingTargetToken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.OwnerCapability); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureOwnerCapability(paths); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("EnsureOwnerCapability unsafe err = %v", err)
	}
}

func TestReadCapabilityRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.cap")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxCapabilityFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapability(path); !errors.Is(err, ErrCapabilityFileTooLarge) {
		t.Fatalf("ReadCapability oversized err = %v", err)
	}
}

func TestReadPrivateCapabilityRejectsSymlink(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Home, "target.cap")
	if err := os.WriteFile(target, []byte("cap_validLookingTargetToken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.OwnerCapability); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateCapability(paths.Home, paths.OwnerCapability, "owner capability file"); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("ReadPrivateCapability symlink err = %v", err)
	}
}

func TestReadPrivateCapabilityRejectsHardlink(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("cap_validLookingOwnerToken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(paths.OwnerCapability, filepath.Join(paths.Home, "owner-linked.cap")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateCapability(paths.Home, paths.OwnerCapability, "owner capability file"); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("ReadPrivateCapability hardlink err = %v", err)
	}
}

func TestReadPrivateCapabilityRejectsNonPrivateFile(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OwnerCapability, []byte("cap_validLookingOwnerToken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paths.OwnerCapability, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateCapability(paths.Home, paths.OwnerCapability, "owner capability file"); !errors.Is(err, ErrCapabilityFileUnsafe) {
		t.Fatalf("ReadPrivateCapability non-private err = %v", err)
	}
}

func TestReadCapabilityRejectsFIFOBeforeReading(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.cap")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadCapability(path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCapabilityFileUnsafe) {
			t.Fatalf("ReadCapability fifo err = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		if fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("ReadCapability blocked on fifo")
	}
}

func TestReadPrivateCapabilityRejectsFIFOBeforeReading(t *testing.T) {
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(paths.OwnerCapability, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadPrivateCapability(paths.Home, paths.OwnerCapability, "owner capability file")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrCapabilityFileUnsafe) {
			t.Fatalf("ReadPrivateCapability fifo err = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		if fd, err := unix.Open(paths.OwnerCapability, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			_ = unix.Close(fd)
		}
		t.Fatal("ReadPrivateCapability blocked on fifo")
	}
}
