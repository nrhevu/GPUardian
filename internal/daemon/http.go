package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"rocguardd/internal/model"
	"rocguardd/internal/protocol"
	"rocguardd/internal/store"
)

func (s *Server) startNodeHTTP(ctx context.Context) (func(), error) {
	if (s.Cfg.NodeTLSCert == "") != (s.Cfg.NodeTLSKey == "") {
		return nil, errors.New("both ROCGUARD_NODE_TLS_CERT and ROCGUARD_NODE_TLS_KEY are required for TLS")
	}
	listener, err := net.Listen("tcp", s.Cfg.NodeAddr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           s.nodeHTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if s.Cfg.NodeTLSCert != "" || s.Cfg.NodeTLSKey != "" {
			_ = server.ServeTLS(listener, s.Cfg.NodeTLSCert, s.Cfg.NodeTLSKey)
			return
		}
		_ = server.Serve(listener)
	}()
	return func() {
		_ = server.Close()
		_ = listener.Close()
	}, nil
}

func (s *Server) nodeHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.nodeAuth(s.handleNodeHealth))
	mux.HandleFunc("/api/v1/snapshot", s.nodeAuth(s.handleNodeSnapshot))
	mux.HandleFunc("/api/v1/reservations", s.nodeAuth(s.handleNodeReservations))
	mux.HandleFunc("/api/v1/claim-keys", s.nodeAuth(s.handleNodeClaimKeys))
	mux.HandleFunc("/api/v1/show-keys", s.nodeAuth(s.handleNodeShowKeys))
	mux.HandleFunc("/api/v1/revoke", s.nodeAuth(s.handleNodeRevoke))
	return mux
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

func (s *Server) handleNodeReservations(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.RegisterArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
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
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil && !errors.Is(err, io.EOF) {
		writeHTTPError(w, http.StatusBadRequest, err.Error())
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

func (s *Server) handleNodeRevoke(w http.ResponseWriter, r *http.Request, rootKey string) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var args protocol.RevokeArgs
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
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

func protocolRequest(method string, args any) protocol.Request {
	raw, _ := json.Marshal(args)
	return protocol.Request{ID: "http", Method: method, Args: raw}
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
