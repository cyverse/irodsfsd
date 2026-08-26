package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	godaemonizer "github.com/cyverse/go-daemonizer"
	appconfig "github.com/cyverse/irodsfsd/internal/config"
	"github.com/cyverse/irodsfsd/internal/daemon"
	"github.com/spf13/cobra"
)

const (
	defaultConfigPath = "/etc/irodsfsd/config.yaml"
	defaultStopWait   = 10 * time.Second
)

var (
	version   = "dev"
	gitCommit = "unknown"
)

type startupParams struct {
	ConfigPath string           `json:"config_path"`
	Config     appconfig.Config `json:"config"`
}

type rootOptions struct {
	configPath string
	pidFile    string
}

// Execute parses os.Args and executes the selected command.
func Execute() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve irodsfsd executable: %w", err)
	}
	// go-daemonizer relaunches os.Args[0] after changing to the configured
	// working directory. Always provide an absolute executable path so an
	// invocation such as ./irodsfsd continues to work with --working-directory /.
	os.Args[0] = executable

	// New removes go-daemonizer's private flag from os.Args. This must happen
	// before Cobra parses the command line.
	d := godaemonizer.New()
	return newRootCommand(d, os.Stdout, os.Stderr).Execute()
}

func newRootCommand(d *godaemonizer.Daemon, stdout, stderr io.Writer) *cobra.Command {
	opts := &rootOptions{}
	root := &cobra.Command{
		Use:           "irodsfsd",
		Short:         "Manage irodsfs mounts",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true

	root.PersistentFlags().StringVarP(&opts.configPath, "config", "c", defaultConfigPath, "Path to the daemon configuration file")
	root.PersistentFlags().StringVar(&opts.pidFile, "pid-file", "", "Override the daemon PID file path")

	root.AddCommand(
		newStartCommand(d, opts),
		newRunCommand(opts),
		newStopCommand(opts),
		newStatusCommand(opts),
		newVersionCommand(),
	)
	return root
}

func newStartCommand(d *godaemonizer.Daemon, opts *rootOptions) *cobra.Command {
	var logFile string
	var workingDirectory string

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start irodsfsd as a background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if d.IsDaemon() {
				return runDaemonChild(cmd.Context(), d)
			}

			config, err := loadConfig(opts, logFile, workingDirectory)
			if err != nil {
				return err
			}
			params := startupParams{ConfigPath: opts.configPath, Config: *config}
			return daemonize(cmd, d, params)
		},
	}
	cmd.Flags().StringVar(&logFile, "log-file", "", "Override the daemon stdout/stderr log path")
	cmd.Flags().StringVar(&workingDirectory, "working-directory", "", "Override the daemon working directory")
	return cmd
}

func daemonize(cmd *cobra.Command, d *godaemonizer.Daemon, params startupParams) error {
	logPath := params.Config.DaemonLogFile
	if strings.TrimSpace(logPath) == "" {
		return errors.New("log file path cannot be empty")
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create daemon log directory for %q: %w", logPath, err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open daemon log %q: %w", logPath, err)
	}
	defer logFile.Close()

	cfg := &godaemonizer.Config{
		Dir:    params.Config.WorkingDirectory,
		Env:    os.Environ(),
		Stdin:  nil,
		Stdout: logFile,
		Stderr: logFile,
	}
	if err := d.Daemonize(cmd.Context(), params, cfg); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "irodsfsd started (pid file: %s)\n", params.Config.PIDFile)
	return nil
}

func runDaemonChild(ctx context.Context, d *godaemonizer.Daemon) error {
	var params startupParams
	ready, err := d.WaitForParent(&params)
	if err != nil {
		return fmt.Errorf("receive daemon startup parameters: %w", err)
	}
	return daemon.Run(ctx, daemon.Options{
		ConfigPath: params.ConfigPath,
		Config:     params.Config,
	}, ready)
}

func newRunCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run irodsfsd in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			config, err := loadConfig(opts, "", "")
			if err != nil {
				return err
			}
			return daemon.Run(cmd.Context(), daemon.Options{
				ConfigPath: opts.configPath,
				Config:     *config,
			}, nil)
		},
	}
}

func newStopCommand(opts *rootOptions) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running irodsfsd process",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pidFile, err := resolvePIDFile(opts)
			if err != nil {
				return err
			}
			pid, err := readPID(pidFile)
			if err != nil {
				return err
			}
			if err := signalPID(pid, syscall.SIGTERM); err != nil {
				return err
			}

			deadline := time.Now().Add(wait)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for processRunning(pid) {
				if time.Now().After(deadline) {
					return fmt.Errorf("timed out waiting for irodsfsd process %d to stop", pid)
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-ticker.C:
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "irodsfsd stopped (pid %d)\n", pid)
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", defaultStopWait, "Maximum time to wait for shutdown")
	return cmd
}

func newStatusCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether irodsfsd is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pidFile, err := resolvePIDFile(opts)
			if err != nil {
				return err
			}
			pid, err := readPID(pidFile)
			if err != nil {
				return err
			}
			if !processRunning(pid) {
				return fmt.Errorf("irodsfsd is not running (stale pid %d)", pid)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "irodsfsd is running (pid %d)\n", pid)
			return nil
		},
	}
}

func loadConfig(opts *rootOptions, logFile, workingDirectory string) (*appconfig.Config, error) {
	config, err := appconfig.Load(opts.configPath)
	if err != nil {
		return nil, err
	}
	if opts.pidFile != "" {
		config.PIDFile = opts.pidFile
	}
	if logFile != "" {
		config.DaemonLogFile = logFile
	}
	if workingDirectory != "" {
		config.WorkingDirectory = workingDirectory
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate configuration after command-line overrides: %w", err)
	}
	return config, nil
}

func resolvePIDFile(opts *rootOptions) (string, error) {
	if opts.pidFile != "" {
		return opts.pidFile, nil
	}
	config, err := appconfig.Load(opts.configPath)
	if err != nil {
		return "", err
	}
	return config.PIDFile, nil
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "irodsfsd %s (commit %s)\n", version, gitCommit)
		},
	}
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("irodsfsd is not running: pid file %q does not exist", path)
		}
		return 0, fmt.Errorf("read pid file %q: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("pid file %q contains an invalid pid", path)
	}
	return pid, nil
}

func signalPID(pid int, signal syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Signal(signal); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("irodsfsd is not running (stale pid %d)", pid)
		}
		return fmt.Errorf("signal process %d: %w", pid, err)
	}
	return nil
}

func processRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
