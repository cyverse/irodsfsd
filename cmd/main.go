package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	godaemonizer "github.com/cyverse/go-daemonizer"
	cmd_commons "github.com/cyverse/irodsfsd/cmd/commons"
	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const defaultStopWait = 10 * time.Second

func Execute(d *godaemonizer.Daemon) error {
	return newRootCommand(d).Execute()
}

func newRootCommand(d *godaemonizer.Daemon) *cobra.Command {
	root := &cobra.Command{
		Use:           "irodsfsd",
		Short:         "Run iRODS FUSE mount management daemon",
		Long:          "Run iRODS FUSE mount management daemon that manages multiple irodsfs mounts.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd:   true,
			DisableNoDescFlag:   true,
			DisableDescriptions: true,
			HiddenDefaultCmd:    true,
		},
	}

	cmd_commons.SetCommonFlags(root)
	root.AddCommand(
		newStartCommand(d),
		newRunCommand(),
		newStopCommand(),
		newStatusCommand(),
		newVersionCommand(),
	)
	return root
}

func newStartCommand(d *godaemonizer.Daemon) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start irodsfsd as a background daemon",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if d.IsDaemon() {
				return runDaemonChild(command, d)
			}

			config, err := loadConfig(command)
			if err != nil {
				return errors.Wrapf(err, "process command flags")
			}
			logWriter, err := config.GetLogWriter(true)
			if err != nil {
				return errors.Wrapf(err, "get parent log writer")
			}
			if logWriter != nil {
				defer logWriter.Close()
				log.SetOutput(logWriter)
			}

			if err := d.Daemonize(context.Background(), config, nil); err != nil {
				return errors.Wrapf(err, "daemonize irodsfsd")
			}

			fmt.Fprintf(command.OutOrStdout(), "irodsfsd started (pid file: %s)\n", config.PIDFile)
			return nil
		},
	}
}

func runDaemonChild(command *cobra.Command, d *godaemonizer.Daemon) error {
	var config commons.Config
	ready, err := d.WaitForParent(&config)
	if err != nil {
		return errors.Wrapf(err, "receive daemon startup parameters")
	}

	logWriter, err := config.GetLogWriter(false)
	if err != nil {
		ready(err)
		return errors.Wrapf(err, "get daemon log writer")
	}
	if logWriter != nil {
		defer logWriter.Close()
		log.SetOutput(logWriter)
	}

	return runDaemonManaged(&config, ready)
}

func newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run irodsfsd in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			config, err := loadConfig(command)
			if err != nil {
				return errors.Wrapf(err, "process command flags")
			}
			if err := configureForegroundPaths(config); err != nil {
				return errors.Wrapf(err, "configure foreground paths")
			}

			logWriter, err := config.GetLogWriter(true)
			if err != nil {
				return errors.Wrapf(err, "get foreground log writer")
			}
			if logWriter != nil {
				defer logWriter.Close()
				log.SetOutput(logWriter)
			}

			return runForeground(config)
		},
	}
}

func loadConfig(command *cobra.Command) (*commons.Config, error) {
	return cmd_commons.ProcessCommonFlags(command)
}

func configureForegroundPaths(config *commons.Config) error {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return errors.Wrapf(err, "get current working directory")
	}

	oldDataRootPath := config.DataRootPath
	oldDefaultMountRootPath := filepath.Join(oldDataRootPath, commons.MountRootPathDefault)

	config.DataRootPath = workingDirectory
	if config.MountRootPath == "" || filepath.Clean(config.MountRootPath) == filepath.Clean(oldDefaultMountRootPath) {
		config.MountRootPath = filepath.Join(workingDirectory, commons.MountRootPathDefault)
	}

	return nil
}

func newStopCommand() *cobra.Command {
	var wait time.Duration
	command := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running irodsfsd process",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			config, err := cmd_commons.ProcessCommonFlags(command)
			if err != nil {
				return errors.Wrapf(err, "process command flags")
			}

			pid, err := cmd_commons.ReadPID(config.PIDFile)
			if err != nil {
				return err
			}
			if !cmd_commons.ProcessRunning(pid) {
				return errors.Newf("irodsfsd is not running (stale pid %d)", pid)
			}
			if err := cmd_commons.SignalPID(pid, syscall.SIGTERM); err != nil {
				return err
			}

			deadline := time.Now().Add(wait)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for cmd_commons.ProcessRunning(pid) {
				if time.Now().After(deadline) {
					return errors.Newf("timed out waiting for irodsfsd process %d to stop", pid)
				}
				select {
				case <-command.Context().Done():
					return command.Context().Err()
				case <-ticker.C:
				}
			}

			fmt.Fprintf(command.OutOrStdout(), "irodsfsd stopped (pid %d)\n", pid)
			return nil
		},
	}
	command.Flags().DurationVar(&wait, "wait", defaultStopWait, "Maximum time to wait for shutdown")
	return command
}

func newStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether irodsfsd is running",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			config, err := cmd_commons.ProcessCommonFlags(command)
			if err != nil {
				return errors.Wrapf(err, "process command flags")
			}

			pid, err := cmd_commons.ReadPID(config.PIDFile)
			if err != nil {
				return err
			}
			if !cmd_commons.ProcessRunning(pid) {
				return errors.Newf("irodsfsd is not running (stale pid %d)", pid)
			}

			fmt.Fprintf(command.OutOrStdout(), "irodsfsd is running (pid %d)\n", pid)
			return nil
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return cmd_commons.PrintVersion(command)
		},
	}
}

func main() {
	myFormatter := &commons.StacktraceTextFormatter{
		TextFormatter: log.TextFormatter{
			TimestampFormat: "2006-01-02 15:04:05.000000",
			FullTimestamp:   true,
		},
	}

	log.SetFormatter(myFormatter)
	log.SetLevel(log.InfoLevel)
	log.SetReportCaller(true)

	logger := log.WithFields(log.Fields{})

	// go-daemonizer relaunches os.Args[0]. Use an absolute path so daemon
	// startup does not depend on the configured working directory.
	executable, err := os.Executable()
	if err != nil {
		logger.WithError(err).Fatal("failed to resolve executable path")
	}
	os.Args[0] = executable

	// must be called before Cobra parses os.Args so --__daemon__ is stripped.
	daemon := godaemonizer.New()
	if err := Execute(daemon); err != nil {
		fmt.Fprintf(os.Stderr, "irodsfsd: %v\n", err)
		os.Exit(1)
	}
}

func runDaemonManaged(config *commons.Config, ready func(error)) error {
	pidFile, err := cmd_commons.AcquirePIDFile(config.PIDFile)
	if err != nil {
		reportReady(ready, err)
		return err
	}
	defer pidFile.Close()

	return runUntilShutdown(config, ready)
}

func runForeground(config *commons.Config) error {
	return runUntilShutdown(config, nil)
}

func runUntilShutdown(config *commons.Config, ready func(error)) error {
	runErr, shutdownFn := run(config)
	if runErr != nil {
		reportReady(ready, runErr)
		return runErr
	}

	reportReady(ready, nil)
	waitForShutdown()

	if shutdownFn != nil {
		shutdownFn()
	}
	return nil
}

func reportReady(ready func(error), err error) {
	if ready != nil {
		ready(err)
	}
}

// run runs iRODS FUSE mount management daemon Service.
func run(config *commons.Config) (error, func()) {
	logger := log.WithFields(log.Fields{})

	if config.Debug {
		log.SetLevel(log.DebugLevel)
	}

	versionInfo := commons.GetVersion()
	logger.Infof("iRODS FUSE Mount Management Service version - %q, commit - %q", versionInfo.ServiceVersion, versionInfo.GitCommit)

	if err := config.MakeWorkDirs(); err != nil {
		mkdirErr := errors.Wrap(err, "make work dir error")
		logger.Error(mkdirErr)
		return mkdirErr, nil
	}

	if err := config.Validate(); err != nil {
		configErr := errors.Wrap(err, "invalid configuration")
		logger.Error(configErr)
		return configErr, nil
	}

	svc, err := service.NewService(config)
	if err != nil {
		serviceErr := errors.Wrap(err, "failed to create the service")
		logger.Error(serviceErr)
		return serviceErr, nil
	}
	if err := svc.Start(); err != nil {
		serviceErr := errors.Wrap(err, "failed to start the service")
		logger.Error(serviceErr)
		svc.Release()
		return serviceErr, nil
	}

	shutdown := func() {
		svc.Stop()
		svc.Release()
		logger.Info("irodsfsd stopped")
	}
	return nil, shutdown
}

func waitForShutdown() {
	signalChannel := make(chan os.Signal, 1)

	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChannel)
	<-signalChannel
}
