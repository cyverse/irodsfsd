package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/logstore"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestRESTMountService(t *testing.T) {
	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRESTHandler(server, commons.NewDefaultConfig()).RegisterRoutes(mux)

	mountResponse := performRESTRequest(t, mux, http.MethodPost, "/api/v1/mounts", `{
		"mount_id":"rest-mount",
		"config":{"mount_path":"/mnt/rest"}
	}`)
	if mountResponse.Code != http.StatusAccepted {
		t.Fatalf("Mount status = %d, body = %s", mountResponse.Code, mountResponse.Body.String())
	}
	if location := mountResponse.Header().Get("Location"); location != "/api/v1/mounts/rest-mount" {
		t.Fatalf("Location = %q", location)
	}
	mounted := &api.MountResponse{}
	if err := protojson.Unmarshal(mountResponse.Body.Bytes(), mounted); err != nil {
		t.Fatal(err)
	}
	if mounted.GetMount().GetMountId() != "rest-mount" {
		t.Fatalf("Mount ID = %q", mounted.GetMount().GetMountId())
	}

	getResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/rest-mount", "")
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GetMount status = %d, body = %s", getResponse.Code, getResponse.Body.String())
	}

	listResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts?states=MOUNTED&mount_path_prefix=/mnt&client_user=alice", "")
	if listResponse.Code != http.StatusOK {
		t.Fatalf("ListMounts status = %d, body = %s", listResponse.Code, listResponse.Body.String())
	}
	listed := &api.ListMountsResponse{}
	if err := protojson.Unmarshal(listResponse.Body.Bytes(), listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Mounts) != 1 {
		t.Fatalf("listed mounts = %d", len(listed.Mounts))
	}

	unmountResponse := performRESTRequest(t, mux, http.MethodDelete, "/api/v1/mounts/rest-mount", "")
	if unmountResponse.Code != http.StatusAccepted {
		t.Fatalf("Unmount status = %d, body = %s", unmountResponse.Code, unmountResponse.Body.String())
	}

	missingResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/rest-mount", "")
	if missingResponse.Code != http.StatusNotFound || !strings.Contains(missingResponse.Body.String(), `"code":"NOT_FOUND"`) {
		t.Fatalf("missing response = %d, body = %s", missingResponse.Code, missingResponse.Body.String())
	}
}

func TestRESTRejectsInvalidRequests(t *testing.T) {
	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRESTHandler(server, commons.NewDefaultConfig()).RegisterRoutes(mux)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "empty body", method: http.MethodPost, path: "/api/v1/mounts"},
		{name: "unknown field", method: http.MethodPost, path: "/api/v1/mounts", body: `{"unexpected":true}`},
		{name: "unknown state", method: http.MethodGet, path: "/api/v1/mounts?state=broken"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRESTRequest(t, mux, test.method, test.path, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRESTHealthEndpoints(t *testing.T) {
	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRESTHandler(server, commons.NewDefaultConfig()).RegisterRoutes(mux)
	for _, path := range []string{"/healthz", "/readyz", "/api/v1/healthz", "/api/v1/readyz"} {
		response := performRESTRequest(t, mux, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, response.Code)
		}
	}
}

func TestServiceStartsRESTAndGRPCListeners(t *testing.T) {
	config := commons.NewDefaultConfig()
	config.ServiceEndpoint = "tcp://127.0.0.1:13020"
	config.ServicePort = 13021
	svc, err := newService(config, newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	grpcListener := bufconn.Listen(1024 * 1024)
	restListener := bufconn.Listen(1024 * 1024)
	svc.listen = func(string, string) (net.Listener, error) { return grpcListener, nil }
	svc.restListen = func(network string, address string) (net.Listener, error) {
		if network != "tcp" || address != ":13021" {
			t.Fatalf("REST listen = %s %s", network, address)
		}
		return restListener, nil
	}
	if err := svc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Release)

	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return restListener.Dial()
		},
	}
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.CloseIdleConnections)
	response, err := client.Get("http://bufnet/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}

	svc.Stop()
}

func TestRESTMountLogs(t *testing.T) {
	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	config := commons.NewDefaultConfig()
	config.LogRootPath = t.TempDir()
	mux := http.NewServeMux()
	NewRESTHandler(server, config).RegisterRoutes(mux)

	mountResponse := performRESTRequest(t, mux, http.MethodPost, "/api/v1/mounts", `{
		"mount_id":"log-mount",
		"config":{"mount_path":"/mnt/rest"}
	}`)
	if mountResponse.Code != http.StatusAccepted {
		t.Fatalf("Mount status = %d, body = %s", mountResponse.Code, mountResponse.Body.String())
	}

	mountLog, err := logstore.Open(config.GetMountLogPath("log-mount"), []string{"top-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mountLog.Stdout().Write([]byte("connecting with password top-secret\nready\n")); err != nil {
		t.Fatal(err)
	}
	if err := mountLog.Close(); err != nil {
		t.Fatal(err)
	}

	response := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/log-mount/logs", "")
	if response.Code != http.StatusOK {
		t.Fatalf("logs status = %d, body = %s", response.Code, response.Body.String())
	}
	var decoded struct {
		Logs []logstore.Record `json:"logs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Logs) != 2 {
		t.Fatalf("logs = %+v, want 2 records", decoded.Logs)
	}
	if decoded.Logs[0].Message != "connecting with password [REDACTED]" {
		t.Errorf("logs[0].Message = %q, want the secret redacted", decoded.Logs[0].Message)
	}

	tailResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/log-mount/logs?tail=1", "")
	var tailDecoded struct {
		Logs []logstore.Record `json:"logs"`
	}
	if err := json.Unmarshal(tailResponse.Body.Bytes(), &tailDecoded); err != nil {
		t.Fatal(err)
	}
	if len(tailDecoded.Logs) != 1 || tailDecoded.Logs[0].Message != "ready" {
		t.Errorf("tail=1 logs = %+v, want just the last line", tailDecoded.Logs)
	}

	missingResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/missing/logs", "")
	if missingResponse.Code != http.StatusNotFound {
		t.Errorf("logs for a missing mount status = %d, want 404", missingResponse.Code)
	}

	badQueryResponse := performRESTRequest(t, mux, http.MethodGet, "/api/v1/mounts/log-mount/logs?tail=not-a-number", "")
	if badQueryResponse.Code != http.StatusBadRequest {
		t.Errorf("logs with an invalid tail status = %d, want 400", badQueryResponse.Code)
	}
}

// TestServiceMetricsAndWebRoutesCoexist exercises the exact route
// registration NewService performs, without needing NewService's own
// FUSE/BadgerDB dependencies (unavailable in some sandboxes). It catches a
// real bug found by manually running the daemon: registering a bare
// "/metrics" pattern alongside the web UI's "GET /" catch-all makes
// net/http.ServeMux panic at registration time, since it cannot resolve
// which pattern is "more specific" when one restricts by method and the
// other does not.
func TestServiceMetricsAndWebRoutesCoexist(t *testing.T) {
	svc, err := newService(commons.NewDefaultConfig(), newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	svc.registerMetricsRoute(NewMetrics(&MountManager{mounts: map[string]*managedMount{}}))

	metricsResponse := performRESTRequest(t, svc.mux, http.MethodGet, "/metrics", "")
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, body = %s", metricsResponse.Code, metricsResponse.Body.String())
	}
	rootResponse := performRESTRequest(t, svc.mux, http.MethodGet, "/", "")
	if rootResponse.Code != http.StatusOK || !strings.Contains(rootResponse.Body.String(), "<html") {
		t.Fatalf("GET / status = %d, body = %s", rootResponse.Code, rootResponse.Body.String())
	}
}

func TestRESTMountRequestBodyTooLarge(t *testing.T) {
	server, err := newMountServer(newFakeMountOperations())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewRESTHandler(server, commons.NewDefaultConfig()).RegisterRoutes(mux)

	oversizedBody := `{"mount_id":"` + strings.Repeat("a", int(maxRESTRequestBodySize)) + `"}`
	response := performRESTRequest(t, mux, http.MethodPost, "/api/v1/mounts", oversizedBody)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s, want 413", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"REQUEST_ENTITY_TOO_LARGE"`) {
		t.Errorf("body = %s, want a REQUEST_ENTITY_TOO_LARGE code", response.Body.String())
	}
}

func performRESTRequest(t *testing.T, handler http.Handler, method string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
