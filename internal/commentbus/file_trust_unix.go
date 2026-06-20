//go:build darwin || linux

package commentbus

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

func validateCurrentUserOwner(info os.FileInfo, label string) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Geteuid()) {
		return errors.New(label + " must be owned by the current user")
	}
	return nil
}

func validateTrustedExecutablePath(path string, label string) error {
	if !isSafeAbsoluteLocalPath(path) {
		return errors.New("invalid " + label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New(label + " must exist")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New(label + " must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New(label + " must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return errors.New(label + " must be executable")
	}
	if err := validateTrustedPathOwner(info, label); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(label + " must not be group- or world-writable")
	}
	dir := filepath.Dir(path)
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return errors.New(label + " parent must exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New(label + " parent must not be a symlink")
		}
		if !info.IsDir() {
			return errors.New(label + " parent must be a directory")
		}
		if err := validateTrustedPathOwner(info, label+" parent"); err != nil {
			return err
		}
		if isUnsafeTrustedDirMode(dir, info, false, true) {
			return errors.New(label + " parent must not be group- or world-writable")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

func validateTrustedExecutableReferencePath(path string, info os.FileInfo, label string) error {
	if !isSafeAbsoluteLocalPath(path) {
		return errors.New("invalid " + label)
	}
	if err := validateTrustedOriginalPathDir(filepath.Dir(path), label+" parent"); err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if err := validateTrustedPathOwner(info, label+" symlink"); err != nil {
			return err
		}
	}
	return nil
}

func validateTrustedOriginalPathDir(path string, label string) error {
	if !isSafeAbsoluteLocalPath(path) {
		return errors.New("invalid " + label)
	}
	clean := filepath.Clean(path)
	dirs := []string{}
	for {
		dirs = append(dirs, clean)
		parent := filepath.Dir(clean)
		if parent == clean {
			break
		}
		clean = parent
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		info, err := os.Lstat(dir)
		if err != nil {
			return errors.New(label + " must exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := validateTrustedPathOwner(info, label); err != nil {
				return err
			}
			continue
		}
		if !info.IsDir() {
			return errors.New(label + " must be a directory")
		}
		if err := validateTrustedPathOwner(info, label); err != nil {
			return err
		}
		if isUnsafeTrustedDirMode(dir, info, true, true) {
			return errors.New(label + " must not be group- or world-writable")
		}
	}
	return nil
}

func validateTrustedLaunchDirPath(path string, label string) error {
	if !isSafeAbsoluteLocalPath(path) {
		return errors.New("invalid " + label)
	}
	clean := filepath.Clean(path)
	info, err := os.Lstat(clean)
	if err != nil {
		return errors.New(label + " must exist")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New(label + " must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New(label + " must be a directory")
	}
	if err := validateCurrentUserOwner(info, label); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(label + " must not be group- or world-writable")
	}
	dir := filepath.Dir(clean)
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return errors.New(label + " parent must exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New(label + " parent must not be a symlink")
		}
		if !info.IsDir() {
			return errors.New(label + " parent must be a directory")
		}
		if err := validateTrustedPathOwner(info, label+" parent"); err != nil {
			return err
		}
		if isUnsafeTrustedDirMode(dir, info, false, false) {
			return errors.New(label + " parent must not be group- or world-writable")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

func validateTrustedSearchPathDir(path string, label string) error {
	if !isSafeAbsoluteLocalPath(path) {
		return errors.New("invalid " + label)
	}
	dir := filepath.Clean(path)
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return errors.New(label + " must exist")
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New(label + " must not be a symlink")
		}
		if !info.IsDir() {
			return errors.New(label + " must be a directory")
		}
		if err := validateTrustedPathOwner(info, label); err != nil {
			return err
		}
		if isUnsafeTrustedDirMode(dir, info, true, true) {
			return errors.New(label + " must not be group- or world-writable")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return nil
}

func isAllowedGroupWritableHomebrewDir(path string, allowBin bool) bool {
	return isAllowedGroupWritableHomebrewDirForGOOS(path, allowBin, runtime.GOOS)
}

func isAllowedGroupWritableHomebrewDirForGOOS(path string, allowBin bool, goos string) bool {
	clean := filepath.Clean(path)
	if clean == "/opt/homebrew/Cellar" {
		return true
	}
	if goos == "darwin" && clean == "/opt/homebrew/lib" {
		return true
	}
	if allowBin && clean == "/opt/homebrew/bin" {
		return true
	}
	if clean == "/usr/local/Cellar" {
		return true
	}
	if goos == "darwin" && clean == "/usr/local/lib" {
		return true
	}
	if allowBin && clean == "/usr/local/bin" {
		return true
	}
	return false
}

func isUnsafeTrustedDirMode(path string, info os.FileInfo, allowHomebrewBin bool, allowHomebrew bool) bool {
	perm := info.Mode().Perm()
	if perm&0o002 != 0 {
		return info.Mode()&os.ModeSticky == 0
	}
	if perm&0o020 != 0 {
		return !allowHomebrew || !isAllowedGroupWritableHomebrewDir(path, allowHomebrewBin)
	}
	return false
}

func validateTrustedPathOwner(info os.FileInfo, label string) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		uid := stat.Uid
		if uid != 0 && uid != uint32(os.Geteuid()) {
			return errors.New(label + " must be owned by root or the current user")
		}
	}
	return nil
}
