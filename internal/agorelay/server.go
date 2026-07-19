package agorelay

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"claudexflow/internal/agobridge"
)

const defaultMaxBodyBytes int64 = 1 << 20

type ServerConfig struct {
	MaxBodyBytes   int64
	TrustedProxies []*net.IPNet
	MaxPollTimeout time.Duration
}

type Server struct {
	store  *Store
	config ServerConfig
}

func NewServer(store *Store, config ServerConfig) *Server {
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxPollTimeout <= 0 {
		config.MaxPollTimeout = 30 * time.Second
	}
	return &Server{store: store, config: config}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bridge/poll", server.poll)
	mux.HandleFunc("POST /v1/relay/requests", server.enqueue)
	mux.HandleFunc("GET /v1/relay/results", server.result)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !server.secureRequest(request) {
			writeRelayError(writer, http.StatusUpgradeRequired, errors.New("HTTPS is required"))
			return
		}
		mux.ServeHTTP(writer, request)
	})
}

func (server *Server) secureRequest(request *http.Request) bool {
	if request.TLS != nil {
		return true
	}
	forwardedProto := request.Header.Values("X-Forwarded-Proto")
	if len(forwardedProto) != 1 || forwardedProto[0] != "https" || len(server.config.TrustedProxies) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	for _, network := range server.config.TrustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (server *Server) poll(writer http.ResponseWriter, request *http.Request) {
	principal, err := server.authenticate(request, RoleDaemon)
	if err != nil {
		writeRelayError(writer, http.StatusUnauthorized, err)
		return
	}
	var poll agobridge.PollEnvelope
	if err := server.decode(writer, request, &poll); err != nil {
		writeRelayError(writer, http.StatusBadRequest, err)
		return
	}
	timeout := parsePollTimeout(request.Header.Get("X-Bridge-Poll-Timeout"), server.config.MaxPollTimeout)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		result, err := server.store.Poll(request.Context(), principal, poll)
		if err != nil {
			writeStoreError(writer, err)
			return
		}
		if len(result.Requests) != 0 || len(poll.Responses) != 0 {
			server.writeJSON(writer, http.StatusOK, result)
			return
		}
		poll.Responses = nil
		select {
		case <-request.Context().Done():
			return
		case <-deadline.C:
			server.writeJSON(writer, http.StatusOK, result)
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (server *Server) enqueue(writer http.ResponseWriter, request *http.Request) {
	principal, err := server.authenticate(request, RoleBrowser)
	if err != nil {
		writeRelayError(writer, http.StatusUnauthorized, err)
		return
	}
	var input EnqueueRequest
	if err := server.decode(writer, request, &input); err != nil {
		writeRelayError(writer, http.StatusBadRequest, err)
		return
	}
	result, err := server.store.Enqueue(request.Context(), principal, input)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	server.writeJSON(writer, http.StatusAccepted, result)
}

func (server *Server) result(writer http.ResponseWriter, request *http.Request) {
	principal, err := server.authenticate(request, RoleBrowser)
	if err != nil {
		writeRelayError(writer, http.StatusUnauthorized, err)
		return
	}
	sequence, err := strconv.ParseUint(request.URL.Query().Get("sequence"), 10, 64)
	if err != nil || sequence == 0 {
		writeRelayError(writer, http.StatusBadRequest, ErrInvalid)
		return
	}
	result, err := server.store.Result(request.Context(), principal, sequence)
	if err != nil {
		writeStoreError(writer, err)
		return
	}
	if result.Pending {
		server.writeJSON(writer, http.StatusAccepted, result)
		return
	}
	server.writeJSON(writer, http.StatusOK, result.Response)
}

func (server *Server) authenticate(request *http.Request, role string) (Principal, error) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) <= len("Bearer ") {
		return Principal{}, ErrUnauthorized
	}
	return server.store.Authenticate(request.Context(), strings.TrimPrefix(values[0], "Bearer "), role)
}

func (server *Server) decode(writer http.ResponseWriter, request *http.Request, output any) error {
	reader := http.MaxBytesReader(writer, request.Body, server.config.MaxBodyBytes)
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func parsePollTimeout(value string, maximum time.Duration) time.Duration {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return maximum
	}
	if duration > maximum {
		return maximum
	}
	return duration
}

func writeStoreError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		writeRelayError(writer, http.StatusForbidden, err)
	case errors.Is(err, ErrConflict):
		writeRelayError(writer, http.StatusConflict, err)
	case errors.Is(err, ErrInvalid):
		writeRelayError(writer, http.StatusBadRequest, err)
	case errors.Is(err, ErrNotFound):
		writeRelayError(writer, http.StatusNotFound, err)
	default:
		writeRelayError(writer, http.StatusInternalServerError, errors.New("internal relay error"))
	}
}

func writeRelayError(writer http.ResponseWriter, status int, err error) {
	writeRelayJSON(writer, status, map[string]string{"error": err.Error()})
}

func writeRelayJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (server *Server) writeJSON(writer http.ResponseWriter, status int, value any) {
	encoded, err := json.Marshal(value)
	if err != nil || int64(len(encoded)+1) > server.config.MaxBodyBytes {
		writeRelayError(writer, http.StatusInternalServerError, errors.New("relay response exceeds configured limit"))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_, _ = writer.Write(append(encoded, '\n'))
}
