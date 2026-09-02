package service

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/store"
)

func scrapeMetrics(t *testing.T, metrics *Metrics) string {
	t.Helper()
	request := httptest.NewRequest("GET", "/metrics", nil)
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("metrics scrape status = %d, body = %s", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func TestMetricsRecordMountAndUnmountOperations(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), false)
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	manager, err := newMountManager(config, &fakeFuseController{}, func(path string) (bool, error) {
		return path == mountPath, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.metrics = NewMetrics(manager)

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_MOUNTED)

	mountedScrape := scrapeMetrics(t, manager.metrics)
	if !strings.Contains(mountedScrape, `irodsfsd_mount_operations_total{operation="mount",result="accepted"} 1`) {
		t.Errorf("mount operation counter missing or wrong: %s", grepLine(mountedScrape, "irodsfsd_mount_operations_total"))
	}
	if !strings.Contains(mountedScrape, `irodsfsd_mounts{state="MOUNT_STATE_MOUNTED"} 1`) {
		t.Errorf("mounts gauge missing or wrong: %s", grepLine(mountedScrape, "irodsfsd_mounts"))
	}

	// Reject a second Mount for the same ID, so the "rejected" result label
	// is exercised too.
	if _, err := manager.Mount(context.Background(), &api.MountRequest{
		MountId: &result.MountId,
		Config:  newTestIRODSFSMountConfig(mountPath),
	}); err == nil {
		t.Fatal("expected the duplicate Mount to be rejected")
	}

	if _, err := manager.Unmount(context.Background(), &api.UnmountRequest{MountId: result.MountId}); err != nil {
		t.Fatal(err)
	}

	finalScrape := scrapeMetrics(t, manager.metrics)
	if !strings.Contains(finalScrape, `irodsfsd_mount_operations_total{operation="mount",result="rejected"} 1`) {
		t.Errorf("rejected mount counter missing: %s", grepLine(finalScrape, "irodsfsd_mount_operations_total"))
	}
	if !strings.Contains(finalScrape, `irodsfsd_mount_operations_total{operation="unmount",result="accepted"} 1`) {
		t.Errorf("unmount operation counter missing: %s", grepLine(finalScrape, "irodsfsd_mount_operations_total"))
	}
	if strings.Contains(finalScrape, `irodsfsd_mounts{state="MOUNT_STATE_MOUNTED"}`) {
		t.Errorf("mounts gauge still reports a MOUNTED mount after unmount: %s", grepLine(finalScrape, "irodsfsd_mounts"))
	}
}

func TestMetricsRecordChildCrash(t *testing.T) {
	tempDir := t.TempDir()
	executablePath := makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), true) // always crashes
	mountPath := filepath.Join(tempDir, "mount")
	if err := os.Mkdir(mountPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = executablePath
	config.MountRootPath = filepath.Join(tempDir, "data")
	config.LogRootPath = filepath.Join(tempDir, "logs")
	config.AllowedMountRootPaths = []string{tempDir}
	config.Retry.MaxAttempts = 1
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, nil
	}, time.Now, newTestRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.metrics = NewMetrics(manager)

	result, err := manager.Mount(context.Background(), &api.MountRequest{Config: newTestIRODSFSMountConfig(mountPath)})
	if err != nil {
		t.Fatal(err)
	}
	waitForMountState(t, manager, result.MountId, api.MountState_MOUNT_STATE_FAILED)

	scrape := scrapeMetrics(t, manager.metrics)
	if !strings.Contains(scrape, "irodsfsd_child_crashes_total 1") {
		t.Errorf("child crash counter missing or wrong: %s", grepLine(scrape, "irodsfsd_child_crashes_total"))
	}
}

func TestMetricsRecordReconcileError(t *testing.T) {
	tempDir := t.TempDir()
	repository := newTestRepository(t)
	if err := repository.Create(context.Background(), &store.MountRecord{Info: &api.MountInfo{
		MountId: "mount-1",
		State:   api.MountState_MOUNT_STATE_MOUNTED,
		Config:  newTestIRODSFSMountConfig(filepath.Join(tempDir, "mount")),
	}}); err != nil {
		t.Fatal(err)
	}

	config := commons.NewDefaultConfig()
	config.IRODSFSExecutablePath = makeFakeIRODSFS(t, tempDir, filepath.Join(tempDir, "stdin"), filepath.Join(tempDir, "args"), true)
	config.AllowedMountRootPaths = []string{tempDir}
	probeErr := errors.New("mount table is unreadable")
	manager, err := newMountManager(config, &fakeFuseController{}, func(string) (bool, error) {
		return false, probeErr
	}, time.Now, repository)
	if err != nil {
		t.Fatal(err)
	}
	manager.metrics = NewMetrics(manager)

	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	scrape := scrapeMetrics(t, manager.metrics)
	if !strings.Contains(scrape, "irodsfsd_reconcile_errors_total 1") {
		t.Errorf("reconcile error counter missing or wrong: %s", grepLine(scrape, "irodsfsd_reconcile_errors_total"))
	}
}

func grepLine(text string, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return "(not found)"
}
