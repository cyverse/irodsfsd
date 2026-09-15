package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveIndex(t *testing.T) string {
	t.Helper()

	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("management UI returned %d, want %d", recorder.Code, http.StatusOK)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("management UI served %q, want text/html", contentType)
	}

	return recorder.Body.String()
}

func TestHandlerServesTheEmbeddedPage(t *testing.T) {
	body := serveIndex(t)

	if !strings.Contains(body, "<table>") || !strings.Contains(body, `id="mount-rows"`) {
		t.Fatalf("management UI does not contain the mounts table")
	}
}

// A mount path, a WebDAV URL and an NFS host:path all arrive without spaces to
// break on. The table is 100% wide inside a page capped at 1200px and has no
// horizontal scroll container, so a cell that cannot wrap widens the table past
// the page instead.
func TestTableCellsWrapUnbreakableValues(t *testing.T) {
	body := serveIndex(t)

	for _, expected := range []string{"overflow-wrap: anywhere", "word-break: break-word"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("management UI does not set %q on its table cells", expected)
		}
	}

	// The rule has to sit on the cells themselves. It used to apply to one
	// column, which left the client column to overflow.
	cellRule := strings.Index(body, "th, td {")
	if cellRule < 0 {
		t.Fatalf("management UI has no th, td rule to carry the wrapping")
	}
	ruleEnd := strings.Index(body[cellRule:], "}")
	if ruleEnd < 0 {
		t.Fatalf("management UI has an unterminated th, td rule")
	}
	rule := body[cellRule : cellRule+ruleEnd]
	for _, expected := range []string{"overflow-wrap: anywhere", "word-break: break-word"} {
		if !strings.Contains(rule, expected) {
			t.Fatalf("the th, td rule does not carry %q: %s", expected, rule)
		}
	}
}

// Every value the table shows comes from the API: a user names the mount path,
// and a client supplies the iRODS account, the WebDAV URL and the NFS export.
// The page builds rows with innerHTML, so each of those has to be escaped.
func TestRenderedValuesGoThroughEscaping(t *testing.T) {
	body := serveIndex(t)

	if !strings.Contains(body, "function escapeHtml(") {
		t.Fatalf("management UI has no escapeHtml helper")
	}

	// The helper has to cover the quote characters too, since values are also
	// written into a title attribute.
	for _, expected := range []string{`"&": "&amp;"`, `"<": "&lt;"`, `'"': "&quot;"`, `"'": "&#39;"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("escapeHtml does not map %s", expected)
		}
	}

	for _, expected := range []string{
		"escapeHtml(config.mount_path)",
		"escapeHtml(mount.mount_id)",
		"escapeHtml(config.davfs.url",
		"escapeHtml(record.message)",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("management UI does not escape %s", expected)
		}
	}
}
