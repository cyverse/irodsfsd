// Package logstore converts a mount child's stdout/stderr into a
// size- and age-rotated, line-oriented log file, and lets that file be
// queried by tail count or time. It knows nothing about MountManager or
// api.MountInfo; callers supply the file path and the secret values to
// redact.
package logstore

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	maxSizeMB  = 10
	maxBackups = 5
	maxAgeDays = 14
)

// Record is one line of child stdout/stderr, timestamped and tagged with
// which stream it came from.
type Record struct {
	Time    time.Time `json:"time"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

// MountLog appends child stdout/stderr to one rotated log file as one JSON
// Record per line, redacting any configured secret value found in a line.
type MountLog struct {
	mutex   sync.Mutex
	writer  io.WriteCloser
	secrets []string
	now     func() time.Time
	stdout  *lineWriter
	stderr  *lineWriter
}

// Open opens (creating parent directories as needed) the rotated log file
// at path. secrets are exact values (passwords, tickets, tokens) that are
// replaced with "[REDACTED]" wherever they appear in a logged line, as
// defense in depth alongside never placing them on a command line.
func Open(path string, secrets []string) (*MountLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.Wrapf(err, "failed to create mount log directory for %q", path)
	}
	nonEmptySecrets := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			nonEmptySecrets = append(nonEmptySecrets, secret)
		}
	}

	log := &MountLog{
		writer:  &lumberjack.Logger{Filename: path, MaxSize: maxSizeMB, MaxBackups: maxBackups, MaxAge: maxAgeDays},
		secrets: nonEmptySecrets,
		now:     time.Now,
	}
	log.stdout = &lineWriter{log: log, stream: "stdout"}
	log.stderr = &lineWriter{log: log, stream: "stderr"}
	return log, nil
}

// Stdout returns the writer to use as a child process's Stdout.
func (log *MountLog) Stdout() io.Writer { return log.stdout }

// Stderr returns the writer to use as a child process's Stderr.
func (log *MountLog) Stderr() io.Writer { return log.stderr }

// Close flushes any buffered partial line from both streams and closes the
// underlying file. It does not delete the file: a future Open of the same
// path resumes the same history.
func (log *MountLog) Close() error {
	log.stdout.flush()
	log.stderr.flush()
	return log.writer.Close()
}

func (log *MountLog) writeLine(stream string, line string) {
	for _, secret := range log.secrets {
		line = strings.ReplaceAll(line, secret, "[REDACTED]")
	}
	data, err := json.Marshal(Record{Time: log.now(), Stream: stream, Message: line})
	if err != nil {
		return
	}
	data = append(data, '\n')

	log.mutex.Lock()
	defer log.mutex.Unlock()
	_, _ = log.writer.Write(data)
}

// lineWriter buffers partial writes until a newline completes a line, so
// arbitrary-sized process pipe reads become whole log lines. It is only
// ever written to by the single goroutine os/exec dedicates to copying one
// pipe, so it needs no lock of its own.
type lineWriter struct {
	log    *MountLog
	stream string
	buf    []byte
}

func (writer *lineWriter) Write(data []byte) (int, error) {
	writer.buf = append(writer.buf, data...)
	for {
		index := bytes.IndexByte(writer.buf, '\n')
		if index < 0 {
			break
		}
		line := string(writer.buf[:index])
		writer.buf = writer.buf[index+1:]
		writer.log.writeLine(writer.stream, line)
	}
	return len(data), nil
}

func (writer *lineWriter) flush() {
	if len(writer.buf) == 0 {
		return
	}
	writer.log.writeLine(writer.stream, string(writer.buf))
	writer.buf = nil
}
