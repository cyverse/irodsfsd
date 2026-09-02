package logstore

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestMountLogSplitsWritesIntoLinesAndRedactsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mounts", "m1", "irodsfs.log")
	log, err := Open(path, []string{"super-secret"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	// A single Write spanning multiple lines, and a write split mid-line
	// across two calls, must both become whole line records.
	if _, err := log.Stdout().Write([]byte("connecting with password super-secret\nready\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Stderr().Write([]byte("warn: sl")); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Stderr().Write([]byte("ow response\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records, err := Query(path, QueryOptions{})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("len(records) = %d, want 3: %+v", len(records), records)
	}
	if records[0].Stream != "stdout" || records[0].Message != "connecting with password [REDACTED]" {
		t.Errorf("records[0] = %+v, want redacted stdout line", records[0])
	}
	if records[1].Stream != "stdout" || records[1].Message != "ready" {
		t.Errorf("records[1] = %+v, want %q", records[1], "ready")
	}
	if records[2].Stream != "stderr" || records[2].Message != "warn: slow response" {
		t.Errorf("records[2] = %+v, want the two writes joined into one line", records[2])
	}
}

func TestMountLogFlushesTrailingPartialLineOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "irodsfs.log")
	log, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := log.Stdout().Write([]byte("no trailing newline")); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	records, err := Query(path, QueryOptions{})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(records) != 1 || records[0].Message != "no trailing newline" {
		t.Errorf("records = %+v, want one flushed partial line", records)
	}
}

func TestMountLogAppendsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "irodsfs.log")
	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := first.Stdout().Write([]byte("before restart\n")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := second.Stdout().Write([]byte("after restart\n")); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	records, err := Query(path, QueryOptions{})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(records) != 2 || records[0].Message != "before restart" || records[1].Message != "after restart" {
		t.Errorf("records = %+v, want both pre- and post-restart lines", records)
	}
}

func TestQueryMissingFileReturnsNoRecords(t *testing.T) {
	records, err := Query(filepath.Join(t.TempDir(), "missing.log"), QueryOptions{})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil for a missing log file", err)
	}
	if records != nil {
		t.Errorf("records = %+v, want nil", records)
	}
}

func TestQueryTailSinceAndLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "irodsfs.log")
	log, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		log.now = fixedTime(base.Add(time.Duration(i) * time.Second))
		if _, err := log.Stdout().Write([]byte(fmt.Sprintf("line-%d\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	tail, err := Query(path, QueryOptions{Tail: 2})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(tail) != 2 || tail[0].Message != "line-3" || tail[1].Message != "line-4" {
		t.Errorf("Tail: 2 result = %+v, want the last two lines", tail)
	}

	since, err := Query(path, QueryOptions{Since: base.Add(2 * time.Second)})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(since) != 2 || since[0].Message != "line-3" || since[1].Message != "line-4" {
		t.Errorf("Since result = %+v, want lines strictly after the cutoff", since)
	}

	limited, err := Query(path, QueryOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(limited) != 2 || limited[0].Message != "line-0" || limited[1].Message != "line-1" {
		t.Errorf("Limit: 2 result = %+v, want the first two lines", limited)
	}
}

func fixedTime(value time.Time) func() time.Time {
	return func() time.Time { return value }
}
