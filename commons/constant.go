package commons

import "time"

const (
	IRODSFSExecutablePathDefault string = "/usr/local/bin/irodsfs"
	MountExecutablePathDefault   string = "/usr/bin/mount"
	UnmountExecutablePathDefault string = "/usr/bin/umount"
	DataRootPathDefault          string = "/var/lib/irodsfsd"
	PIDFilePathDefault           string = "/run/irodsfsd/irodsfsd.pid"
	AllowedMountRootPathDefault  string = "/mnt/irods"
	KubeletMountRootPathDefault  string = "/var/lib/kubelet"

	MountRootPathDefault     string = "mounts"
	MountDatabasePathDefault string = "db"

	RetryMaxAttemptsDefault  int           = 5
	RetryInitialDelayDefault time.Duration = time.Second
	RetryMaxDelayDefault     time.Duration = 30 * time.Second
	RetryMultiplierDefault   float64       = 2
	RetryJitterDefault       float64       = 0.2

	MountTimeoutDefault        time.Duration = 30 * time.Second
	UnmountTimeoutDefault      time.Duration = 15 * time.Second
	DAVFSUnmountTimeoutDefault time.Duration = 3 * time.Minute
	ReconcileIntervalDefault   time.Duration = 10 * time.Second
	MaxConcurrentMountsDefault int           = 40

	RecoveryEncryptionKeySizeDefault int = 32

	ManagementServicePortDefault int = 13021
)
