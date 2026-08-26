// Package daemon owns the lifecycle of the long-running irodsfsd process.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	appconfig "github.com/cyverse/irodsfsd/internal/config"
)

// Options are the startup values needed by the daemon skeleton.
type Options struct {
	ConfigPath string
	Config     appconfig.Config
}

// Run acquires the process lock, reports readiness to the daemonizing parent,
// and blocks until SIGINT, SIGTERM, or context cancellation.
func Run(ctx context.Context, opts Options, ready func(error)) error {
	pidFile, err := acquirePIDFile(opts.Config.PIDFile)
	if err != nil {
		reportReady(ready, err)
		return err
	}
	defer pidFile.Close()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("irodsfsd started",
		"pid", os.Getpid(),
		"config", opts.ConfigPath,
		"pid_file", opts.Config.PIDFile,
		"api_service_port", opts.Config.APIServicePort,
	)
	reportReady(ready, nil)

	<-ctx.Done()
	slog.Info("irodsfsd stopping", "pid", os.Getpid())
	return nil
}

func reportReady(ready func(error), err error) {
	if ready != nil {
		ready(err)
	}
}

type lockedPIDFile struct {
	file *os.File
	path string
	pid  int
}

func acquirePIDFile(path string) (*lockedPIDFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("pid file path cannot be empty")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create pid directory for %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open pid file %q: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("irodsfsd is already running (pid file: %s)", path)
		}
		return nil, fmt.Errorf("lock pid file %q: %w", path, err)
	}

	pid := os.Getpid()
	if err := file.Truncate(0); err != nil {
		releaseFile(file)
		return nil, fmt.Errorf("truncate pid file %q: %w", path, err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		releaseFile(file)
		return nil, fmt.Errorf("seek pid file %q: %w", path, err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", pid); err != nil {
		releaseFile(file)
		return nil, fmt.Errorf("write pid file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		releaseFile(file)
		return nil, fmt.Errorf("sync pid file %q: %w", path, err)
	}

	return &lockedPIDFile{file: file, path: path, pid: pid}, nil
}

func (p *lockedPIDFile) Close() error {
	if p == nil || p.file == nil {
		return nil
	}

	// Only remove the file if it still names this process.
	data, readErr := os.ReadFile(p.path)
	if readErr == nil {
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if parseErr == nil && pid == p.pid {
			_ = os.Remove(p.path)
		}
	}

	err := releaseFile(p.file)
	p.file = nil
	return err
}

func releaseFile(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
