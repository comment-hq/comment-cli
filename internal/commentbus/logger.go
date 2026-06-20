package commentbus

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type structuredLogger struct {
	mu     sync.Mutex
	writer io.Writer
	closer io.Closer
	now    func() time.Time
}

func newStructuredLogger(paths Paths, writer io.Writer, now func() time.Time) (*structuredLogger, error) {
	logger := &structuredLogger{writer: writer, now: now}
	if logger.now == nil {
		logger.now = time.Now
	}
	if logger.writer != nil {
		return logger, nil
	}
	if err := os.MkdirAll(paths.Logs, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(paths.Logs, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(paths.Logs, "commentd.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	logger.writer = file
	logger.closer = file
	return logger, nil
}

func (l *structuredLogger) close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

func (l *structuredLogger) info(msg string, data map[string]any) {
	l.write("info", msg, data)
}

func (l *structuredLogger) warn(msg string, data map[string]any) {
	l.write("warn", msg, data)
}

func (l *structuredLogger) write(level string, msg string, data map[string]any) {
	if l == nil || l.writer == nil {
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	entry := map[string]any{
		"ts":        l.now().UTC().Format(time.RFC3339Nano),
		"level":     level,
		"component": "commentbus",
		"msg":       msg,
		"data":      data,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.writer.Write(append(line, '\n'))
}
