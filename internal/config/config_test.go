package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseYAMLOverlaysDefaults(t *testing.T) {
	data := "api_service_port: 9090\n" +
		"irodsfs_binary: /opt/irodsfs\n" +
		"mount_timeout: 45s\n" +
		"retry:\n" +
		"  max_attempts: 3\n" +
		"  initial_delay: 2s\n" +
		"  max_delay: 20s\n" +
		"  multiplier: 2\n" +
		"  jitter: 0.1\n"
	config, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.APIServicePort != 9090 {
		t.Fatalf("APIServicePort = %d", config.APIServicePort)
	}
	if config.MountTimeout.Duration() != 45*time.Second {
		t.Fatalf("MountTimeout = %s", config.MountTimeout)
	}
	if config.DataDir != "/var/lib/irodsfsd" {
		t.Fatalf("default DataDir was not retained: %q", config.DataDir)
	}
	if config.MountRoot != "/var/lib/irodsfsd/mounts" {
		t.Fatalf("default MountRoot was not retained: %q", config.MountRoot)
	}
	if len(config.AllowedMountRoots) != 2 || config.AllowedMountRoots[1] != "/var/lib/kubelet" {
		t.Fatalf("default AllowedMountRoots = %v", config.AllowedMountRoots)
	}
}

func TestParseJSONOverlaysDefaults(t *testing.T) {
	data := `{
		"api_service_port": 8181,
		"pid_file": "/tmp/irodsfsd.pid",
		"mount_root": "/tmp/irodsfsd-mounts",
		"shutdown_grace_period": "12s"
	}`
	config, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.PIDFile != "/tmp/irodsfsd.pid" {
		t.Fatalf("PIDFile = %q", config.PIDFile)
	}
	if config.MountRoot != "/tmp/irodsfsd-mounts" {
		t.Fatalf("MountRoot = %q", config.MountRoot)
	}
	if config.ShutdownGracePeriod.Duration() != 12*time.Second {
		t.Fatalf("ShutdownGracePeriod = %s", config.ShutdownGracePeriod)
	}
}

func TestLoadDetectsFormatFromContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(path, []byte(`{"api_service_port":8181}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.APIServicePort != 8181 {
		t.Fatalf("APIServicePort = %d", config.APIServicePort)
	}
}

func TestParseDetectsYAMLFlowMapping(t *testing.T) {
	config, err := Parse([]byte(`{api_service_port: 8282}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.APIServicePort != 8282 {
		t.Fatalf("APIServicePort = %d", config.APIServicePort)
	}
}

func TestParsePartialNestedConfigRetainsDefaults(t *testing.T) {
	config, err := Parse([]byte("retry:\n  max_attempts: 2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.Retry.InitialDelay.Duration() != time.Second {
		t.Fatalf("Retry.InitialDelay = %s", config.Retry.InitialDelay)
	}
	if config.APIServicePort != 13021 {
		t.Fatalf("APIServicePort = %d", config.APIServicePort)
	}
}

func TestParseRejectsUnknownField(t *testing.T) {
	_, err := Parse([]byte("unknown_setting: true\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown_setting") {
		t.Fatalf("Parse error = %v", err)
	}
}

func TestParseRejectsInvalidDuration(t *testing.T) {
	_, err := Parse([]byte("mount_timeout: tomorrow\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("Parse error = %v", err)
	}
}

func TestValidateRejectsRootMountAllowance(t *testing.T) {
	config := NewDefaultConfig()
	config.AllowedMountRoots = []string{"/"}
	if err := config.Validate(); err == nil {
		t.Fatal("Validate unexpectedly accepted filesystem root")
	}
}

func TestValidateRejectsInvalidAPIServicePort(t *testing.T) {
	for _, port := range []int{0, 65536} {
		config := NewDefaultConfig()
		config.APIServicePort = port
		if err := config.Validate(); err == nil {
			t.Fatalf("Validate unexpectedly accepted API service port %d", port)
		}
	}
}
