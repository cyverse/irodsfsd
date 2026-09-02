package commons

import (
	"bytes"
	"fmt"

	log "github.com/sirupsen/logrus"
)

type StacktraceTextFormatter struct {
	TextFormatter log.TextFormatter
}

func (f *StacktraceTextFormatter) Format(entry *log.Entry) ([]byte, error) {
	baseBytes, err := f.TextFormatter.Format(entry)
	if err != nil {
		return nil, err
	}

	if entry.Level <= log.ErrorLevel {
		// error?
		if errVal, exists := entry.Data[log.ErrorKey]; exists {
			if err, ok := errVal.(error); ok {
				var buf bytes.Buffer
				buf.Write(baseBytes)

				// extract stacktrace
				stackTrace := fmt.Sprintf("%+v", err)

				buf.WriteString("--- Stack Trace ---\n")
				buf.WriteString(stackTrace)
				buf.WriteString("\n-------------------\n")

				return buf.Bytes(), nil
			}
		}
	}

	return baseBytes, nil
}
