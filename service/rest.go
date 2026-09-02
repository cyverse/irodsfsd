package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cyverse/irodsfsd/commons"
	"github.com/cyverse/irodsfsd/service/api"
	"github.com/cyverse/irodsfsd/service/logstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	maxRESTRequestBodySize int64 = 1024 * 1024
	defaultLogTail               = 200
	maxLogLimit                  = 1000
)

var (
	restMarshalOptions     = protojson.MarshalOptions{UseProtoNames: true}
	restUnmarshalOptions   = protojson.UnmarshalOptions{DiscardUnknown: false}
	errRequestBodyTooLarge = errors.New("request body exceeds the maximum allowed size")
)

type RESTHandler struct {
	server *MountServer
	config *commons.Config
}

type restErrorBody struct {
	Error restError `json:"error"`
}

type restError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewRESTHandler(server *MountServer, config *commons.Config) *RESTHandler {
	return &RESTHandler{server: server, config: config}
}

func (handler *RESTHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/mounts", handler.mount)
	mux.HandleFunc("GET /api/v1/mounts", handler.listMounts)
	mux.HandleFunc("GET /api/v1/mounts/{mountID}", handler.getMount)
	mux.HandleFunc("DELETE /api/v1/mounts/{mountID}", handler.unmount)
	mux.HandleFunc("GET /api/v1/mounts/{mountID}/logs", handler.logs)
	mux.HandleFunc("GET /healthz", handler.health)
	mux.HandleFunc("GET /readyz", handler.readiness)
	mux.HandleFunc("GET /api/v1/healthz", handler.health)
	mux.HandleFunc("GET /api/v1/readyz", handler.readiness)
}

func (handler *RESTHandler) mount(response http.ResponseWriter, request *http.Request) {
	mountRequest := &api.MountRequest{}
	if err := decodeProtoJSON(response, request, mountRequest); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeJSON(response, http.StatusRequestEntityTooLarge, restErrorBody{Error: restError{Code: "REQUEST_ENTITY_TOO_LARGE", Message: err.Error()}})
			return
		}
		writeRESTError(response, status.Error(codes.InvalidArgument, err.Error()))
		return
	}
	ctx := withSourceAddress(request.Context(), request.RemoteAddr)
	mounted, err := handler.server.Mount(ctx, mountRequest)
	if err != nil {
		writeRESTError(response, err)
		return
	}
	response.Header().Set("Location", "/api/v1/mounts/"+mounted.GetMount().GetMountId())
	writeProtoJSON(response, http.StatusAccepted, mounted)
}

func (handler *RESTHandler) unmount(response http.ResponseWriter, request *http.Request) {
	ctx := withSourceAddress(request.Context(), request.RemoteAddr)
	unmounted, err := handler.server.Unmount(ctx, &api.UnmountRequest{MountId: request.PathValue("mountID")})
	if err != nil {
		writeRESTError(response, err)
		return
	}
	writeProtoJSON(response, http.StatusAccepted, unmounted)
}

// logs returns recent lines from one mount's child stdout/stderr, bounded
// by tail/since/limit so a query can never return an unbounded response.
func (handler *RESTHandler) logs(response http.ResponseWriter, request *http.Request) {
	mountID := request.PathValue("mountID")
	// GetMount both confirms the mount ID exists (so a typo reports 404,
	// not an empty log) and reuses the existing not-found error mapping.
	if _, err := handler.server.GetMount(request.Context(), &api.GetMountRequest{MountId: mountID}); err != nil {
		writeRESTError(response, err)
		return
	}

	options, err := logQueryOptionsFromRequest(request)
	if err != nil {
		writeRESTError(response, status.Error(codes.InvalidArgument, err.Error()))
		return
	}
	records, err := logstore.Query(handler.config.GetMountLogPath(mountID), options)
	if err != nil {
		writeRESTError(response, status.Error(codes.Internal, err.Error()))
		return
	}
	writeJSON(response, http.StatusOK, map[string]any{"logs": records})
}

func logQueryOptionsFromRequest(request *http.Request) (logstore.QueryOptions, error) {
	query := request.URL.Query()
	options := logstore.QueryOptions{Limit: maxLogLimit}

	if value := query.Get("since"); value != "" {
		since, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return options, fmt.Errorf("invalid since %q: must be RFC3339", value)
		}
		options.Since = since
	} else {
		// tail defaults to the most recent lines only when the caller is
		// not already bounding the query by time.
		options.Tail = defaultLogTail
	}
	if value := query.Get("tail"); value != "" {
		tail, err := strconv.Atoi(value)
		if err != nil || tail < 0 {
			return options, fmt.Errorf("invalid tail %q: must be a non-negative integer", value)
		}
		options.Tail = tail
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 0 {
			return options, fmt.Errorf("invalid limit %q: must be a non-negative integer", value)
		}
		if limit > 0 && limit < maxLogLimit {
			options.Limit = limit
		}
	}
	return options, nil
}

func (handler *RESTHandler) getMount(response http.ResponseWriter, request *http.Request) {
	mount, err := handler.server.GetMount(request.Context(), &api.GetMountRequest{MountId: request.PathValue("mountID")})
	if err != nil {
		writeRESTError(response, err)
		return
	}
	writeProtoJSON(response, http.StatusOK, mount)
}

func (handler *RESTHandler) listMounts(response http.ResponseWriter, request *http.Request) {
	listRequest, err := listMountsRequestFromQuery(request)
	if err != nil {
		writeRESTError(response, status.Error(codes.InvalidArgument, err.Error()))
		return
	}
	mounts, err := handler.server.ListMounts(request.Context(), listRequest)
	if err != nil {
		writeRESTError(response, err)
		return
	}
	writeProtoJSON(response, http.StatusOK, mounts)
}

func (handler *RESTHandler) health(response http.ResponseWriter, _ *http.Request) {
	writeJSON(response, http.StatusOK, map[string]string{"status": "ok"})
}

func (handler *RESTHandler) readiness(response http.ResponseWriter, request *http.Request) {
	if handler.server == nil || handler.server.manager == nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
		return
	}
	if err := handler.server.Ready(request.Context()); err != nil {
		writeJSON(response, http.StatusServiceUnavailable, map[string]string{"status": "not_ready", "reason": err.Error()})
		return
	}
	writeJSON(response, http.StatusOK, map[string]string{"status": "ready"})
}

func listMountsRequestFromQuery(request *http.Request) (*api.ListMountsRequest, error) {
	query := request.URL.Query()
	listRequest := &api.ListMountsRequest{}
	stateValues := append(query["state"], query["states"]...)
	for _, values := range stateValues {
		for _, value := range strings.Split(values, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			state, err := parseMountState(value)
			if err != nil {
				return nil, err
			}
			listRequest.States = append(listRequest.States, state)
		}
	}
	if value, exists := query["mount_path_prefix"]; exists {
		prefix := ""
		if len(value) > 0 {
			prefix = value[len(value)-1]
		}
		listRequest.MountPathPrefix = &prefix
	}
	if value, exists := query["client_user"]; exists {
		clientUser := ""
		if len(value) > 0 {
			clientUser = value[len(value)-1]
		}
		listRequest.ClientUser = &clientUser
	}
	return listRequest, nil
}

func parseMountState(value string) (api.MountState, error) {
	upperValue := strings.ToUpper(value)
	if number, err := strconv.ParseInt(upperValue, 10, 32); err == nil {
		state := api.MountState(number)
		if _, exists := api.MountState_name[int32(state)]; exists {
			return state, nil
		}
	}
	if !strings.HasPrefix(upperValue, "MOUNT_STATE_") {
		upperValue = "MOUNT_STATE_" + upperValue
	}
	if number, exists := api.MountState_value[upperValue]; exists {
		return api.MountState(number), nil
	}
	return api.MountState_MOUNT_STATE_UNSPECIFIED, fmt.Errorf("unknown mount state %q", value)
}

func decodeProtoJSON(response http.ResponseWriter, request *http.Request, message proto.Message) error {
	request.Body = http.MaxBytesReader(response, request.Body, maxRESTRequestBodySize)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errRequestBodyTooLarge
		}
		return fmt.Errorf("failed to read request body: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("request body is required")
	}
	if err := restUnmarshalOptions.Unmarshal(data, message); err != nil {
		return fmt.Errorf("invalid JSON request: %w", err)
	}
	return nil
}

func writeProtoJSON(response http.ResponseWriter, statusCode int, message proto.Message) {
	data, err := restMarshalOptions.Marshal(message)
	if err != nil {
		writeRESTError(response, status.Error(codes.Internal, "failed to encode response"))
		return
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(statusCode)
	_, _ = response.Write(append(data, '\n'))
}

func writeRESTError(response http.ResponseWriter, err error) {
	code := status.Code(err)
	statusCode := http.StatusInternalServerError
	switch code {
	case codes.InvalidArgument:
		statusCode = http.StatusBadRequest
	case codes.NotFound:
		statusCode = http.StatusNotFound
	case codes.AlreadyExists, codes.FailedPrecondition, codes.Aborted:
		statusCode = http.StatusConflict
	case codes.ResourceExhausted:
		statusCode = http.StatusTooManyRequests
	case codes.Canceled:
		statusCode = http.StatusRequestTimeout
	case codes.DeadlineExceeded:
		statusCode = http.StatusGatewayTimeout
	case codes.Unavailable:
		statusCode = http.StatusServiceUnavailable
	}
	writeJSON(response, statusCode, restErrorBody{Error: restError{Code: restCodeName(code), Message: status.Convert(err).Message()}})
}

func restCodeName(code codes.Code) string {
	switch code {
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.Aborted:
		return "ABORTED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.Canceled:
		return "CANCELED"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.Unavailable:
		return "UNAVAILABLE"
	case codes.Internal:
		return "INTERNAL"
	default:
		return strings.ToUpper(code.String())
	}
}

func writeJSON(response http.ResponseWriter, statusCode int, value any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(statusCode)
	_ = json.NewEncoder(response).Encode(value)
}
