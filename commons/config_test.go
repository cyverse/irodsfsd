package commons

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseYAMLOverlaysDefaults(t *testing.T) {
	data := "service_endpoint: tcp://127.0.0.1:9090\n" +
		"service_port: 14000\n" +
		"irodsfs_executable_path: /opt/irodsfs\n" +
		"debug: true\n" +
		"mount_timeout: 45s\n" +
		"retry:\n" +
		"  max_attempts: 3\n" +
		"  initial_delay: 2s\n" +
		"  max_delay: 20s\n" +
		"  multiplier: 2\n" +
		"  jitter: 0.1\n"
	config, err := NewConfigFromYAML(NewDefaultConfig(), []byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.ServiceEndpoint != "tcp://127.0.0.1:9090" {
		t.Fatalf("ServiceEndpoint = %q", config.ServiceEndpoint)
	}
	if config.ServicePort != 14000 {
		t.Fatalf("ServicePort = %d", config.ServicePort)
	}
	if config.IRODSFSExecutablePath != "/opt/irodsfs" {
		t.Fatalf("IRODSFSExecutablePath = %q", config.IRODSFSExecutablePath)
	}
	if config.MountExecutablePath != MountExecutablePathDefault {
		t.Fatalf("MountExecutablePath = %q", config.MountExecutablePath)
	}
	if config.UnmountExecutablePath != UnmountExecutablePathDefault {
		t.Fatalf("UnmountExecutablePath = %q", config.UnmountExecutablePath)
	}
	if !config.Debug {
		t.Fatal("Debug = false")
	}
	if time.Duration(config.MountTimeout) != 45*time.Second {
		t.Fatalf("MountTimeout = %s", time.Duration(config.MountTimeout))
	}
	if config.DataRootPath != "/var/lib/irodsfsd" {
		t.Fatalf("default DataRootPath was not retained: %q", config.DataRootPath)
	}
	if config.GetMountRootPath() != "/var/lib/irodsfsd/mounts" {
		t.Fatalf("default mount root path = %q", config.GetMountRootPath())
	}
	if len(config.AllowedMountRootPaths) != 2 || config.AllowedMountRootPaths[1] != "/var/lib/kubelet" {
		t.Fatalf("default AllowedMountRootPaths = %v", config.AllowedMountRootPaths)
	}
}

func TestParseJSONOverlaysDefaults(t *testing.T) {
	data := `{
		"service_endpoint": "tcp://127.0.0.1:8181",
		"pid_file": "/tmp/irodsfsd.pid",
		"data_root_path": "/tmp/irodsfsd",
		"mount_root_path": "/tmp/irodsfsd/mounts"
	}`
	config, err := NewConfigFromJSON(NewDefaultConfig(), []byte(data))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.PIDFile != "/tmp/irodsfsd.pid" {
		t.Fatalf("PIDFile = %q", config.PIDFile)
	}
	if config.GetMountRootPath() != "/tmp/irodsfsd/mounts" {
		t.Fatalf("mount root path = %q", config.GetMountRootPath())
	}
}

func TestLoadDetectsFormatFromContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(path, []byte(`{"service_endpoint":"tcp://127.0.0.1:8181"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	config, err := NewConfigFromFile(NewDefaultConfig(), path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.ServiceEndpoint != "tcp://127.0.0.1:8181" {
		t.Fatalf("ServiceEndpoint = %q", config.ServiceEndpoint)
	}
}

func TestParseDetectsYAMLFlowMapping(t *testing.T) {
	config, err := NewConfigFromYAML(NewDefaultConfig(), []byte(`{service_endpoint: "tcp://127.0.0.1:8282"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.ServiceEndpoint != "tcp://127.0.0.1:8282" {
		t.Fatalf("ServiceEndpoint = %q", config.ServiceEndpoint)
	}
}

func TestParsePartialNestedConfigRetainsDefaults(t *testing.T) {
	config, err := NewConfigFromYAML(NewDefaultConfig(), []byte("retry:\n  max_attempts: 2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if time.Duration(config.Retry.InitialDelay) != time.Second {
		t.Fatalf("Retry.InitialDelay = %s", time.Duration(config.Retry.InitialDelay))
	}
	if config.GetServiceEndpoint() != "unix:///var/lib/irodsfsd/comm.sock" {
		t.Fatalf("service endpoint = %q", config.GetServiceEndpoint())
	}
	if config.MaxConcurrentMounts != 40 {
		t.Fatalf("MaxConcurrentMounts = %d", config.MaxConcurrentMounts)
	}
	if config.ServicePort != ServicePortDefault {
		t.Fatalf("ServicePort = %d", config.ServicePort)
	}
	if time.Duration(config.DAVFSUnmountTimeout) != 3*time.Minute {
		t.Fatalf("DAVFSUnmountTimeout = %s", time.Duration(config.DAVFSUnmountTimeout))
	}
}

func TestNewConfigFromYAMLIgnoresUnknownField(t *testing.T) {
	_, err := NewConfigFromYAML(NewDefaultConfig(), []byte("unknown_setting: true\n"))
	if err != nil {
		t.Fatalf("NewConfigFromYAML: %v", err)
	}
}

func TestNewConfigFromYAMLIgnoresRemovedField(t *testing.T) {
	_, err := NewConfigFromYAML(NewDefaultConfig(), []byte("runtime_dir: /run/irodsfsd\n"))
	if err != nil {
		t.Fatalf("NewConfigFromYAML: %v", err)
	}
}

func TestParseRejectsInvalidDuration(t *testing.T) {
	_, err := NewConfigFromYAML(NewDefaultConfig(), []byte("mount_timeout: tomorrow\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("Parse error = %v", err)
	}
}

func TestValidateRejectsRootMountAllowance(t *testing.T) {
	config := newValidConfig()
	config.AllowedMountRootPaths = []string{"/"}
	if err := config.Validate(); err == nil {
		t.Fatal("Validate unexpectedly accepted filesystem root")
	}
}

func TestValidateRejectsInvalidServiceEndpoint(t *testing.T) {
	config := newValidConfig()
	config.ServiceEndpoint = "http://127.0.0.1:8080"
	if err := config.Validate(); err == nil {
		t.Fatal("Validate unexpectedly accepted an HTTP service endpoint")
	}
}

func TestValidateRejectsNonPositiveDAVFSUnmountTimeout(t *testing.T) {
	config := newValidConfig()
	config.DAVFSUnmountTimeout = 0
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "davfs_unmount_timeout must be positive") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestValidateRejectsInvalidServicePort(t *testing.T) {
	config := newValidConfig()
	config.ServicePort = 65536
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "service_port must be between 0 and 65535") {
		t.Fatalf("Validate error = %v", err)
	}
}

func TestGetRecoveryEncryptionKey(t *testing.T) {
	want := []byte("0123456789abcdef0123456789abcdef")
	config := NewDefaultConfig()
	config.RecoveryEncryptionKey = base64.StdEncoding.EncodeToString(want)

	got, err := config.GetRecoveryEncryptionKey()
	if err != nil {
		t.Fatalf("GetRecoveryEncryptionKey: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("GetRecoveryEncryptionKey = %q", got)
	}
}

func TestGetRecoveryEncryptionKeyRejectsInvalidKey(t *testing.T) {
	config := NewDefaultConfig()
	config.RecoveryEncryptionKey = base64.StdEncoding.EncodeToString([]byte("too short"))

	if _, err := config.GetRecoveryEncryptionKey(); err == nil {
		t.Fatal("GetRecoveryEncryptionKey unexpectedly accepted a short key")
	}
}

func newValidConfig() *Config {
	config := NewDefaultConfig()
	key := []byte("0123456789abcdef0123456789abcdef")
	config.RecoveryEncryptionKey = base64.StdEncoding.EncodeToString(key)
	return config
}
