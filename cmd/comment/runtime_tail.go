package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const runtimeTailDefaultBytes = 64 * 1024

func runRuntimeTail(args []string) error {
	fs := flag.NewFlagSet("comment __runtime-tail", flag.ContinueOnError)
	check := fs.Bool("check", false, "check runtime tail helper support")
	logPath := fs.String("log", "", "runtime output log path")
	maxBytes := fs.Int("bytes", runtimeTailDefaultBytes, "maximum bytes to keep")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("__runtime-tail does not accept positional arguments")
	}
	if *check {
		return nil
	}
	if *logPath == "" {
		return fmt.Errorf("__runtime-tail requires --log")
	}
	if *maxBytes < 1024 {
		return fmt.Errorf("__runtime-tail --bytes must be at least 1024")
	}
	return copyRuntimeOutputTail(os.Stdin, io.Discard, *logPath, *maxBytes)
}

func copyRuntimeOutputTail(input io.Reader, output io.Writer, logPath string, maxBytes int) error {
	buffer := make([]byte, 32*1024)
	tail := make([]byte, 0, maxBytes)
	var writeErr error
	var logErr error
	for {
		n, readErr := input.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			tail = appendBoundedTail(tail, chunk, maxBytes)
			if err := writeRuntimeOutputTailLog(logPath, tail); err != nil && logErr == nil {
				logErr = err
			}
			if _, err := output.Write(chunk); err != nil && writeErr == nil {
				writeErr = err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := writeRuntimeOutputTailLog(logPath, tail); err != nil {
		return err
	}
	if logErr != nil {
		return logErr
	}
	return writeErr
}

func appendBoundedTail(tail []byte, chunk []byte, maxBytes int) []byte {
	if len(chunk) >= maxBytes {
		tail = tail[:0]
		return append(tail, chunk[len(chunk)-maxBytes:]...)
	}
	tail = append(tail, chunk...)
	if len(tail) <= maxBytes {
		return tail
	}
	copy(tail, tail[len(tail)-maxBytes:])
	return tail[:maxBytes]
}

func writeRuntimeOutputTailLog(logPath string, tail []byte) error {
	dir := filepath.Dir(logPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".runtime-tail-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(tail); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, logPath)
}
