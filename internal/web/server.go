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

	"rocguard/internal/config"
	"rocguard/internal/model"
	"rocguard/internal/protocol"
)

type Server struct {
	Cfg      config.Config
	Registry *Registry
	Users    *UserStore
	Client   NodeAPI

	fleetCacheMu  sync.Mutex
	fleetCache    fleetSnapshot
	fleetCacheAt  time.Time
	fleetCacheOK  bool
	fleetCacheTTL time.Duration
	now           func() time.Time
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

type serverSnapshot struct {
	Server   PublicServerRecord  `json:"server"`
	Online   bool                `json:"online"`
	Error    string              `json:"error,omitempty"`
	Snapshot *model.NodeSnapshot `json:"snapshot,omitempty"`
}

func New(cfg config.Config) *Server {
	return &Server{
		Cfg:           cfg,
		Registry:      NewRegistry(cfg.WebRegistry),
		Users:         NewUserStore(cfg.WebUsers),
		Client:        NodeClient{Timeout: 4 * time.Second},
		fleetCacheTTL: time.Second,
		now:           time.Now,
	}
}

func (s *Server) Run(ctx context.Context) error {
	if strings.TrimSpace(s.Cfg.WebPassword) == "" {
		return errors.New("ROCGUARD_WEB_PASSWORD is required")
	}
	if err := s.Users.BootstrapAdmin(s.Cfg.WebUser, s.Cfg.WebPassword); err != nil {
		return err
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
	mux.HandleFunc("/api/register", s.handleRegister)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("/api/password", s.requireSession(s.handleChangePassword))
	mux.HandleFunc("/api/users", s.requireAdmin(s.handleUsers))
	mux.HandleFunc("/api/servers", s.requireSession(s.handleServers))
	mux.HandleFunc("/api/servers/", s.requireSession(s.handleServerAction))
	mux.HandleFunc("/api/fleet/snapshot", s.requireSession(s.handleFleetSnapshot))
	mux.HandleFunc("/", s.handleStatic)
	return mux
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		user, err := s.Users.Create(req.Username, req.Password, req.Role)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, user)
	case http.MethodDelete:
		session, _ := currentSession(r)
		var req deleteUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		args.Name = session.User
		result, err := s.Client.CreateReservation(r.Context(), record, args)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if result.TokenID == "" {
			if status, statusErr := s.Client.ShowKeys(r.Context(), record, record.RootKey); statusErr == nil {
				result.TokenID = reservationTokenID(result, status)
			}
		}
		s.clearFleetCache()
		result.Token = ""
		writeJSON(w, http.StatusCreated, result)
	case action == "claim-keys" && r.Method == http.MethodPost:
		var args protocol.RegisterArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		args.ID = strings.TrimSpace(args.ID)
		args.Mode = strings.TrimSpace(args.Mode)
		if args.ID == "" {
			writeJSONError(w, http.StatusBadRequest, "id is required")
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
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
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
	defer s.fleetCacheMu.Unlock()

	now := s.nowTime()
	if s.fleetCacheOK && now.Sub(s.fleetCacheAt) < s.fleetSnapshotCacheTTL() {
		return s.fleetCache, nil
	}
	out, err := s.fetchFleetSnapshot(ctx)
	if err != nil {
		return fleetSnapshot{}, err
	}
	s.fleetCache = out
	s.fleetCacheAt = now
	s.fleetCacheOK = true
	return out, nil
}

func (s *Server) fetchFleetSnapshot(ctx context.Context) (fleetSnapshot, error) {
	records, err := s.Registry.List()
	if err != nil {
		return fleetSnapshot{}, err
	}
	out := fleetSnapshot{Servers: make([]serverSnapshot, len(records))}
	var wg sync.WaitGroup
	for i, record := range records {
		wg.Add(1)
		go func(i int, record ServerRecord) {
			defer wg.Done()
			nodeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			snapshot, err := s.Client.Snapshot(nodeCtx, record)
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
	return out, nil
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
			snapshot.Tokens = filterTokens(snapshot.Tokens, session.User)
			snapshot.Reservations = scrubReservations(snapshot.Reservations, session.User)
			snapshot.Authorizations = filterAuthorizations(snapshot.Authorizations, session.User)
			snapshot.SoftClaims = filterSoftClaims(snapshot.SoftClaims, session.User)
			snapshot.Leases = filterLeases(snapshot.Leases, session.User)
			snapshot.Bypasses = nil
			for i := range snapshot.GPUs {
				if snapshot.GPUs[i].Reservation != nil && !sameOwner(snapshot.GPUs[i].Reservation.Holder, session.User) {
					reservation := *snapshot.GPUs[i].Reservation
					reservation.GroupID = ""
					snapshot.GPUs[i].Reservation = &reservation
				}
				if snapshot.GPUs[i].Claim != nil && !sameOwner(snapshot.GPUs[i].Claim.Holder, session.User) {
					snapshot.GPUs[i].Claim = nil
				}
			}
			filtered.Snapshot = &snapshot
		}
		out.Servers = append(out.Servers, filtered)
	}
	return out
}

func filterTokens(tokens []model.TokenView, user string) []model.TokenView {
	out := make([]model.TokenView, 0, len(tokens))
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
	out := make([]model.AuthorizationView, 0, len(authorizations))
	for _, authorization := range authorizations {
		if sameOwner(authorization.Holder, user) {
			out = append(out, authorization)
		}
	}
	return out
}

func filterSoftClaims(claims []model.SoftClaimView, user string) []model.SoftClaimView {
	out := make([]model.SoftClaimView, 0, len(claims))
	for _, claim := range claims {
		if sameOwner(claim.Holder, user) {
			out = append(out, claim)
		}
	}
	return out
}

func filterLeases(leases []model.Lease, user string) []model.Lease {
	out := make([]model.Lease, 0, len(leases))
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
