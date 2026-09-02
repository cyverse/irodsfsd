package commons

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAcquirePIDFileCreatesAndRemovesFile(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "runtime", "irodsfsd.pid")
	pidFile, err := AcquirePIDFile(pidPath)
	if err != nil {
		t.Fatalf("acquirePIDFile: %v", err)
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("pid file = %q, want %q", got, want)
	}

	if err := pidFile.Close(); err != nil {
		t.Fatalf("close pid file: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("pid file still exists after close: %v", err)
	}
}

func TestAcquirePIDFileRejectsSecondOwner(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "irodsfsd.pid")
	first, err := AcquirePIDFile(pidPath)
	if err != nil {
		t.Fatalf("first acquirePIDFile: %v", err)
	}
	defer first.Close()

	if _, err := AcquirePIDFile(pidPath); err == nil {
		t.Fatal("second acquirePIDFile unexpectedly succeeded")
	}
}

func TestLockedPIDFileCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	pidFile, err := AcquirePIDFile(filepath.Join(t.TempDir(), "irodsfsd.pid"))
	if err != nil {
		t.Fatalf("acquirePIDFile: %v", err)
	}
	if err := pidFile.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := pidFile.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestReadPIDRejectsInvalidContent(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "irodsfsd.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if _, err := ReadPID(pidPath); err == nil {
		t.Fatal("readPID unexpectedly accepted invalid content")
	}
}

func TestReadPIDRejectsMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := ReadPID(filepath.Join(t.TempDir(), "missing.pid")); err == nil {
		t.Fatal("readPID unexpectedly accepted a missing file")
	}
}

func TestProcessRunningRecognizesCurrentProcess(t *testing.T) {
	t.Parallel()

	if !ProcessRunning(os.Getpid()) {
		t.Fatal("processRunning did not recognize the current process")
	}
}
