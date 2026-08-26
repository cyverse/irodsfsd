package command

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	godaemonizer "github.com/cyverse/go-daemonizer"
)

func TestVersionCommand(t *testing.T) {
	out := &bytes.Buffer{}
	cmd := newRootCommand(godaemonizer.New(), out, &bytes.Buffer{})
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "irodsfsd dev") {
		t.Fatalf("version output = %q", got)
	}
}

func TestStatusCommandForCurrentProcess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "irodsfsd.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	out := &bytes.Buffer{}
	cmd := newRootCommand(godaemonizer.New(), out, &bytes.Buffer{})
	cmd.SetArgs([]string{"--pid-file", pidPath, "status"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := out.String(); !strings.Contains(got, strconv.Itoa(os.Getpid())) {
		t.Fatalf("status output = %q", got)
	}
}

func TestReadPIDRejectsInvalidContent(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "irodsfsd.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if _, err := readPID(pidPath); err == nil {
		t.Fatal("readPID unexpectedly accepted invalid content")
	}
}

func TestResolvePIDFileFromJSONConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	pidPath := filepath.Join(t.TempDir(), "configured.pid")
	data := []byte(`{"pid_file":` + strconv.Quote(pidPath) + `}`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := resolvePIDFile(&rootOptions{configPath: configPath})
	if err != nil {
		t.Fatalf("resolvePIDFile: %v", err)
	}
	if got != pidPath {
		t.Fatalf("resolvePIDFile = %q, want %q", got, pidPath)
	}
}

func TestCommandLineValuesOverrideYAMLConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	data := []byte("pid_file: /configured/irodsfsd.pid\n" +
		"daemon_log_file: /configured/irodsfsd.log\n" +
		"working_directory: /configured\n")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	config, err := loadConfig(
		&rootOptions{configPath: configPath, pidFile: filepath.Join(dir, "override.pid")},
		filepath.Join(dir, "override.log"),
		dir,
	)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if config.PIDFile != filepath.Join(dir, "override.pid") {
		t.Fatalf("PIDFile = %q", config.PIDFile)
	}
	if config.DaemonLogFile != filepath.Join(dir, "override.log") {
		t.Fatalf("DaemonLogFile = %q", config.DaemonLogFile)
	}
	if config.WorkingDirectory != dir {
		t.Fatalf("WorkingDirectory = %q", config.WorkingDirectory)
	}
}
