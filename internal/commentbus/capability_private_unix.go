//go:build darwin || linux

package commentbus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func openCapabilityFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validateCapabilityFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func OpenPrivateFile(root string, path string, label string) (*os.File, error) {
	if err := validatePrivatePath(root, path, label); err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, capabilityUnsafeError("%s must live under selected home", label)
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	dirfd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, privateOpenError(err, root, fmt.Sprintf("%s parent directory", label))
	}
	defer func() { _ = unix.Close(dirfd) }()
	if err := validatePrivateDirFD(dirfd, label); err != nil {
		return nil, err
	}
	currentPath := root
	for _, part := range parts[:len(parts)-1] {
		if part == "" || part == "." {
			continue
		}
		currentPath = filepath.Join(currentPath, part)
		nextfd, err := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, privateOpenError(err, currentPath, fmt.Sprintf("%s parent directory", label))
		}
		if err := validatePrivateDirFD(nextfd, label); err != nil {
			_ = unix.Close(nextfd)
			return nil, err
		}
		_ = unix.Close(dirfd)
		dirfd = nextfd
	}
	name := parts[len(parts)-1]
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, privateOpenError(err, path, label)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := validatePrivateFile(file, label); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateCapabilityFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("%w: could not inspect", ErrCapabilityFileUnsafe)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: must be a regular file", ErrCapabilityFileUnsafe)
	}
	return nil
}

func validatePrivateFile(file *os.File, label string) error {
	info, err := file.Stat()
	if err != nil {
		return capabilityUnsafeError("could not inspect %s", label)
	}
	if !info.Mode().IsRegular() {
		return capabilityUnsafeError("%s must be a regular file", label)
	}
	if err := validateCurrentUserOwner(info, label); err != nil {
		return fmt.Errorf("%w: %w", ErrCapabilityFileUnsafe, err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return capabilityUnsafeError("%s must not be hard-linked", label)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return capabilityUnsafeError("%s must be private", label)
	}
	return nil
}

func validatePrivateDirFD(fd int, label string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return capabilityUnsafeError("could not inspect %s parent directory", label)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return capabilityUnsafeError("%s parent path must be a directory", label)
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return capabilityUnsafeError("%s parent directory must be owned by the current user", label)
	}
	if os.FileMode(stat.Mode).Perm()&0o077 != 0 {
		return capabilityUnsafeError("%s parent directory must be private", label)
	}
	return nil
}

func validatePrivatePath(root string, path string, label string) error {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return capabilityUnsafeError("%s must live under selected home", label)
	}
	return nil
}

func privateOpenError(err error, path string, label string) error {
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, unix.ELOOP) {
		return capabilityUnsafeError("%s must not be a symlink", label)
	}
	return capabilityUnsafeError("could not open %s", label)
}
