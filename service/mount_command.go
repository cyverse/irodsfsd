package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	irodsfsd_commons "github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/logstore"
)

type mountClientType string

const (
	mountClientIRODSFS mountClientType = "irodsfs"
	mountClientDAVFS   mountClientType = "davfs"
	mountClientNFS     mountClientType = "nfs"
)

// collectMountSecrets gathers every known secret value in config so a
// mount's log can redact them wherever they appear, as defense in depth
// alongside never placing them on a command line.
func collectMountSecrets(config *api.MountConfig) []string {
	var secrets []string
	if account := config.GetIrodsfs().GetAccount(); account != nil {
		secrets = append(secrets, account.GetIrodsUserPassword(), account.GetIrodsTicket(), account.GetIrodsPamToken())
	}
	if redis := config.GetIrodsfs().GetCache().GetBackend().GetRedis(); redis != nil {
		secrets = append(secrets, redis.GetPassword())
	}
	if davfs := config.GetDavfs(); davfs != nil {
		secrets = append(secrets, davfs.GetPassword())
	}
	return secrets
}

func clientType(config *api.MountConfig) (mountClientType, error) {
	if config == nil {
		return "", errors.New("mount config is required")
	}
	switch config.ClientConfig.(type) {
	case *api.MountConfig_Irodsfs:
		return mountClientIRODSFS, nil
	case *api.MountConfig_Davfs:
		return mountClientDAVFS, nil
	case *api.MountConfig_Nfs:
		return mountClientNFS, nil
	default:
		return "", errors.New("mount client config is required")
	}
}

func (manager *MountManager) makeMountCommand(config *api.MountConfig, mountID string, dataRootPath string, mountLog *logstore.MountLog) (*exec.Cmd, mountClientType, error) {
	client, err := clientType(config)
	if err != nil {
		return nil, "", err
	}

	var command *exec.Cmd
	switch client {
	case mountClientIRODSFS:
		configJSON, err := makeIRODSFSConfigJSON(config, mountID, dataRootPath)
		if err != nil {
			return nil, "", err
		}
		command = exec.Command(manager.config.IRODSFSExecutablePath, "-f", "-c", "-", config.MountPath)
		command.Stdin = bytes.NewReader(configJSON)
	case mountClientDAVFS:
		command, err = manager.makeDAVFSMountCommand(config, dataRootPath)
		if err != nil {
			return nil, "", err
		}
	case mountClientNFS:
		command = manager.makeNFSMountCommand(config)
	default:
		return nil, "", errors.Errorf("unsupported mount client %q", client)
	}

	if mountLog != nil {
		command.Stdout = mountLog.Stdout()
		command.Stderr = mountLog.Stderr()
	} else {
		command.Stdout = io.Discard
		command.Stderr = io.Discard
	}
	return command, client, nil
}

func (manager *MountManager) makeDAVFSMountCommand(config *api.MountConfig, dataRootPath string) (*exec.Cmd, error) {
	davfs := config.GetDavfs()
	cachePath := filepath.Join(dataRootPath, "cache")
	if err := os.MkdirAll(cachePath, 0o777); err != nil {
		return nil, errors.Wrap(err, "failed to create DAVFS cache directory")
	}

	configPath := filepath.Join(dataRootPath, "davfs2.conf")
	if err := writeDAVFSConfig(configPath, davfs.Config, cachePath); err != nil {
		return nil, err
	}

	options := []string{"conf=" + configPath}
	options = append(options, config.MountOptions...)
	options = appendReadOnlyOption(options, config.ReadOnly)
	if username := davfs.GetUsername(); username != "" && username != "anonymous" {
		options = append(options, "username="+username)
	}

	args := makeSystemMountArgs("davfs", options, davfs.Url, config.MountPath)
	command := exec.Command(manager.config.MountExecutablePath, args...)
	if davfs.GetUsername() != "" && davfs.GetUsername() != "anonymous" && davfs.GetPassword() != "" {
		command.Stdin = strings.NewReader(davfs.GetPassword() + "\n")
	}
	return command, nil
}

func (manager *MountManager) makeNFSMountCommand(config *api.MountConfig) *exec.Cmd {
	nfs := config.GetNfs()
	options := append([]string(nil), config.MountOptions...)
	options = appendReadOnlyOption(options, config.ReadOnly)
	if nfs.Port != 0 && nfs.Port != 2049 {
		options = append(options, fmt.Sprintf("port=%d", nfs.Port))
	}
	source := fmt.Sprintf("%s:%s", nfs.Host, nfs.Path)
	args := makeSystemMountArgs("nfs", options, source, config.MountPath)
	return exec.Command(manager.config.MountExecutablePath, args...)
}

func makeSystemMountArgs(fsType string, options []string, source string, target string) []string {
	args := []string{"-t", fsType}
	if len(options) > 0 {
		args = append(args, "-o", strings.Join(options, ","))
	}
	return append(args, source, target)
}

func appendReadOnlyOption(options []string, readOnly bool) []string {
	if !readOnly {
		return options
	}
	for _, option := range options {
		if option == "ro" {
			return options
		}
	}
	return append(options, "ro")
}

func writeDAVFSConfig(path string, params map[string]string, cachePath string) error {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var content strings.Builder
	for _, key := range keys {
		if key == "cache_dir" || key == "kernel_fs" {
			continue
		}
		content.WriteString(key)
		content.WriteByte(' ')
		content.WriteString(params[key])
		content.WriteByte('\n')
	}
	content.WriteString("cache_dir ")
	content.WriteString(cachePath)
	content.WriteByte('\n')
	content.WriteString("kernel_fs fuse\n")

	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		return errors.Wrap(err, "failed to write DAVFS configuration")
	}
	return nil
}

func validateDAVFSConfig(config *api.DAVFSConfig) error {
	if config == nil {
		return errors.New("DAVFS config is required")
	}
	if config.Url == "" {
		return errors.New("DAVFS URL is required")
	}
	if _, err := url.ParseRequestURI(config.Url); err != nil {
		return errors.Wrap(err, "DAVFS URL is invalid")
	}
	for key, value := range config.Config {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, " \t\r\n") {
			return errors.Errorf("invalid DAVFS config key %q", key)
		}
		if strings.ContainsAny(value, "\r\n") {
			return errors.Errorf("DAVFS config value %q contains a newline", key)
		}
	}
	return nil
}

func validateNFSConfig(config *api.NFSConfig) error {
	if config == nil {
		return errors.New("NFS config is required")
	}
	if config.Host == "" {
		return errors.New("NFS host is required")
	}
	if config.Path == "" {
		return errors.New("NFS path is required")
	}
	if config.Port < 0 || config.Port > 65535 {
		return errors.New("NFS port must be between 0 and 65535")
	}
	return nil
}

func (manager *MountManager) unmountClient(ctx context.Context, entry *managedMount) error {
	return manager.unmountByClientType(ctx, entry.client, entry.info.Config.MountPath)
}

// unmountByClientType runs the client-appropriate unmount command without
// requiring a live managedMount, so it can also be used to resume an
// unmount for a record loaded from the repository (e.g. during startup
// reconciliation) that has no supervised exec.Cmd in this process.
func (manager *MountManager) unmountByClientType(ctx context.Context, client mountClientType, mountPath string) error {
	if client == mountClientIRODSFS {
		return manager.fuse.Unmount(mountPath)
	}
	timeout := time.Duration(manager.config.UnmountTimeout)
	if client == mountClientDAVFS {
		timeout = time.Duration(manager.config.DAVFSUnmountTimeout)
		if timeout <= 0 {
			timeout = irodsfsd_commons.DAVFSUnmountTimeoutDefault
		}
	}
	err := manager.runSystemUnmount(ctx, mountPath, false, timeout)
	if client != mountClientDAVFS || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if lazyErr := manager.fuse.Unmount(mountPath); lazyErr != nil {
		return errors.CombineErrors(err, errors.Wrap(lazyErr, "DAVFS lazy unmount failed"))
	}
	return errors.Wrap(ErrDAVFSLazyUnmount, "DAVFS cache synchronization exceeded the unmount timeout")
}

func (manager *MountManager) runSystemUnmount(ctx context.Context, mountPath string, lazy bool, timeout time.Duration) error {
	args := []string{mountPath}
	if lazy {
		args = []string{"-l", mountPath}
	}
	if timeout <= 0 {
		timeout = defaultUnmountTimeout
	}
	unmountContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(unmountContext, manager.config.UnmountExecutablePath, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		if contextErr := unmountContext.Err(); contextErr != nil {
			return errors.Wrapf(contextErr, "%s timed out for %q", manager.config.UnmountExecutablePath, mountPath)
		}
		return errors.Wrapf(err, "%s failed for %q: %s", manager.config.UnmountExecutablePath, mountPath, strings.TrimSpace(string(output)))
	}
	return nil
}
