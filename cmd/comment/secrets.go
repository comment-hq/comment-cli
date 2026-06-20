package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const maxCLISecretValueBytes = 64 * 1024

var secretNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type secretsFile struct {
	Version   int               `json:"version"`
	Secrets   map[string]string `json:"secrets"`
	UpdatedAt string            `json:"updated_at,omitempty"`
}

func runSecrets(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "add":
		return runSecretsAdd(args[1:])
	case "get":
		return runSecretsGet(args[1:])
	default:
		return fmt.Errorf("unknown secrets command %q", args[0])
	}
}

func runSecretsAdd(args []string) error {
	fs := flag.NewFlagSet("comment secrets add", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	valueStdin := fs.Bool("value-stdin", false, "read secret value from stdin")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) < 1 {
		return errors.New("secrets add requires a name")
	}
	if len(positionals) > 2 {
		return errors.New("secrets add accepts one name and one value")
	}
	name := positionals[0]
	if err := validateSecretName(name); err != nil {
		return err
	}
	if *valueStdin && len(positionals) == 2 {
		return errors.New("secrets add accepts either a positional value or --value-stdin, not both")
	}
	var value string
	if *valueStdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, maxCLISecretValueBytes+1))
		if err != nil {
			return err
		}
		if len(data) > maxCLISecretValueBytes {
			return errors.New("secret value exceeds 65536 bytes")
		}
		value = strings.TrimRight(string(data), "\r\n")
	} else {
		if len(positionals) != 2 {
			return errors.New("secrets add requires a value or --value-stdin")
		}
		value = positionals[1]
	}
	if err := validateSecretValue(value); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	secrets, err := readSecretsFile(paths)
	if err != nil {
		return err
	}
	if secrets.Secrets == nil {
		secrets.Secrets = map[string]string{}
	}
	secrets.Version = 1
	secrets.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	secrets.Secrets[name] = value
	if err := writeSecretsFile(paths, secrets); err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":   true,
		"name": name,
		"path": secretsPath(paths),
	})
}

func runSecretsGet(args []string) error {
	fs := flag.NewFlagSet("comment secrets get", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	positionals := fs.Args()
	if len(positionals) != 1 {
		return errors.New("secrets get requires exactly one name")
	}
	name := positionals[0]
	if err := validateSecretName(name); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	secrets, err := readSecretsFile(paths)
	if err != nil {
		return err
	}
	value, ok := secrets.Secrets[name]
	if !ok {
		return fmt.Errorf("secret %q not found", name)
	}
	fmt.Println(value)
	return nil
}

func validateSecretName(name string) error {
	if !secretNameRE.MatchString(name) {
		return errors.New("invalid secret name: use env-var style names like OPENAI_API_KEY")
	}
	return nil
}

func validateSecretValue(value string) error {
	if value == "" {
		return errors.New("secret value must not be empty")
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("secret value must not contain NUL bytes")
	}
	if len([]byte(value)) > maxCLISecretValueBytes {
		return errors.New("secret value exceeds 65536 bytes")
	}
	return nil
}

func readSecretsFile(paths commentbus.Paths) (secretsFile, error) {
	path := secretsPath(paths)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return secretsFile{Version: 1, Secrets: map[string]string{}}, nil
		}
		return secretsFile{}, err
	}
	var secrets secretsFile
	if err := json.Unmarshal(data, &secrets); err != nil {
		return secretsFile{}, errors.New("invalid secrets file")
	}
	if secrets.Version != 1 {
		return secretsFile{}, errors.New("unsupported secrets file version")
	}
	if secrets.Secrets == nil {
		secrets.Secrets = map[string]string{}
	}
	return secrets, nil
}

func writeSecretsFile(paths commentbus.Paths, secrets secretsFile) error {
	data, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return err
	}
	return commentbus.WritePrivateFileAtomic(secretsPath(paths), append(data, '\n'), 0o600)
}

func secretsPath(paths commentbus.Paths) string {
	return filepath.Join(paths.Home, ".secrets")
}
