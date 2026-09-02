package logstore

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
)

// QueryOptions bounds a log query. Tail (if positive) keeps only the last N
// matching lines; Limit (if positive) caps the total number of lines
// returned. Since (if non-zero) drops lines at or before that time.
type QueryOptions struct {
	Since time.Time
	Tail  int
	Limit int
}

// Query reads the log file at path (only its current, not-yet-rotated
// contents; rotation already bounds how much history exists) and returns
// the records matching options. A missing file is not an error: it simply
// means nothing has been logged yet.
func Query(path string, options QueryOptions) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, "failed to read mount log %q", path)
	}

	var records []Record
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue // a partial or corrupt line never stops the rest of the query
		}
		if !options.Since.IsZero() && !record.Time.After(options.Since) {
			continue
		}
		records = append(records, record)
	}

	if options.Tail > 0 && len(records) > options.Tail {
		records = records[len(records)-options.Tail:]
	}
	if options.Limit > 0 && len(records) > options.Limit {
		records = records[:options.Limit]
	}
	return records, nil
}
