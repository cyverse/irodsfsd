// Package config loads and validates irodsfsd configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts Go duration strings in both JSON and YAML.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }
func (d Duration) String() string          { return time.Duration(d).String() }

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	return d.parse(value)
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string")
	}
	return d.parse(node.Value)
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duration) parse(value string) error {
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

type RetryConfig struct {
	MaxAttempts  int      `json:"max_attempts" yaml:"max_attempts"`
	InitialDelay Duration `json:"initial_delay" yaml:"initial_delay"`
	MaxDelay     Duration `json:"max_delay" yaml:"max_delay"`
	Multiplier   float64  `json:"multiplier" yaml:"multiplier"`
	Jitter       float64  `json:"jitter" yaml:"jitter"`
}

type Config struct {
	APIServicePort    int      `json:"api_service_port" yaml:"api_service_port"`
	IRODSFSBinary     string   `json:"irodsfs_binary" yaml:"irodsfs_binary"`
	DataDir           string   `json:"data_dir" yaml:"data_dir"`
	RuntimeDir        string   `json:"runtime_dir" yaml:"runtime_dir"`
	MountRoot         string   `json:"mount_root" yaml:"mount_root"` // Parent directory for per-mount irodsfs data roots.
	PIDFile           string   `json:"pid_file" yaml:"pid_file"`
	DaemonLogFile     string   `json:"daemon_log_file" yaml:"daemon_log_file"`
	WorkingDirectory  string   `json:"working_directory" yaml:"working_directory"`
	AllowedMountRoots []string `json:"allowed_mount_roots" yaml:"allowed_mount_roots"`

	Retry               RetryConfig `json:"retry" yaml:"retry"`
	MountTimeout        Duration    `json:"mount_timeout" yaml:"mount_timeout"`
	UnmountTimeout      Duration    `json:"unmount_timeout" yaml:"unmount_timeout"`
	ShutdownGracePeriod Duration    `json:"shutdown_grace_period" yaml:"shutdown_grace_period"`
	ReconcileInterval   Duration    `json:"reconcile_interval" yaml:"reconcile_interval"`
	MaxConcurrentMounts int         `json:"max_concurrent_mounts" yaml:"max_concurrent_mounts"`
	RestoreOnStart      bool        `json:"restore_on_start" yaml:"restore_on_start"`
}

func NewDefaultConfig() *Config {
	return &Config{
		APIServicePort:    13021,
		IRODSFSBinary:     "/usr/local/bin/irodsfs",
		DataDir:           "/var/lib/irodsfsd",
		RuntimeDir:        "/run/irodsfsd",
		MountRoot:         "/var/lib/irodsfsd/mounts",
		PIDFile:           "/run/irodsfsd/irodsfsd.pid",
		DaemonLogFile:     "/var/log/irodsfsd/irodsfsd.log",
		WorkingDirectory:  "/var/lib/irodsfsd",
		AllowedMountRoots: []string{"/mnt/irods", "/var/lib/kubelet"},
		Retry: RetryConfig{
			MaxAttempts:  5,
			InitialDelay: Duration(time.Second),
			MaxDelay:     Duration(30 * time.Second),
			Multiplier:   2,
			Jitter:       0.2,
		},
		MountTimeout:        Duration(30 * time.Second),
		UnmountTimeout:      Duration(15 * time.Second),
		ShutdownGracePeriod: Duration(20 * time.Second),
		ReconcileInterval:   Duration(10 * time.Second),
		MaxConcurrentMounts: 4,
		RestoreOnStart:      true,
	}
}

func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("configuration file path cannot be empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration file %q: %w", path, err)
	}
	config, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse configuration file %q: %w", path, err)
	}
	return config, nil
}

func Parse(data []byte) (*Config, error) {
	config := NewDefaultConfig()
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("configuration is empty")
	}

	var err error
	if json.Valid(trimmed) {
		err = decodeJSON(trimmed, config)
	} else {
		err = decodeYAML(trimmed, config)
	}
	if err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration: %w", err)
	}
	return config, nil
}

func decodeJSON(data []byte, config *Config) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(config); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode JSON: multiple values are not allowed")
		}
		return fmt.Errorf("decode JSON trailing data: %w", err)
	}
	return nil
}

func decodeYAML(data []byte, config *Config) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil {
		return fmt.Errorf("decode YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode YAML: multiple documents are not allowed")
		}
		return fmt.Errorf("decode YAML trailing document: %w", err)
	}
	return nil
}

func (c *Config) Validate() error {
	if c.APIServicePort < 1 || c.APIServicePort > 65535 {
		return fmt.Errorf("api_service_port must be between 1 and 65535, got %d", c.APIServicePort)
	}

	paths := map[string]string{
		"irodsfs_binary":    c.IRODSFSBinary,
		"data_dir":          c.DataDir,
		"runtime_dir":       c.RuntimeDir,
		"mount_root":        c.MountRoot,
		"pid_file":          c.PIDFile,
		"daemon_log_file":   c.DaemonLogFile,
		"working_directory": c.WorkingDirectory,
	}
	for name, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s %q must be an absolute path", name, path)
		}
	}

	if len(c.AllowedMountRoots) == 0 {
		return errors.New("allowed_mount_roots must contain at least one path")
	}
	for _, root := range c.AllowedMountRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("allowed mount root %q must be an absolute path", root)
		}
		if filepath.Clean(root) == string(filepath.Separator) {
			return errors.New("filesystem root cannot be an allowed mount root")
		}
	}

	if c.Retry.MaxAttempts < 1 {
		return errors.New("retry.max_attempts must be at least 1")
	}
	if c.Retry.InitialDelay.Duration() <= 0 || c.Retry.MaxDelay.Duration() <= 0 {
		return errors.New("retry delays must be positive")
	}
	if c.Retry.MaxDelay.Duration() < c.Retry.InitialDelay.Duration() {
		return errors.New("retry.max_delay must be greater than or equal to retry.initial_delay")
	}
	if c.Retry.Multiplier < 1 {
		return errors.New("retry.multiplier must be at least 1")
	}
	if c.Retry.Jitter < 0 || c.Retry.Jitter > 1 {
		return errors.New("retry.jitter must be between 0 and 1")
	}

	durations := map[string]Duration{
		"mount_timeout":         c.MountTimeout,
		"unmount_timeout":       c.UnmountTimeout,
		"shutdown_grace_period": c.ShutdownGracePeriod,
		"reconcile_interval":    c.ReconcileInterval,
	}
	for name, duration := range durations {
		if duration.Duration() <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.MaxConcurrentMounts < 1 {
		return errors.New("max_concurrent_mounts must be at least 1")
	}

	return nil
}
