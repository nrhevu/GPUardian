package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"rocguard/internal/model"
	"rocguard/internal/netlimit"
	"rocguard/internal/protocol"
	"rocguard/internal/store"
	"rocguard/internal/telemetry"
)

const maxNodeHTTPConnections = 128

func (s *Server) startNodeHTTP(ctx context.Context) (func(), error) {
	if (s.Cfg.NodeTLSCert == "") != (s.Cfg.NodeTLSKey == "") {
		return nil, errors.New("both ROCGUARD_NODE_TLS_CERT and ROCGUARD_NODE_TLS_KEY are required for TLS")
	}
	if s.Cfg.NodeTLSCert == "" && !s.Cfg.NodeAllowInsecure {
		return nil, errors.New("refusing node HTTP listener without TLS; configure TLS or explicitly set ROCGUARD_NODE_ALLOW_INSECURE=1")
	}
	var tlsConfig *tls.Config
	if s.Cfg.NodeTLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(s.Cfg.NodeTLSCert, s.Cfg.NodeTLSKey)
		if err != nil {
			return nil, err
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	}
	listener, err := net.Listen("tcp", s.Cfg.NodeAddr)
	if err != nil {
		return nil, err
	}
	listener = netlimit.NewListener(listener, maxNodeHTTPConnections)
	server := &http.Server{
		Handler:           s.nodeHTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         tlsConfig,
	}
	closed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-closed:
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if tlsConfig != nil {
			_ = server.ServeTLS(listener, "", "")
			return
		}
		_ = server.Serve(listener)
	}()
	var closeOnce sync.Once
	return func() {
		closeOnce.Do(func() {
			close(closed)
			_ = server.Close()
			_ = listener.Close()
		})
	}, nil
}

func (s *Server) nodeHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.nodeAuth(s.handleNodeHealth))
	mux.HandleFunc("/api/v1/snapshot", s.nodeAuth(s.handleNodeSnapshot))
	mux.HandleFunc("/api/v1/info", s.nodeAuth(s.handleNodeInfo))
	mux.HandleFunc("/api/v1/telemetry", s.nodeAuth(s.handleNodeTelemetry))
	mux.HandleFunc("/api/v1/reservations", s.nodeAuth(s.handleNodeReservations))
	mux.HandleFunc("/api/v1/claim-keys", s.nodeAuth(s.handleNodeClaimKeys))
	mux.HandleFunc("/api/v1/show-keys", s.nodeAuth(s.handleNodeShowKeys))
	mux.HandleFunc("/api/v1/allow", s.nodeAuth(s.handleNodeAllow))
	mux.HandleFunc("/api/v1/revoke", s.nodeAuth(s.handleNodeRevoke))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		release, ok := s.acquireNodeHTTPRequest()
		if !ok {
			w.Header().Set("Retry-After", "1")
			writeHTTPError(w, http.StatusServiceUnavailable, "too many concurrent node requests; retry shortly")
			return
		}
		defer release()
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		}
		if nodeRequestHasBody(r) && !nodeRequestHasJSONContentType(r) {
			writeHTTPError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) acquireNodeHTTPRequest() (func(), bool) {
	s.nodeHTTPOnce.Do(func() {
		s.nodeHTTPSlots = make(chan struct{}, maxConcurrentNodeHTTP)
	})
	select {
	case s.nodeHTTPSlots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.nodeHTTPSlots }) }, true
	default:
		return nil, false
	}
}

func nodeRequestHasBody(r *http.Request) bool {
	return r.Body != nil && (r.ContentLength != 0 || len(r.TransferEncoding) > 0)
}

func nodeRequestHasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

type nodeHTTPFunc func(http.ResponseWriter, *http.Request, string)

func (s *Server) nodeAuth(next nodeHTTPFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rootKey, ok := bearerToken(r)
		if !ok {
			writeHTTPError(w, http.StatusUnauthorized, "bearer root key is required")
			return
		}
		valid, err := s.Store.ValidateRootKey(rootKey)
		if err != nil {
			writeHTTPError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !valid {
			writeHTTPError(w, http.StatusUnauthorized, store.ErrInvalidRootKey.Error())
			return
		}
		next(w, r, rootKey)
	}
}

func (s *Server) handleNodeHealth(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeHTTPJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleNodeSnapshot(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	snapshot, err := s.Snapshot(r.Context(), time.Now())
	if err != nil {
		writeHTTPError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeHTTPJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleNodeInfo(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Telemetry == nil {
		writeHTTPError(w, http.StatusServiceUnavailable, "telemetry is unavailable")
		return
	}
	writeHTTPJSON(w, http.StatusOK, s.Telemetry.Info())
}

func (s *Server) handleNodeTelemetry(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method != http.MethodGet {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.Telemetry == nil {
		writeHTTPError(w, http.StatusServiceUnavailable, "telemetry is unavailable")
		return
	}
	limit := telemetry.DefaultPageLimit
	if text := strings.TrimSpace(r.URL.Query().Get("limit")); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > telemetry.MaxPageLimit {
			writeHTTPError(w, http.StatusBadRequest, "limit must be between 1 and 256")
			return
		}
		limit = parsed
	}
	page, err := s.Telemetry.Page(strings.TrimSpace(r.URL.Query().Get("cursor")), limit)
	if err != nil {
		var gap *telemetry.CursorGap
		if errors.As(err, &gap) {
			writeHTTPJSON(w, http.StatusGone, gap)
			return
		}
		writeHTTPError(w, http.StatusBadRequest, "invalid telemetry cursor")
		return
	}
	writeHTTPJSON(w, http.StatusOK, page)
}

func (s *Server) handleNodeReservations(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.RegisterArgs
	if err := decodeNodeJSON(r, &args); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	args.Name = strings.TrimSpace(args.Name)
	if args.Name == "" {
		writeHTTPError(w, http.StatusBadRequest, "name is required")
		return
	}
	args.RootKey = rootKey
	args.Mode = model.TokenModeReserved
	result, err := s.dispatch(r.Context(), peer{}, protocolRequest("register", args))
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	writeHTTPJSON(w, http.StatusCreated, result)
}

func (s *Server) handleNodeClaimKeys(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.RegisterArgs
	if err := decodeNodeJSON(r, &args); err != nil && !errors.Is(err, io.EOF) {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	args.Name = strings.TrimSpace(args.Name)
	if args.Name == "" {
		writeHTTPError(w, http.StatusBadRequest, "name is required")
		return
	}
	args.RootKey = rootKey
	args.Mode = model.TokenModeClaimed
	result, err := s.dispatch(r.Context(), peer{}, protocolRequest("register", args))
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	writeHTTPJSON(w, http.StatusCreated, result)
}

func (s *Server) handleNodeShowKeys(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := s.Store.KeyStatus(rootKey, time.Now())
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	writeHTTPJSON(w, http.StatusOK, result)
}

func (s *Server) handleNodeAllow(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.AllowArgs
	if err := decodeNodeJSON(r, &args); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	tokenID := strings.TrimSpace(args.ID)
	if tokenID == "" {
		writeHTTPError(w, http.StatusBadRequest, "id is required")
		return
	}
	status, err := s.Store.KeyStatus(rootKey, time.Now())
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	var secret string
	for _, token := range status.Tokens {
		if token.ID == tokenID {
			secret = token.Key
			break
		}
	}
	if secret == "" {
		writeHTTPError(w, http.StatusNotFound, "key not found or secret is not stored")
		return
	}
	var result any
	switch strings.TrimSpace(args.Mode) {
	case model.ModeDocker:
		result, err = s.dispatch(r.Context(), peer{}, protocolRequest("allow_docker", protocol.DockerAllowArgs{Container: args.Container}, secret))
	case model.ModeK8s:
		result, err = s.dispatch(r.Context(), peer{}, protocolRequest("allow_k8s", protocol.K8sAllowArgs{Namespace: args.Namespace}, secret))
	case model.ModeUser:
		result, err = s.dispatch(r.Context(), peer{}, protocolRequest("allow_user", protocol.UserAllowArgs{User: args.User}, secret))
	default:
		writeHTTPError(w, http.StatusBadRequest, "mode must be docker, k8s, or user")
		return
	}
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	writeHTTPJSON(w, http.StatusCreated, result)
}

func (s *Server) handleNodeRevoke(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.RevokeArgs
	if err := decodeNodeJSON(r, &args); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
		return
	}
	args.RootKey = rootKey
	result, err := s.dispatch(r.Context(), peer{}, protocolRequest("revoke", args))
	if err != nil {
		writeDispatchError(w, err)
		return
	}
	writeHTTPJSON(w, http.StatusOK, result)
}

func protocolRequest(method string, args any, token ...string) protocol.Request {
	raw, _ := json.Marshal(args)
	req := protocol.Request{ID: "http", Method: method, Args: raw}
	if len(token) > 0 {
		req.Token = token[0]
	}
	return req
}

func bearerToken(r *http.Request) (string, bool) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	prefix := "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}

func decodeNodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func writeDispatchError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrInvalidRootKey) {
		status = http.StatusUnauthorized
	}
	writeHTTPError(w, status, err.Error())
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	writeHTTPJSON(w, status, map[string]string{"error": message})
}
