package commentbus

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxCapabilityFileBytes = 1024

var CapabilityTokenRE = regexp.MustCompile(`^cap_[A-Za-z0-9_-]{20,128}$`)

var ErrCapabilityFileTooLarge = errors.New("capability file too large")
var ErrCapabilityFileUnsafe = errors.New("capability file unsafe")

type CapabilityFile struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

func EnsureOwnerCapability(paths Paths) (CapabilityFile, error) {
	if err := EnsureBaseDirs(paths); err != nil {
		return CapabilityFile{}, err
	}
	if file, err := OpenPrivateFile(paths.Home, paths.OwnerCapability, "owner capability file"); err == nil {
		token, readErr := ReadCapabilityFromReader(file)
		if readErr == nil && CapabilityTokenRE.MatchString(token) {
			if err := file.Chmod(0o600); err != nil {
				_ = file.Close()
				return CapabilityFile{}, err
			}
			if err := file.Close(); err != nil {
				return CapabilityFile{}, err
			}
			return CapabilityFile{Path: paths.OwnerCapability, Created: false}, nil
		}
		_ = file.Close()
		if readErr != nil && !errors.Is(readErr, ErrCapabilityFileTooLarge) {
			return CapabilityFile{}, readErr
		}
	} else if !os.IsNotExist(err) {
		return CapabilityFile{}, err
	}
	token, err := generateCapabilityToken()
	if err != nil {
		return CapabilityFile{}, err
	}
	if err := WritePrivateFileAtomic(paths.OwnerCapability, []byte(token+"\n"), 0o600); err != nil {
		return CapabilityFile{}, err
	}
	return CapabilityFile{Path: paths.OwnerCapability, Created: true}, nil
}

func ReadCapability(path string) (string, error) {
	file, err := openCapabilityFile(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return ReadCapabilityFromReader(file)
}

func ReadPrivateCapability(root string, path string, label string) (string, error) {
	file, err := OpenPrivateFile(root, path, label)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return ReadCapabilityFromReader(file)
}

func capabilityUnsafeError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCapabilityFileUnsafe, fmt.Sprintf(format, args...))
}

func ReadCapabilityFromReader(reader io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCapabilityFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxCapabilityFileBytes {
		return "", ErrCapabilityFileTooLarge
	}
	return strings.TrimSpace(string(data)), nil
}

func generateCapabilityToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	token := "cap_" + base64.RawURLEncoding.EncodeToString(bytes)
	if !CapabilityTokenRE.MatchString(token) {
		return "", os.ErrInvalid
	}
	return token, nil
}
