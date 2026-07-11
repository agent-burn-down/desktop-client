package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mattn/go-isatty"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

const (
	logDirPerm    = 0o700
	logMaxSizeMB  = 10
	logMaxBackups = 5
	logMaxAgeDays = 28
)

// newLogger builds a JSON slog logger writing to <dir>/logs/collector.log with
// size-based rotation. It also writes to stderr when verbose is set or stderr
// is a TTY. The returned io.Closer flushes and closes the rotating file.
func newLogger(dir string, verbose bool) (*slog.Logger, io.Closer, error) {
	logPath := filepath.Join(dir, "logs", "collector.log")
	if err := os.MkdirAll(filepath.Dir(logPath), logDirPerm); err != nil {
		return nil, nil, fmt.Errorf("create log dir: %w", err)
	}
	rotator := &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    logMaxSizeMB,
		MaxBackups: logMaxBackups,
		MaxAge:     logMaxAgeDays,
		Compress:   true,
	}
	var w io.Writer = rotator
	if verbose || isatty.IsTerminal(os.Stderr.Fd()) {
		w = io.MultiWriter(rotator, os.Stderr)
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), rotator, nil
}
