package web

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rocguard/internal/config"
	"rocguard/internal/history"
	"rocguard/internal/model"
	"rocguard/internal/netlimit"
	"rocguard/internal/protocol"
)

type Server struct {
	Cfg      config.Config
	Registry *Registry
	Users    *UserStore
	Client   NodeAPI
	History  *history.Store

	fleetCacheMu   sync.Mutex
	fleetCache     fleetSnapshot
	fleetCacheAt   time.Time
	fleetCacheOK   bool
	fleetCacheTTL  time.Duration
	fleetRefresh   *fleetRefreshCall
	now            func() time.Time
	sessionKey     []byte
	sessionKeyErr  error
	loginMu        sync.Mutex
	loginAttempts  map[string]loginAttempt
	requestMu      sync.Mutex
	activeUsers    map[string]int
	activeTotal    int
	activeNonAdmin int
	historySyncMu  sync.Mutex
	historySync    map[string]historySyncState
}

type addServerRequest struct {
	Name          string `json:"name"`
	Endpoint      string `json:"endpoint"`
	RootKey       string `json:"root_key"`
	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"`
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role,omitempty"`
}

type deleteUserRequest struct {
	Username string `json:"username"`
}

type showKeyRequest struct {
	ID      string `json:"id"`
	RootKey string `json:"root_key,omitempty"`
}

type fleetSnapshot struct {
	Servers []serverSnapshot `json:"servers"`
}

type fleetRefreshCall struct {
	done   chan struct{}
	result fleetSnapshot
	err    error
}

const (
	fleetRefreshTimeout          = 20 * time.Second
	fleetNodeTimeout             = 4 * time.Second
	maxFleetWorkers              = 16
	maxNodeSnapshotRecords       = 4096
	maxNodeSnapshotProcesses     = 2048
	maxFleetSnapshotRecords      = 32768
	maxFleetSnapshotProcesses    = 8192
	maxFleetSnapshotRetainedSize = 32 << 20
	maxFleetErrorBytes           = 1024
	maxWebHTTPConnections        = 256
	maxAuthenticatedRequests     = 64
	maxAuthenticatedRequestsUser = 8
	reservedAdminRequests        = 8
)

type fleetSnapshotBudget struct {
	mu        sync.Mutex
	records   int
	processes int
	bytes     int64
}

type serverSnapshot struct {
	Server   PublicServerRecord  `json:"server"`
	Online   bool                `json:"online"`
	Error    string              `json:"error,omitempty"`
	Snapshot *model.NodeSnapshot `json:"snapshot,omitempty"`
}

func New(cfg config.Config) *Server {
	sessionKey, sessionKeyErr := loadOrCreateSessionKey(cfg.WebSessionKey)
	return &Server{
		Cfg:           cfg,
		Registry:      NewRegistry(cfg.WebRegistry, cfg.WebAllowInsecureNodes),
		Users:         NewUserStore(cfg.WebUsers),
		Client:        NodeClient{Timeout: 4 * time.Second, AllowInsecureNodes: cfg.WebAllowInsecureNodes},
		fleetCacheTTL: time.Second,
		now:           time.Now,
		sessionKey:    sessionKey,
		sessionKeyErr: sessionKeyErr,
		loginAttempts: make(map[string]loginAttempt),
		activeUsers:   make(map[string]int),
		historySync:   make(map[string]historySyncState),
	}
}

func (s *Server) Run(ctx context.Context) error {
	if s.sessionKeyErr != nil {
		return fmt.Errorf("initialize web session key: %w", s.sessionKeyErr)
	}
	if len(s.sessionKey) != sessionKeyBytes {
		return errors.New("web session key is unavailable")
	}
	if (s.Cfg.WebTLSCert == "") != (s.Cfg.WebTLSKey == "") {
		return errors.New("both ROCGUARD_WEB_TLS_CERT and ROCGUARD_WEB_TLS_KEY are required for TLS")
	}
	if s.Cfg.WebTLSCert == "" && !s.Cfg.WebAllowInsecure {
		return errors.New("refusing web HTTP listener without TLS; configure TLS or explicitly set ROCGUARD_WEB_ALLOW_INSECURE=1")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.Cfg.WebTLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(s.Cfg.WebTLSCert, s.Cfg.WebTLSKey)
		if err != nil {
			return fmt.Errorf("load web TLS certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	if err := s.Users.BootstrapAdmin(s.Cfg.WebUser, s.Cfg.WebPassword); err != nil {
		return err
	}
	historyPath := strings.TrimSpace(s.Cfg.WebDB)
	if historyPath == "" {
		historyPath = filepath.Join(filepath.Dir(s.Cfg.WebRegistry), "history.db")
	}
	historyStore, err := history.Open(historyPath)
	if err != nil {
		return fmt.Errorf("initialize history database: %w", err)
	}
	s.History = historyStore
	defer historyStore.Close()
	go s.runHistoryCollector(ctx)
	httpServer := &http.Server{
		Addr:              s.Cfg.WebAddr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         tlsConfig,
	}
	listener, err := net.Listen("tcp", s.Cfg.WebAddr)
	if err != nil {
		return err
	}
	listener = netlimit.NewListener(listener, maxWebHTTPConnections)
	defer listener.Close()
	serveDone := make(chan struct{})
	defer close(serveDone)
	go func() {
		select {
		case <-ctx.Done():
		case <-serveDone:
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	var serveErr error
	if s.Cfg.WebTLSCert != "" {
		serveErr = httpServer.ServeTLS(listener, "", "")
	} else {
		serveErr = httpServer.Serve(listener)
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		return nil
	}
	return serveErr
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", s.handleLogin)
	if s.Cfg.WebAllowRegistration {
		mux.HandleFunc("/api/register", s.handleRegister)
	}
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("/api/password", s.requireSession(s.handleChangePassword))
	mux.HandleFunc("/api/users", s.requireAdmin(s.handleUsers))
	mux.HandleFunc("/api/servers", s.requireSession(s.handleServers))
	mux.HandleFunc("/api/servers/", s.requireSession(s.handleServerAction))
	mux.HandleFunc("/api/fleet/snapshot", s.requireSession(s.handleFleetSnapshot))
	mux.HandleFunc("/api/history/summary", s.requireSession(s.handleHistorySummary))
	mux.HandleFunc("/api/history/search", s.requireSession(s.handleHistorySearch))
	mux.HandleFunc("/api/history/sessions", s.requireSession(s.handleHistorySessions))
	mux.HandleFunc("/api/history/sessions/", s.requireSession(s.handleHistorySessionAction))
	mux.HandleFunc("/", s.handleStatic)
	return s.securityMiddleware(mux)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.Users.List()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, users)
	case http.MethodPost:
		var req createUserRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		user, err := s.Users.Create(req.Username, req.Password, req.Role)
		if err != nil {
			if errors.Is(err, errPasswordWorkBusy) {
				w.Header().Set("Retry-After", "1")
				writeJSONError(w, http.StatusTooManyRequests, "password service is temporarily busy; try again")
				return
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, user)
	case http.MethodDelete:
		session, _ := currentSession(r)
		var req deleteUserRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if sameOwner(req.Username, session.User) {
			writeJSONError(w, http.StatusBadRequest, "cannot delete the current user")
			return
		}
		if err := s.Users.Delete(req.Username); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
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
		session, _ := currentSession(r)
		if session.Role != RoleAdmin {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		var req addServerRequest
		if err := decodeJSONBody(r, &req); err != nil {
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
		s.clearFleetCache()
		writeJSON(w, http.StatusCreated, publicServerRecord(stored))
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleServerAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
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
		if session.Role != RoleAdmin {
			writeJSONError(w, http.StatusForbidden, "admin access required")
			return
		}
		if err := s.Registry.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		s.clearFleetCache()
		writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
	case action == "reservations" && r.Method == http.MethodPost:
		var args protocol.RegisterArgs
		if err := decodeJSONBody(r, &args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		args.Name = session.User
		preparedSessionID := ""
		if s.History != nil && args.StartsAt != nil && args.ExpiresAt != nil {
			if client, ok := s.Client.(TelemetryNodeAPI); ok {
				if info, infoErr := client.Info(r.Context(), record); infoErr == nil && info.NodeID != "" && telemetryCapability(info, "reservation_external_session_id") {
					preparedSessionID = "sess_" + randomHex(12)
					args.ExternalSessionID = preparedSessionID
					if prepareErr := s.History.PrepareSession(r.Context(), preparedSessionID, info.NodeID, record.ID, record.Name, session.User,
						args.Purpose, args.StartsAt.UTC(), args.ExpiresAt.UTC(), args.GPUs); prepareErr != nil {
						writeJSONError(w, http.StatusInternalServerError, prepareErr.Error())
						return
					}
				}
			}
		}
		result, err := s.Client.CreateReservation(r.Context(), record, args)
		if err != nil && preparedSessionID != "" && unsupportedExternalSessionError(err) {
			_ = s.History.DropProvisioningSession(r.Context(), preparedSessionID)
			preparedSessionID = ""
			args.ExternalSessionID = ""
			result, err = s.Client.CreateReservation(r.Context(), record, args)
		}
		if err != nil {
			if preparedSessionID != "" {
				_ = s.History.DropProvisioningSession(r.Context(), preparedSessionID)
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if result.TokenID == "" {
			if status, statusErr := s.Client.ShowKeys(r.Context(), record, record.RootKey); statusErr == nil {
				result.TokenID = reservationTokenID(result, status)
			}
		}
		if preparedSessionID != "" && result.TokenID != "" {
			_ = s.History.ConfirmSession(r.Context(), preparedSessionID, result.TokenID, result.ReservationIDs, result.GPUs)
		}
		s.clearFleetCache()
		result.Token = ""
		writeJSON(w, http.StatusCreated, result)
	case action == "claim-keys" && r.Method == http.MethodPost:
		if session.Role != RoleAdmin {
			writeJSONError(w, http.StatusForbidden, "admin access required for claim keys")
			return
		}
		var args protocol.RegisterArgs
		if err := decodeJSONBody(r, &args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		args.Name = session.User
		result, err := s.Client.CreateClaimKey(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.clearFleetCache()
		writeJSON(w, http.StatusCreated, result)
	case action == "show-key" && r.Method == http.MethodPost:
		var req showKeyRequest
		if err := decodeJSONBody(r, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		tokenID := strings.TrimSpace(req.ID)
		if tokenID == "" {
			writeJSONError(w, http.StatusBadRequest, "id is required")
			return
		}
		status, err := s.Client.ShowKeys(r.Context(), record, record.RootKey)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, err.Error())
			return
		}
		token, ok := findToken(status.Tokens, tokenID)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "key not found")
			return
		}
		if session.Role != RoleAdmin && !sameOwner(token.Name, session.User) {
			writeJSONError(w, http.StatusForbidden, "key access denied")
			return
		}
		writeJSON(w, http.StatusOK, model.KeyStatus{
			Now:    status.Now,
			Tokens: []model.TokenView{token},
		})
	case action == "allow" && r.Method == http.MethodPost:
		var args protocol.AllowArgs
		if err := decodeJSONBody(r, &args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		args.ID = strings.TrimSpace(args.ID)
		args.Mode = strings.TrimSpace(args.Mode)
		if args.ID == "" {
			writeJSONError(w, http.StatusBadRequest, "id is required")
			return
		}
		if session.Role != RoleAdmin && allowArgsHaveWildcard(args) {
			writeJSONError(w, http.StatusForbidden, "wildcard authorization requires admin access")
			return
		}
		allowed, err := s.canUseKey(r.Context(), record, session, args.ID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !allowed {
			writeJSONError(w, http.StatusForbidden, "key access denied")
			return
		}
		result, err := s.Client.Allow(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.clearFleetCache()
		writeJSON(w, http.StatusCreated, result)
	case action == "revoke" && r.Method == http.MethodPost:
		var args protocol.RevokeArgs
		if err := decodeJSONBody(r, &args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(args.ID) == "" {
			writeJSONError(w, http.StatusBadRequest, "id is required")
			return
		}
		allowed, err := s.canRevoke(r.Context(), record, session, args.ID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !allowed {
			writeJSONError(w, http.StatusForbidden, "revoke access denied")
			return
		}
		result, err := s.Client.Revoke(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.clearFleetCache()
		writeJSON(w, http.StatusOK, result)
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func unsupportedExternalSessionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "external_session_id") &&
		(strings.Contains(message, "unknown") || strings.Contains(message, "unrecognized") || strings.Contains(message, "unsupported"))
}

func (s *Server) handleFleetSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out, err := s.cachedFleetSnapshot(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	session, _ := currentSession(r)
	out = filterFleetSnapshot(out, session)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) cachedFleetSnapshot(ctx context.Context) (fleetSnapshot, error) {
	s.fleetCacheMu.Lock()
	now := s.nowTime()
	if s.fleetCacheOK && now.Sub(s.fleetCacheAt) < s.fleetSnapshotCacheTTL() {
		out := s.fleetCache
		s.fleetCacheMu.Unlock()
		return out, nil
	}
	if call := s.fleetRefresh; call != nil {
		s.fleetCacheMu.Unlock()
		select {
		case <-ctx.Done():
			return fleetSnapshot{}, ctx.Err()
		case <-call.done:
			return call.result, call.err
		}
	}
	call := &fleetRefreshCall{done: make(chan struct{})}
	s.fleetRefresh = call
	s.fleetCacheMu.Unlock()

	go s.refreshFleetSnapshot(call)
	select {
	case <-ctx.Done():
		return fleetSnapshot{}, ctx.Err()
	case <-call.done:
		return call.result, call.err
	}
}

func (s *Server) refreshFleetSnapshot(call *fleetRefreshCall) {
	refreshCtx, cancel := context.WithTimeout(context.Background(), fleetRefreshTimeout)
	defer cancel()
	out, err := s.fetchFleetSnapshot(refreshCtx)

	s.fleetCacheMu.Lock()
	call.result = out
	call.err = err
	if err == nil {
		s.fleetCache = out
		s.fleetCacheAt = s.nowTime()
		s.fleetCacheOK = true
	}
	s.fleetRefresh = nil
	close(call.done)
	s.fleetCacheMu.Unlock()
}

func (s *Server) fetchFleetSnapshot(ctx context.Context) (fleetSnapshot, error) {
	records, err := s.Registry.List()
	if err != nil {
		return fleetSnapshot{}, err
	}
	out := fleetSnapshot{Servers: make([]serverSnapshot, len(records))}
	if len(records) == 0 {
		return out, nil
	}
	type snapshotJob struct {
		index  int
		record ServerRecord
	}
	jobs := make(chan snapshotJob)
	var wg sync.WaitGroup
	budget := &fleetSnapshotBudget{}
	workers := min(maxFleetWorkers, len(records))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				nodeCtx, cancel := context.WithTimeout(ctx, fleetNodeTimeout)
				snapshot, err := s.Client.Snapshot(nodeCtx, job.record)
				cancel()
				item := serverSnapshot{Server: publicServerRecord(job.record)}
				if err != nil {
					item.Error = boundedFleetError(err)
				} else if err := budget.accept(snapshot); err != nil {
					item.Error = err.Error()
				} else {
					item.Online = true
					item.Snapshot = &snapshot
				}
				out.Servers[job.index] = item
			}
		}()
	}
	dispatching := true
	for i, record := range records {
		select {
		case jobs <- snapshotJob{index: i, record: record}:
		case <-ctx.Done():
			dispatching = false
		}
		if !dispatching {
			break
		}
	}
	close(jobs)
	wg.Wait()
	for i, record := range records {
		if out.Servers[i].Server.ID != "" {
			continue
		}
		message := "snapshot unavailable"
		if err := ctx.Err(); err != nil {
			message = err.Error()
		}
		out.Servers[i] = serverSnapshot{Server: publicServerRecord(record), Error: message}
	}
	return out, nil
}

func boundedFleetError(err error) string {
	message := err.Error()
	if len(message) > maxFleetErrorBytes {
		return message[:maxFleetErrorBytes]
	}
	return message
}

func (b *fleetSnapshotBudget) accept(snapshot model.NodeSnapshot) error {
	records, processes, bytes, err := nodeSnapshotCost(snapshot)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.records+records > maxFleetSnapshotRecords ||
		b.processes+processes > maxFleetSnapshotProcesses ||
		b.bytes+bytes > maxFleetSnapshotRetainedSize {
		return errors.New("fleet snapshot budget exceeded")
	}
	b.records += records
	b.processes += processes
	b.bytes += bytes
	return nil
}

func nodeSnapshotCost(snapshot model.NodeSnapshot) (records, processes int, bytes int64, err error) {
	records = len(snapshot.GPUs) + len(snapshot.Tokens) + len(snapshot.Reservations) +
		len(snapshot.Authorizations) + len(snapshot.SoftClaims) + len(snapshot.Leases) +
		len(snapshot.Bypasses) + len(snapshot.PS)
	for _, gpu := range snapshot.GPUs {
		processes += len(gpu.Processes)
	}
	for _, authorization := range snapshot.Authorizations {
		records += len(authorization.Command)
	}
	for _, lease := range snapshot.Leases {
		records += len(lease.Command)
	}
	records += processes
	if records > maxNodeSnapshotRecords {
		return 0, 0, 0, fmt.Errorf("node snapshot exceeds %d records", maxNodeSnapshotRecords)
	}
	if processes > maxNodeSnapshotProcesses {
		return 0, 0, 0, fmt.Errorf("node snapshot exceeds %d processes", maxNodeSnapshotProcesses)
	}

	bytes = 1024 + int64(records)*512
	add := func(values ...string) {
		for _, value := range values {
			bytes += int64(len(value))
		}
	}
	add(snapshot.Hostname)
	for _, gpu := range snapshot.GPUs {
		add(gpu.State)
		for _, process := range gpu.Processes {
			add(process.Name)
		}
	}
	for _, token := range snapshot.Tokens {
		add(token.ID, token.Key, token.KeyStatus, token.Name, token.Mode)
	}
	for _, reservation := range snapshot.Reservations {
		add(reservation.ID, reservation.GroupID, reservation.Holder, reservation.Purpose)
	}
	for _, authorization := range snapshot.Authorizations {
		add(authorization.ID, authorization.TokenID, authorization.Mode, authorization.TokenMode,
			authorization.Holder, authorization.Username, authorization.ContainerID,
			authorization.ContainerPattern, authorization.Namespace)
		add(authorization.Command...)
	}
	for _, claim := range snapshot.SoftClaims {
		add(claim.ID, claim.AuthorizationID, claim.Holder)
	}
	for _, lease := range snapshot.Leases {
		add(lease.ID, lease.Mode, lease.TokenHash, lease.Holder, lease.CgroupPath,
			lease.CgroupRel, lease.ContainerID, lease.Namespace)
		add(lease.Command...)
	}
	for _, bypass := range snapshot.Bypasses {
		add(bypass.ID, bypass.Type, bypass.Command, bypass.Reason)
	}
	for _, row := range snapshot.PS {
		add(row.ID, row.GPU, row.User, row.Command)
	}
	if bytes > maxFleetSnapshotRetainedSize {
		return 0, 0, 0, errors.New("node snapshot retained size is too large")
	}
	return records, processes, bytes, nil
}

func (s *Server) nowTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) fleetSnapshotCacheTTL() time.Duration {
	if s.fleetCacheTTL > 0 {
		return s.fleetCacheTTL
	}
	return time.Second
}

func (s *Server) clearFleetCache() {
	s.fleetCacheMu.Lock()
	defer s.fleetCacheMu.Unlock()
	s.fleetCacheOK = false
}

func (s *Server) canRevoke(ctx context.Context, record ServerRecord, session sessionInfo, id string) (bool, error) {
	if session.Role == RoleAdmin {
		return true, nil
	}
	status, err := s.Client.ShowKeys(ctx, record, record.RootKey)
	if err != nil {
		return false, err
	}
	if token, ok := findToken(status.Tokens, id); ok {
		return sameOwner(token.Name, session.User), nil
	}
	if reservation, ok := findReservation(status.Reservations, id); ok {
		return sameOwner(reservation.Holder, session.User), nil
	}
	if authorization, ok := findAuthorization(status.Authorizations, id); ok {
		return sameOwner(authorization.Holder, session.User), nil
	}
	return false, nil
}

func (s *Server) canUseKey(ctx context.Context, record ServerRecord, session sessionInfo, id string) (bool, error) {
	if session.Role == RoleAdmin {
		return true, nil
	}
	status, err := s.Client.ShowKeys(ctx, record, record.RootKey)
	if err != nil {
		return false, err
	}
	token, ok := findToken(status.Tokens, id)
	return ok && sameOwner(token.Name, session.User), nil
}

func filterFleetSnapshot(in fleetSnapshot, session sessionInfo) fleetSnapshot {
	if session.Role == RoleAdmin {
		return in
	}
	out := fleetSnapshot{Servers: make([]serverSnapshot, 0, len(in.Servers))}
	for _, item := range in.Servers {
		filtered := item
		if item.Snapshot != nil {
			snapshot := *item.Snapshot
			snapshot.GPUs = cloneFilteredGPUSnapshots(snapshot.GPUs)
			snapshot.Tokens = filterTokens(snapshot.Tokens, session.User)
			snapshot.Reservations = scrubReservations(snapshot.Reservations, session.User)
			snapshot.Authorizations = filterAuthorizations(snapshot.Authorizations, session.User)
			snapshot.SoftClaims = filterSoftClaims(snapshot.SoftClaims, session.User)
			snapshot.Leases = filterLeases(snapshot.Leases, session.User)
			snapshot.Bypasses = nil
			snapshot.PS = nil
			for i := range snapshot.GPUs {
				if snapshot.GPUs[i].Reservation != nil {
					reservation := *snapshot.GPUs[i].Reservation
					if !sameOwner(reservation.Holder, session.User) {
						reservation.GroupID = ""
					}
					snapshot.GPUs[i].Reservation = &reservation
				}
				if snapshot.GPUs[i].Claim != nil {
					if !sameOwner(snapshot.GPUs[i].Claim.Holder, session.User) {
						snapshot.GPUs[i].Claim = nil
					} else {
						claim := *snapshot.GPUs[i].Claim
						snapshot.GPUs[i].Claim = &claim
					}
				}
			}
			filtered.Snapshot = &snapshot
		}
		out.Servers = append(out.Servers, filtered)
	}
	return out
}

func cloneFilteredGPUSnapshots(gpus []model.GPUSnapshot) []model.GPUSnapshot {
	out := append([]model.GPUSnapshot(nil), gpus...)
	for i := range out {
		out[i].Processes = nil
	}
	return out
}

func filterTokens(tokens []model.TokenView, user string) []model.TokenView {
	out := make([]model.TokenView, 0)
	for _, token := range tokens {
		if sameOwner(token.Name, user) {
			out = append(out, token)
		}
	}
	return out
}

func scrubReservations(reservations []model.ReservationView, user string) []model.ReservationView {
	out := make([]model.ReservationView, 0, len(reservations))
	for _, reservation := range reservations {
		if !sameOwner(reservation.Holder, user) {
			reservation.GroupID = ""
		}
		out = append(out, reservation)
	}
	return out
}

func filterAuthorizations(authorizations []model.AuthorizationView, user string) []model.AuthorizationView {
	out := make([]model.AuthorizationView, 0)
	for _, authorization := range authorizations {
		if sameOwner(authorization.Holder, user) {
			out = append(out, authorization)
		}
	}
	return out
}

func filterSoftClaims(claims []model.SoftClaimView, user string) []model.SoftClaimView {
	out := make([]model.SoftClaimView, 0)
	for _, claim := range claims {
		if sameOwner(claim.Holder, user) {
			out = append(out, claim)
		}
	}
	return out
}

func filterLeases(leases []model.Lease, user string) []model.Lease {
	out := make([]model.Lease, 0)
	for _, lease := range leases {
		if sameOwner(lease.Holder, user) {
			out = append(out, lease)
		}
	}
	return out
}

func findToken(tokens []model.TokenView, id string) (model.TokenView, bool) {
	for _, token := range tokens {
		if token.ID == id {
			return token, true
		}
	}
	return model.TokenView{}, false
}

func reservationTokenID(result model.RegisterResult, status model.KeyStatus) string {
	if result.Token != "" {
		for _, token := range status.Tokens {
			if token.Key == result.Token {
				return token.ID
			}
		}
	}
	reservationIDs := make(map[string]struct{}, len(result.ReservationIDs))
	for _, id := range result.ReservationIDs {
		reservationIDs[id] = struct{}{}
	}
	for _, reservation := range status.Reservations {
		if _, ok := reservationIDs[reservation.ID]; ok && reservation.GroupID != "" {
			return reservation.GroupID
		}
	}
	return ""
}

func findReservation(reservations []model.ReservationView, id string) (model.ReservationView, bool) {
	for _, reservation := range reservations {
		if reservation.ID == id {
			return reservation, true
		}
	}
	return model.ReservationView{}, false
}

func findAuthorization(authorizations []model.AuthorizationView, id string) (model.AuthorizationView, bool) {
	for _, authorization := range authorizations {
		if authorization.ID == id {
			return authorization, true
		}
	}
	return model.AuthorizationView{}, false
}

func sameOwner(left, right string) bool {
	leftName, leftErr := normalizeUsername(left)
	rightName, rightErr := normalizeUsername(right)
	return leftErr == nil && rightErr == nil && leftName == rightName
}

func allowArgsHaveWildcard(args protocol.AllowArgs) bool {
	return strings.Contains(args.Container, "*") || strings.Contains(args.Namespace, "*") || strings.Contains(args.User, "*")
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

const maxAPIRequestBytes = 64 << 10

func (s *Server) securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "Cookie")
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxAPIRequestBytes)
			}
			if requestChangesState(r.Method) && !sameOriginRequest(r) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request denied")
				return
			}
			if requestChangesState(r.Method) && requestHasBody(r) && !requestHasJSONContentType(r) {
				writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func requestChangesState(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func requestHasBody(r *http.Request) bool {
	return r.Body != nil && (r.ContentLength != 0 || len(r.TransferEncoding) > 0)
}

func requestHasJSONContentType(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func sameOriginRequest(r *http.Request) bool {
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

func decodeJSONBody(r *http.Request, dst any) error {
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
