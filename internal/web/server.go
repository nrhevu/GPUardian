package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/model"
	"rocguardd/internal/protocol"
)

type Server struct {
	Cfg      config.Config
	Registry *Registry
	Client   NodeClient
}

type addServerRequest struct {
	Name          string `json:"name"`
	Endpoint      string `json:"endpoint"`
	RootKey       string `json:"root_key"`
	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"`
}

type showKeyRequest struct {
	RootKey string `json:"root_key"`
}

type fleetSnapshot struct {
	Servers []serverSnapshot `json:"servers"`
}

type serverSnapshot struct {
	Server   PublicServerRecord  `json:"server"`
	Online   bool                `json:"online"`
	Error    string              `json:"error,omitempty"`
	Snapshot *model.NodeSnapshot `json:"snapshot,omitempty"`
}

func New(cfg config.Config) *Server {
	return &Server{
		Cfg:      cfg,
		Registry: NewRegistry(cfg.WebRegistry),
		Client:   NodeClient{Timeout: 4 * time.Second},
	}
}

func (s *Server) Run(ctx context.Context) error {
	if strings.TrimSpace(s.Cfg.WebPassword) == "" {
		return errors.New("ROCGUARD_WEB_PASSWORD is required")
	}
	httpServer := &http.Server{
		Addr:              s.Cfg.WebAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	err := httpServer.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("/api/servers", s.requireSession(s.handleServers))
	mux.HandleFunc("/api/servers/", s.requireSession(s.handleServerAction))
	mux.HandleFunc("/api/fleet/snapshot", s.requireSession(s.handleFleetSnapshot))
	mux.HandleFunc("/", s.handleStatic)
	return mux
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records, err := s.Registry.PublicList()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, records)
	case http.MethodPost:
		var req addServerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		record := ServerRecord{
			Name:          strings.TrimSpace(req.Name),
			Endpoint:      strings.TrimSpace(req.Endpoint),
			RootKey:       strings.TrimSpace(req.RootKey),
			TLSSkipVerify: req.TLSSkipVerify,
		}
		if err := s.Client.Health(r.Context(), record, record.RootKey); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		stored, err := s.Registry.Upsert(record)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, publicServerRecord(stored))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleServerAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := splitServerAction(r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	record, found, err := s.Registry.Get(id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodDelete:
		if err := s.Registry.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	case action == "reservations" && r.Method == http.MethodPost:
		var args protocol.RegisterArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.Client.CreateReservation(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, result)
	case action == "claim-keys" && r.Method == http.MethodPost:
		var args protocol.RegisterArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
		result, err := s.Client.CreateClaimKey(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, result)
	case action == "show-key" && r.Method == http.MethodPost:
		var req showKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		status, err := s.Client.ShowKeys(r.Context(), record, strings.TrimSpace(req.RootKey))
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, status)
	case action == "revoke" && r.Method == http.MethodPost:
		var args protocol.RevokeArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := s.Client.Revoke(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleFleetSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	records, err := s.Registry.List()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := fleetSnapshot{Servers: make([]serverSnapshot, len(records))}
	var wg sync.WaitGroup
	for i, record := range records {
		wg.Add(1)
		go func(i int, record ServerRecord) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			snapshot, err := s.Client.Snapshot(ctx, record)
			item := serverSnapshot{Server: publicServerRecord(record)}
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Online = true
				item.Snapshot = &snapshot
			}
			out.Servers[i] = item
		}(i, record)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	uiDir := s.Cfg.WebUIDir
	clean := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if clean == "." {
		clean = "index.html"
	}
	if strings.HasPrefix(clean, "..") {
		writeJSONError(w, http.StatusBadRequest, "invalid path")
		return
	}
	candidate := filepath.Join(uiDir, clean)
	if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
		http.ServeFile(w, r, candidate)
		return
	}
	http.ServeFile(w, r, filepath.Join(uiDir, "index.html"))
}

func splitServerAction(rawPath string) (string, string, bool) {
	trimmed := strings.TrimPrefix(rawPath, "/api/servers/")
	if trimmed == rawPath || trimmed == "" {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 1 {
		return parts[0], "", true
	}
	if len(parts) == 2 {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
