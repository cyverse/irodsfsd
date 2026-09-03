package commons

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v3"
)

type RetryConfig struct {
	MaxAttempts  int      `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	InitialDelay Duration `yaml:"initial_delay,omitempty" json:"initial_delay,omitempty"`
	MaxDelay     Duration `yaml:"max_delay,omitempty" json:"max_delay,omitempty"`
	Multiplier   float64  `yaml:"multiplier,omitempty" json:"multiplier,omitempty"`
	Jitter       float64  `yaml:"jitter,omitempty" json:"jitter,omitempty"`
}

// Config holds the parameters list which can be configured
type Config struct {
	ServiceEndpoint       string `yaml:"service_endpoint,omitempty" json:"service_endpoint,omitempty"`
	IRODSFSExecutablePath string `yaml:"irodsfs_executable_path,omitempty" json:"irodsfs_executable_path,omitempty"`
	MountExecutablePath   string `yaml:"mount_executable_path,omitempty" json:"mount_executable_path,omitempty"`
	UnmountExecutablePath string `yaml:"unmount_executable_path,omitempty" json:"unmount_executable_path,omitempty"`
	DataRootPath          string `yaml:"data_root_path,omitempty" json:"data_root_path,omitempty"`
	PIDFile               string `yaml:"pid_file,omitempty" json:"pid_file,omitempty"`

	RecoveryEncryptionKey string `yaml:"recovery_encryption_key,omitempty" json:"recovery_encryption_key,omitempty"`

	AllowedMountRootPaths []string `yaml:"allowed_mount_root_paths,omitempty" json:"allowed_mount_root_paths,omitempty"`
	// AllowFuseAllowOther is design.md's "daemon-wide policy" gate: a mount
	// request may only ask for the FUSE allow_other option (irodsfs or
	// DAVFS) when this is true. allow_other lets every local user on the
	// host reach the mount, not just the irodsfsd service account that
	// actually performed it, so it must be an explicit, host-level opt-in
	// rather than something any mount request can request on its own.
	AllowFuseAllowOther bool        `yaml:"allow_fuse_allow_other,omitempty" json:"allow_fuse_allow_other,omitempty"`
	Retry               RetryConfig `yaml:"retry,omitempty" json:"retry,omitempty"`
	MountTimeout        Duration    `yaml:"mount_timeout,omitempty" json:"mount_timeout,omitempty"`
	UnmountTimeout      Duration    `yaml:"unmount_timeout,omitempty" json:"unmount_timeout,omitempty"`
	DAVFSUnmountTimeout Duration    `yaml:"davfs_unmount_timeout,omitempty" json:"davfs_unmount_timeout,omitempty"`
	ReconcileInterval   Duration    `yaml:"reconcile_interval,omitempty" json:"reconcile_interval,omitempty"`
	MaxConcurrentMounts int         `yaml:"max_concurrent_mounts,omitempty" json:"max_concurrent_mounts,omitempty"`

	ManagementServicePort int `yaml:"management_service_port,omitempty" json:"management_service_port,omitempty"`

	Debug         bool   `yaml:"debug,omitempty" json:"debug,omitempty"`
	LogRootPath   string `yaml:"log_root_path,omitempty" json:"log_root_path,omitempty"`
	MountRootPath string `yaml:"mount_root_path,omitempty" json:"mount_root_path,omitempty"`
}

func NewDefaultConfig() *Config {
	return &Config{
		ServiceEndpoint:       "",
		IRODSFSExecutablePath: IRODSFSExecutablePathDefault,
		MountExecutablePath:   MountExecutablePathDefault,
		UnmountExecutablePath: UnmountExecutablePathDefault,
		DataRootPath:          DataRootPathDefault,
		PIDFile:               PIDFilePathDefault,

		AllowedMountRootPaths: []string{AllowedMountRootPathDefault, KubeletMountRootPathDefault},
		AllowFuseAllowOther:   false,
		Retry: RetryConfig{
			MaxAttempts:  RetryMaxAttemptsDefault,
			InitialDelay: Duration(RetryInitialDelayDefault),
			MaxDelay:     Duration(RetryMaxDelayDefault),
			Multiplier:   RetryMultiplierDefault,
			Jitter:       RetryJitterDefault,
		},
		MountTimeout:        Duration(MountTimeoutDefault),
		UnmountTimeout:      Duration(UnmountTimeoutDefault),
		DAVFSUnmountTimeout: Duration(DAVFSUnmountTimeoutDefault),
		ReconcileInterval:   Duration(ReconcileIntervalDefault),
		MaxConcurrentMounts: MaxConcurrentMountsDefault,

		ManagementServicePort: ManagementServicePortDefault,

		Debug: false,

		LogRootPath:   "", // use default
		MountRootPath: filepath.Join(DataRootPathDefault, MountRootPathDefault),
	}
}

// NewConfigFromFile creates Config from file
func NewConfigFromFile(config *Config, filePath string) (*Config, error) {
	st, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}

		return nil, errors.Wrapf(err, "failed to stat file %q", filePath)
	}

	if st.IsDir() {
		return nil, errors.Newf("configuration must be a file %q", filePath)
	}

	dataBytes, err := os.ReadFile(filePath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read file %q", filePath)
	}

	format := DetectFormat(dataBytes)
	switch format {
	case FormatJSON:
		return NewConfigFromJSONFile(config, filePath)
	case FormatYAML:
		return NewConfigFromYAMLFile(config, filePath)
	default:
		return nil, errors.New("unknown file format")
	}
}

// NewConfigFromYAMLFile creates Config from YAML
func NewConfigFromYAMLFile(config *Config, yamlPath string) (*Config, error) {
	cfg := Config{}
	if config != nil {
		cfg = *config
	}

	yamlBytes, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read YAML file %q", yamlPath)
	}

	err = yaml.Unmarshal(yamlBytes, &cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal YAML file %q to config", yamlPath)
	}

	return &cfg, nil
}

// NewConfigFromJSONFile creates Config from JSON
func NewConfigFromJSONFile(config *Config, jsonPath string) (*Config, error) {
	cfg := Config{}
	if config != nil {
		cfg = *config
	}

	jsonBytes, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read JSON file %q", jsonPath)
	}

	err = json.Unmarshal(jsonBytes, &cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal JSON file %q to config", jsonPath)
	}

	return &cfg, nil
}

// NewConfigFromYAML creates Config from YAML
func NewConfigFromYAML(config *Config, yamlBytes []byte) (*Config, error) {
	cfg := Config{}
	if config != nil {
		cfg = *config
	}

	err := yaml.Unmarshal(yamlBytes, &cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal yaml into config")
	}

	return &cfg, nil
}

// NewConfigFromJSON creates Config from JSON
func NewConfigFromJSON(config *Config, jsonBytes []byte) (*Config, error) {
	cfg := Config{}
	if config != nil {
		cfg = *config
	}

	err := json.Unmarshal(jsonBytes, &cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal json into config")
	}

	return &cfg, nil
}

// GetMountRootPath returns the parent directory for managed irodsfs data roots.
func (config *Config) GetMountRootPath() string {
	if len(config.MountRootPath) > 0 {
		return config.MountRootPath
	}

	return filepath.Join(config.DataRootPath, MountRootPathDefault)
}

// GetMountDatabasePath returns the directory holding the embedded mount
// database that persists mount intent across daemon restarts.
func (config *Config) GetMountDatabasePath() string {
	return filepath.Join(config.GetDataRootPath(), MountDatabasePathDefault)
}

// GetLogRootPath returns the directory containing service and session logs.
func (config *Config) GetLogRootPath() string {
	if len(config.LogRootPath) > 0 {
		return config.LogRootPath
	}

	// default
	return config.DataRootPath
}

// GetLogFilePath returns the daemon stdout/stderr log file path.
func (config *Config) GetLogFilePath() string {
	return filepath.Join(config.GetLogRootPath(), "irodsfsd.log")
}

// GetMountLogPath returns the log file path for one mount's child
// stdout/stderr, stable across daemon restarts and mount retries so its
// history remains queryable.
func (config *Config) GetMountLogPath(mountID string) string {
	return filepath.Join(config.GetLogRootPath(), "mounts", mountID, "irodsfs.log")
}

func (config *Config) GetServiceEndpoint() string {
	if len(config.ServiceEndpoint) > 0 {
		return config.ServiceEndpoint
	}

	return fmt.Sprintf("unix://%s/comm.sock", config.DataRootPath)
}

func (config *Config) GetDataRootPath() string {
	return config.DataRootPath
}

// GetRecoveryEncryptionKey decodes the configured 256-bit Badger encryption key.
func (config *Config) GetRecoveryEncryptionKey() ([]byte, error) {
	encodedKey := strings.TrimSpace(config.RecoveryEncryptionKey)
	if encodedKey == "" {
		return nil, errors.New("recovery encryption key must be given")
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return nil, errors.Wrap(err, "recovery encryption key must be valid base64")
	}
	if len(key) != RecoveryEncryptionKeySizeDefault {
		return nil, errors.Newf("recovery encryption key must decode to exactly %d bytes, got %d", RecoveryEncryptionKeySizeDefault, len(key))
	}
	return key, nil
}

// MakeLogDir makes a log dir required
func (config *Config) MakeLogDir() error {
	return config.makeDir(config.GetLogRootPath())
}

// MakeWorkDirs makes dirs required
func (config *Config) MakeWorkDirs() error {
	dataRootPath := config.GetDataRootPath()
	err := config.makeDir(dataRootPath)
	if err != nil {
		return err
	}

	mountRootPath := config.GetMountRootPath()
	err = config.makeDir(mountRootPath)
	if err != nil {
		return err
	}

	scheme, endpoint, err := ParseServiceEndpoint(config.GetServiceEndpoint())
	if err != nil {
		return err
	}

	if scheme == "unix" {
		err = config.makeUnixSocketDir(endpoint)
		if err != nil {
			return err
		}
	}

	return nil
}

// makeDir makes a dir for use
func (config *Config) makeDir(path string) error {
	if len(path) == 0 {
		return errors.Errorf("failed to create a dir with empty path")
	}

	dirInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// make
			mkdirErr := os.MkdirAll(path, 0775)
			if mkdirErr != nil {
				return errors.Wrapf(mkdirErr, "making a dir %q error", path)
			}

			return nil
		}

		return errors.Wrapf(err, "stating a dir %q error", path)
	}

	if !dirInfo.IsDir() {
		return errors.Errorf("a file %q exist, not a directory", path)
	}

	dirPerm := dirInfo.Mode().Perm()
	if dirPerm&0200 != 0200 {
		return errors.Errorf("a dir %q exist, but does not have the write permission", path)
	}

	return nil
}

// makeUnixSocketDir makes unix socket dir
func (config *Config) makeUnixSocketDir(endpoint string) error {
	// endpoint is a file
	_, err := os.Stat(endpoint)
	if err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrapf(err, "service unix socket file %q error", endpoint)
		}
	} else {
		// file exists
		// remove
		err2 := os.Remove(endpoint)
		if err2 != nil {
			return errors.Wrapf(err2, "failed to remove the existing unix socket file %q", endpoint)
		}
	}

	parentDir := filepath.Dir(endpoint)
	unixSocketDirInfo, err := os.Stat(parentDir)
	if err != nil {
		if os.IsNotExist(err) {
			err2 := os.MkdirAll(parentDir, os.FileMode(0777))
			if err2 != nil {
				return errors.Wrapf(err2, "failed to make a directory for unix socket %q", parentDir)
			}
			// ok - fall
		} else {
			return errors.Wrapf(err, "unix socket directory %q error", parentDir)
		}
	} else {
		unixSocketDirPerm := unixSocketDirInfo.Mode().Perm()
		if unixSocketDirPerm&0200 != 0200 {
			return errors.Errorf("unix socket directory %q must have write permission", parentDir)
		}
		// ok - fall
	}

	return nil
}

// Validate validates configuration
func (config *Config) Validate() error {
	_, _, err := ParseServiceEndpoint(config.GetServiceEndpoint())
	if err != nil {
		return err
	}

	paths := map[string]string{
		"irodsfs_executable_path": config.IRODSFSExecutablePath,
		"mount_executable_path":   config.MountExecutablePath,
		"unmount_executable_path": config.UnmountExecutablePath,
		"data_root_path":          config.DataRootPath,
		"pid_file":                config.PIDFile,
		"log_root_path":           config.GetLogRootPath(),
		"mount_root_path":         config.GetMountRootPath(),
	}

	for name, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s %q must be an absolute path", name, path)
		}
	}

	if len(config.DataRootPath) == 0 {
		return errors.Errorf("data root dir must be given")
	}

	if len(config.PIDFile) == 0 {
		return errors.Errorf("pid file path must be given")
	}

	if _, err := config.GetRecoveryEncryptionKey(); err != nil {
		return err
	}

	if len(config.AllowedMountRootPaths) == 0 {
		return errors.New("allowed_mount_root_paths must contain at least one path")
	}
	for _, root := range config.AllowedMountRootPaths {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("allowed mount root %q must be an absolute path", root)
		}
		if filepath.Clean(root) == string(filepath.Separator) {
			return errors.New("filesystem root cannot be an allowed mount root")
		}
	}

	if config.Retry.MaxAttempts < 1 {
		return errors.New("retry.max_attempts must be at least 1")
	}
	if config.Retry.InitialDelay <= 0 || config.Retry.MaxDelay <= 0 {
		return errors.New("retry delays must be positive")
	}
	if config.Retry.MaxDelay < config.Retry.InitialDelay {
		return errors.New("retry.max_delay must be greater than or equal to retry.initial_delay")
	}
	if config.Retry.Multiplier < 1 {
		return errors.New("retry.multiplier must be at least 1")
	}
	if config.Retry.Jitter < 0 || config.Retry.Jitter > 1 {
		return errors.New("retry.jitter must be between 0 and 1")
	}

	durations := map[string]Duration{
		"mount_timeout":         config.MountTimeout,
		"unmount_timeout":       config.UnmountTimeout,
		"davfs_unmount_timeout": config.DAVFSUnmountTimeout,
		"reconcile_interval":    config.ReconcileInterval,
	}
	for name, duration := range durations {
		if duration <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if config.MaxConcurrentMounts < 1 {
		return errors.New("max_concurrent_mounts must be at least 1")
	}
	if config.ManagementServicePort < 0 || config.ManagementServicePort > 65535 {
		return errors.New("management_service_port must be between 0 and 65535")
	}

	return nil
}

// MultiWriteCloser writes to multiple writers and closes the ones that implement io.Closer.
type MultiWriteCloser struct {
	writers []io.Writer
}

// nonClosingWriter prevents MultiWriteCloser from closing a writer it does not own.
// In particular, foreground logging must never close os.Stderr.
type nonClosingWriter struct {
	io.Writer
}

func NewMultiWriteCloser(writers ...io.Writer) *MultiWriteCloser {
	return &MultiWriteCloser{writers: writers}
}

func (mw *MultiWriteCloser) Write(p []byte) (n int, err error) {
	for _, w := range mw.writers {
		n, err = w.Write(p)
		if err != nil {
			return n, err
		}
	}
	return len(p), nil
}

func (mw *MultiWriteCloser) Close() error {
	var firstErr error
	for _, w := range mw.writers {
		if closer, ok := w.(io.Closer); ok {
			if err := closer.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (config *Config) GetLogWriter(foregroundProcess bool) (io.WriteCloser, error) {
	logFilePath := config.GetLogFilePath()
	if logFilePath == "-" || len(logFilePath) == 0 {
		return os.Stderr, nil
	}

	err := config.MakeLogDir()
	if err != nil {
		return nil, err
	}

	if foregroundProcess {
		fileWriter := getLogWriterForForegroundProcess(logFilePath)
		return NewMultiWriteCloser(nonClosingWriter{Writer: os.Stderr}, fileWriter), nil
	}

	daemonWriter := getLogWriterForDaemonProcess(logFilePath)
	return daemonWriter, nil
}

func getLogWriterForForegroundProcess(logPath string) io.WriteCloser {
	logFilePath := fmt.Sprintf("%s.fg", logPath)
	return &lumberjack.Logger{
		Filename:   logFilePath,
		MaxSize:    50, // 50MB
		MaxBackups: 5,
		MaxAge:     30, // 30 days
		Compress:   false,
	}
}

func getLogWriterForDaemonProcess(logPath string) io.WriteCloser {
	logFilePath := fmt.Sprintf("%s", logPath)
	return &lumberjack.Logger{
		Filename:   logFilePath,
		MaxSize:    50, // 50MB
		MaxBackups: 10,
		MaxAge:     365, // 365 days
		Compress:   false,
	}
}
