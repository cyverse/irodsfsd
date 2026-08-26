package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	appconfig "github.com/cyverse/irodsfsd/internal/config"
)

func TestRunCreatesAndRemovesPIDFile(t *testing.T) {
	t.Parallel()

	pidPath := t.TempDir() + "/irodsfsd.pid"
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	done := make(chan error, 1)
	config := *appconfig.NewDefaultConfig()
	config.PIDFile = pidPath

	go func() {
		done <- Run(ctx, Options{
			ConfigPath: "/test/config.yaml",
			Config:     config,
		}, func(err error) {
			ready <- err
		})
	}()

	if err := <-ready; err != nil {
		t.Fatalf("Run reported initialization failure: %v", err)
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	want := strconv.Itoa(os.Getpid())
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("pid file = %q, want %q", got, want)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pid file still exists after shutdown: %v", err)
	}
}

func TestAcquirePIDFileRejectsSecondOwner(t *testing.T) {
	t.Parallel()

	pidPath := t.TempDir() + "/irodsfsd.pid"
	first, err := acquirePIDFile(pidPath)
	if err != nil {
		t.Fatalf("first acquirePIDFile: %v", err)
	}
	defer first.Close()

	if _, err := acquirePIDFile(pidPath); err == nil {
		t.Fatal("second acquirePIDFile unexpectedly succeeded")
	}
}

func TestAcquirePIDFileCreatesParentDirectory(t *testing.T) {
	t.Parallel()

	pidPath := t.TempDir() + "/nested/runtime/irodsfsd.pid"
	pidFile, err := acquirePIDFile(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDFile: %v", err)
	}
	defer pidFile.Close()

	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("stat pid file: %v", err)
	}
}
