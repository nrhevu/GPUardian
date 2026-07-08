package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"rocguardd/internal/amdsmi"
	"rocguardd/internal/config"
	"rocguardd/internal/enforce"
	"rocguardd/internal/model"
	"rocguardd/internal/proc"
	"rocguardd/internal/protocol"
	"rocguardd/internal/runtime"
	"rocguardd/internal/store"
)

type Server struct {
	Cfg      config.Config
	Store    *store.Store
	AMD      amdsmi.Provider
	Proc     proc.Reader
	Runtime  runtime.Resolver
	Killer   enforce.Killer
	Interval time.Duration
}

type peer struct {
	PID    int
	UID    int
	GID    int
	Groups []uint32
}

func New(cfg config.Config) *Server {
	st := store.New(cfg)
	return &Server{
		Cfg:      cfg,
		Store:    st,
		AMD:      amdsmi.NewCLIProvider(),
		Proc:     proc.NewFSReader(cfg.ProcRoot),
		Runtime:  runtime.CLIResolver{},
		Killer:   enforce.RealKiller{Grace: 2 * time.Second},
		Interval: time.Second,
	}
}

func (s *Server) Run(ctx context.Context) error {
	if err := s.Store.Load(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Cfg.SocketPath), 0755); err != nil {
		return err
	}
	_ = os.Remove(s.Cfg.SocketPath)
	listener, err := net.Listen("unix", s.Cfg.SocketPath)
	if err != nil {
		return err
	}
	defer listener.Close()
	_ = os.Chmod(s.Cfg.SocketPath, 0666)

	go s.monitor(ctx)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	p := peer{PID: -1, UID: -1, GID: -1}
	if resolved, err := peerCred(conn, s.Cfg.ProcRoot); err == nil {
		p = resolved
	}
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		var req protocol.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
			continue
		}
		if req.Method == "run" {
			s.handleRun(ctx, conn, p, req)
			continue
		}
		result, err := s.dispatch(ctx, p, req)
		if err != nil {
			writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
			continue
		}
		data, _ := json.Marshal(result)
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: true, Result: data})
	}
}

func (s *Server) dispatch(ctx context.Context, p peer, req protocol.Request) (any, error) {
	now := time.Now()
	switch req.Method {
	case "register":
		var args protocol.RegisterArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Mode) == "" {
			return nil, errors.New("mode must be reserved or claimed")
		}
		if ok, err := s.Store.ValidateRootKey(args.RootKey); err != nil {
			return nil, err
		} else if !ok {
			return nil, store.ErrInvalidRootKey
		}
		switch store.NormalizeTokenMode(args.Mode) {
		case model.TokenModeReserved:
			if err := validateGPUs(args.GPUs); err != nil {
				return nil, err
			}
			for _, gpu := range args.GPUs {
				if err := s.ensureGPUCanReserve(ctx, gpu); err != nil {
					return nil, err
				}
			}
			secret, token, reservations, err := s.Store.RegisterHardReservations(args.RootKey, args.Name, args.GPUs, args.TTL, now)
			if err != nil {
				return nil, err
			}
			ids := make([]string, 0, len(reservations))
			gpus := make([]int, 0, len(reservations))
			for _, reservation := range reservations {
				ids = append(ids, reservation.ID)
				gpus = append(gpus, reservation.GPU)
			}
			return model.RegisterResult{Token: secret, Mode: token.Mode, ReservationIDs: ids, GPUs: gpus, ExpiresAt: timePtrIfSet(token.ExpiresAt)}, nil
		case model.TokenModeClaimed:
			secret, token, err := s.Store.RegisterSoftToken(args.RootKey, args.Name, now)
			if err != nil {
				return nil, err
			}
			return model.RegisterResult{Token: secret, Mode: token.Mode}, nil
		default:
			return nil, errors.New("mode must be reserved or claimed")
		}
	case "allow_docker":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.DockerAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createDockerAuthorization(ctx, token, tokenHash, p, args)
	case "allow_k8s":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.K8sAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createK8sAuthorization(ctx, token, tokenHash, p, args)
	case "allow_user":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.UserAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createUserAuthorization(token, tokenHash, p, args)
	case "status":
		return s.Store.Status(now)
	case "ps":
		return s.ps(ctx, now)
	case "who":
		var args protocol.WhoArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		rows, err := s.ps(ctx, now)
		if err != nil {
			return nil, err
		}
		var out []model.PSRow
		for _, row := range rows {
			if row.GPU == args.GPU {
				out = append(out, row)
			}
		}
		return out, nil
	case "token_info":
		var args protocol.TokenInfoArgs
		if len(req.Args) > 0 {
			_ = json.Unmarshal(req.Args, &args)
		}
		token := args.Token
		if token == "" {
			token = req.Token
		}
		return s.Store.TokenView(token, now)
	case "show_keys":
		var args protocol.RootKeyArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.Store.KeyStatus(args.RootKey, now)
	case "bypass_add":
		var args protocol.BypassAddArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		if ok, err := s.Store.ValidateRootKey(args.RootKey); err != nil {
			return nil, err
		} else if !ok {
			return nil, store.ErrInvalidRootKey
		}
		return s.addBypass(args, now)
	case "revoke":
		var args protocol.RevokeArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		if ok, err := s.Store.ValidateRootKey(args.RootKey); err != nil {
			return nil, err
		} else if !ok {
			return nil, store.ErrInvalidRootKey
		}
		return map[string]string{"revoked": args.ID}, s.Store.Revoke(args.ID)
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}

func (s *Server) handleRun(ctx context.Context, conn net.Conn, p peer, req protocol.Request) {
	now := time.Now()
	token, tokenHash, err := s.validateToken(req.Token, now)
	if err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	var args protocol.RunArgs
	if err := json.Unmarshal(req.Args, &args); err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	result, err := s.runCommand(ctx, conn, req.ID, token, tokenHash, p, args)
	if err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	data, _ := json.Marshal(result)
	writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: true, Result: data})
}

func (s *Server) validateToken(secret string, now time.Time) (model.Token, string, error) {
	if strings.TrimSpace(secret) == "" {
		return model.Token{}, "", errors.New("KEY token is required")
	}
	return s.Store.ValidateToken(secret, now)
}

func (s *Server) createDockerAuthorization(ctx context.Context, token model.Token, tokenHash string, p peer, args protocol.DockerAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	container := strings.TrimSpace(args.Container)
	if container == "" {
		return model.AllowResult{}, errors.New("container is required")
	}
	var containerID string
	var containerPattern string
	if hasWildcard(container) {
		containerPattern = container
	} else {
		var err error
		containerID, err = s.Runtime.ResolveDockerContainer(ctx, container)
		if err != nil {
			return model.AllowResult{}, fmt.Errorf("resolve docker container: %w", err)
		}
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:               store.NewAuthorizationID(),
		Mode:             model.ModeDocker,
		TokenHash:        tokenHash,
		TokenMode:        store.NormalizeTokenMode(token.Mode),
		Holder:           token.Name,
		UID:              p.UID,
		GID:              p.GID,
		ContainerID:      containerID,
		ContainerPattern: containerPattern,
		CreatedAt:        now.UTC(),
		ExpiresAt:        token.ExpiresAt,
		Active:           true,
	}
	if err := s.Store.AddAuthorization(authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, ContainerID: containerID, ContainerPattern: containerPattern, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func (s *Server) createK8sAuthorization(ctx context.Context, token model.Token, tokenHash string, p peer, args protocol.K8sAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	if strings.TrimSpace(args.Namespace) == "" {
		return model.AllowResult{}, errors.New("namespace is required")
	}
	if _, err := exec.LookPath("crictl"); err != nil {
		if _, err2 := exec.LookPath("kubectl"); err2 != nil {
			return model.AllowResult{}, errors.New("k8s mode requires crictl or kubectl")
		}
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:        store.NewAuthorizationID(),
		Mode:      model.ModeK8s,
		TokenHash: tokenHash,
		TokenMode: store.NormalizeTokenMode(token.Mode),
		Holder:    token.Name,
		UID:       p.UID,
		GID:       p.GID,
		Namespace: strings.TrimSpace(args.Namespace),
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := s.Store.AddAuthorization(authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, Namespace: authorization.Namespace, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func (s *Server) createUserAuthorization(token model.Token, tokenHash string, p peer, args protocol.UserAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	username := strings.TrimSpace(args.User)
	if username == "" {
		return model.AllowResult{}, errors.New("user is required")
	}
	uid := -1
	if !hasWildcard(username) {
		var err error
		uid, err = lookupUID(username)
		if err != nil {
			return model.AllowResult{}, err
		}
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:        store.NewAuthorizationID(),
		Mode:      model.ModeUser,
		TokenHash: tokenHash,
		TokenMode: store.NormalizeTokenMode(token.Mode),
		Holder:    token.Name,
		UID:       uid,
		GID:       p.GID,
		Username:  username,
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := s.Store.AddAuthorization(authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, Username: authorization.Username, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func hasWildcard(value string) bool {
	return strings.Contains(value, "*")
}

func (s *Server) runCommand(ctx context.Context, conn net.Conn, reqID string, token model.Token, tokenHash string, p peer, args protocol.RunArgs) (model.RunResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.RunResult{}, err
	}
	if len(args.Command) == 0 {
		return model.RunResult{}, errors.New("command is required")
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:        store.NewAuthorizationID(),
		Mode:      model.ModeBare,
		TokenHash: tokenHash,
		TokenMode: store.NormalizeTokenMode(token.Mode),
		Holder:    token.Name,
		UID:       p.UID,
		GID:       p.GID,
		Command:   args.Command,
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	cgroupPath, cgroupRel, err := s.createCgroup(authorization.ID)
	if err != nil {
		return model.RunResult{}, err
	}
	authorization.CgroupPath = cgroupPath
	authorization.CgroupRel = cgroupRel
	cmd := exec.CommandContext(ctx, args.Command[0], args.Command[1:]...)
	cmd.Dir = args.Workdir
	cmd.Env = commandEnv(args.Env)
	sys := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 && p.UID >= 0 && p.GID >= 0 {
		sys.Credential = &syscall.Credential{Uid: uint32(p.UID), Gid: uint32(p.GID), Groups: p.Groups}
	}
	cmd.SysProcAttr = sys
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return model.RunResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return model.RunResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return model.RunResult{}, err
	}
	authorization.RootPID = cmd.Process.Pid
	if err := s.movePIDToCgroup(cgroupPath, authorization.RootPID); err != nil {
		_ = cmd.Process.Kill()
		return model.RunResult{}, err
	}
	if err := s.Store.AddAuthorization(authorization); err != nil {
		_ = cmd.Process.Kill()
		return model.RunResult{}, err
	}
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go streamCopy(&wg, &writeMu, conn, reqID, protocol.KindStdout, stdout)
	go streamCopy(&wg, &writeMu, conn, reqID, protocol.KindStderr, stderr)
	waitErr := cmd.Wait()
	wg.Wait()
	_ = s.Store.ReleaseAuthorization(authorization.ID)
	_ = os.Remove(cgroupPath)
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr != nil && exitCode == 0 {
		return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, waitErr
	}
	return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, nil
}

func (s *Server) ensureGPUCanReserve(ctx context.Context, gpu int) error {
	if gpu < 0 {
		return errors.New("gpu must be >= 0")
	}
	state, err := s.Store.Snapshot()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, reservation := range state.Reservations {
		if reservation.GPU == gpu && reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			return fmt.Errorf("gpu %d already reserved by %s", gpu, reservation.ID)
		}
	}
	for _, lease := range state.Leases {
		if lease.GPU == gpu && lease.Active && now.Before(lease.ExpiresAt) {
			return fmt.Errorf("gpu %d already held by legacy lease %s", gpu, lease.ID)
		}
	}
	processes, err := s.AMD.Processes(ctx)
	if err != nil {
		return fmt.Errorf("amd-smi process list: %w", err)
	}
	auth := s.authorizer()
	busy, err := auth.BusyProcessesForGPU(ctx, state, processes, gpu)
	if err != nil {
		return err
	}
	if len(busy) > 0 {
		first := busy[0]
		return fmt.Errorf("gpu %d is busy: pid=%d cmd=%s", gpu, first.Process.PID, strings.Join(first.Info.Cmdline, " "))
	}
	return nil
}

func (s *Server) ensureTokenCanAuthorize(tokenHash string, token model.Token, now time.Time) error {
	if store.NormalizeTokenMode(token.Mode) != model.TokenModeReserved {
		return nil
	}
	state, err := s.Store.Snapshot()
	if err != nil {
		return err
	}
	for _, reservation := range state.Reservations {
		if reservation.TokenHash == tokenHash && reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			return nil
		}
	}
	return errors.New("reserved token has no active reservation")
}

func (s *Server) ps(ctx context.Context, now time.Time) ([]model.PSRow, error) {
	state, err := s.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	processes, err := s.AMD.Processes(ctx)
	if err != nil {
		processes = nil
	}
	auth := s.authorizer()
	auth.DryRun = true
	auth.Killer = nil
	auth.OnAudit = nil
	decisions, err := auth.Enforce(ctx, state, processes)
	if err != nil {
		return nil, err
	}
	var rows []model.PSRow
	liveHard := map[string]bool{}
	for _, decision := range decisions {
		if decision.Action != "allow" || decision.Reason == "bypass" {
			continue
		}
		id := decision.AuthID
		if id == "" {
			id = decision.LeaseID
		}
		if id == "" {
			continue
		}
		holder := strings.TrimSpace(decision.Holder)
		if holder == "" {
			holder = "unknown"
		}
		command := strings.Join(decision.Info.Cmdline, " ")
		if command == "" {
			command = decision.Process.Name
		}
		if command == "" {
			command = fmt.Sprintf("pid %d", decision.Process.PID)
		}
		rows = append(rows, model.PSRow{
			ID:      fmt.Sprintf("%s/%d", id, decision.Process.PID),
			GPU:     decision.Process.GPU,
			User:    holder,
			Command: command,
		})
		if decision.TokenHash != "" {
			liveHard[reservationLiveKey(decision.Process.GPU, decision.TokenHash)] = true
		}
	}
	for _, reservation := range state.Reservations {
		if !reservation.Active || reservation.Revoked || !now.Before(reservation.ExpiresAt) || liveHard[reservationLiveKey(reservation.GPU, reservation.TokenHash)] {
			continue
		}
		rows = append(rows, model.PSRow{
			ID:      reservation.ID,
			GPU:     reservation.GPU,
			User:    reservation.Holder,
			Command: "reserved until " + reservation.ExpiresAt.Format(time.RFC3339),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].GPU != rows[j].GPU {
			return rows[i].GPU < rows[j].GPU
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

func validateGPUs(gpus []int) error {
	if len(gpus) == 0 {
		return errors.New("at least one gpu is required")
	}
	seen := map[int]bool{}
	for _, gpu := range gpus {
		if gpu < 0 {
			return errors.New("gpu must be >= 0")
		}
		if seen[gpu] {
			return fmt.Errorf("duplicate gpu %d", gpu)
		}
		seen[gpu] = true
	}
	return nil
}

func lookupUID(username string) (int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return -1, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return -1, err
	}
	return uid, nil
}

func reservationLiveKey(gpu int, tokenHash string) string {
	return fmt.Sprintf("%d:%s", gpu, tokenHash)
}

func timeIsSet(value time.Time) bool {
	return !value.IsZero()
}

func timePtrIfSet(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	out := value
	return &out
}

func (s *Server) addBypass(args protocol.BypassAddArgs, now time.Time) (model.BypassRule, error) {
	ttl, err := store.ParseTTL(args.TTL, store.DefaultTokenTTL, 30*24*time.Hour)
	if err != nil {
		return model.BypassRule{}, err
	}
	rule := model.BypassRule{
		ID:        store.NewBypassID(),
		Type:      args.Type,
		PID:       args.PID,
		Command:   args.Command,
		UID:       args.UID,
		Reason:    args.Reason,
		CreatedAt: now.UTC(),
		ExpiresAt: now.UTC().Add(ttl),
	}
	switch rule.Type {
	case model.BypassPID:
		if rule.PID <= 0 {
			return model.BypassRule{}, errors.New("pid bypass requires --pid")
		}
	case model.BypassCommand:
		if rule.Command == "" || !filepath.IsAbs(rule.Command) {
			return model.BypassRule{}, errors.New("command bypass requires absolute --command")
		}
	default:
		return model.BypassRule{}, errors.New("bypass type must be pid or command")
	}
	if err := s.Store.AddBypass(rule); err != nil {
		return model.BypassRule{}, err
	}
	return rule, nil
}

func (s *Server) monitor(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.monitorOnce(ctx)
		}
	}
}

func (s *Server) monitorOnce(ctx context.Context) {
	s.cleanupExpiredReservations()
	s.cleanupExpiredAuthorizations()
	s.cleanupFinishedBareAuthorizations()
	s.cleanupExpiredLeases()
	s.cleanupFinishedBareLeases()
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	processes, err := s.AMD.Processes(ctx)
	if err != nil {
		_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "error", Message: "amd-smi process list failed: " + err.Error()})
		return
	}
	decisions, _ := s.authorizer().Enforce(ctx, state, processes)
	now := time.Now()
	for _, decision := range decisions {
		switch decision.Action {
		case "claim":
			_ = s.Store.UpsertSoftClaim(decision.Claim, now)
		case "release_claim":
			_ = s.Store.ReleaseSoftClaim(decision.ClaimID)
		}
	}
}

func (s *Server) cleanupFinishedBareAuthorizations() {
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	now := time.Now()
	for _, authorization := range state.Authorizations {
		if !authorization.Active || authorization.Mode != model.ModeBare || authorization.RootPID <= 0 {
			continue
		}
		if s.Proc != nil && s.Proc.Exists(authorization.RootPID) {
			continue
		}
		if authorization.CgroupPath != "" && !cgroupEmpty(authorization.CgroupPath) {
			continue
		}
		_ = s.Store.ReleaseAuthorization(authorization.ID)
		if authorization.CgroupPath != "" {
			_ = os.Remove(authorization.CgroupPath)
		}
		_ = s.Store.AppendAudit(model.AuditEvent{
			Time:    now.UTC(),
			Kind:    "authorization_released",
			Message: "bare authorization released after process exit",
			LeaseID: authorization.ID,
			User:    authorization.Holder,
		})
	}
}

func (s *Server) cleanupExpiredAuthorizations() {
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	now := time.Now()
	for _, authorization := range state.Authorizations {
		if !authorization.Active || !timeIsSet(authorization.ExpiresAt) || now.Before(authorization.ExpiresAt) {
			continue
		}
		if authorization.RootPID > 0 {
			_ = syscall.Kill(-authorization.RootPID, syscall.SIGTERM)
			time.Sleep(500 * time.Millisecond)
			_ = syscall.Kill(-authorization.RootPID, syscall.SIGKILL)
		}
		_ = s.Store.ReleaseAuthorization(authorization.ID)
		if authorization.CgroupPath != "" {
			_ = os.Remove(authorization.CgroupPath)
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: "authorization_expired", Message: "authorization expired", LeaseID: authorization.ID, User: authorization.Holder})
	}
}

func (s *Server) cleanupExpiredReservations() {
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	now := time.Now()
	for _, reservation := range state.Reservations {
		if !reservation.Active || now.Before(reservation.ExpiresAt) {
			continue
		}
		_ = s.Store.ReleaseReservation(reservation.ID)
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: "reservation_expired", Message: "reserved GPU reservation expired", GPU: reservation.GPU, LeaseID: reservation.ID, User: reservation.Holder})
	}
}

func (s *Server) cleanupFinishedBareLeases() {
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	now := time.Now()
	for _, lease := range state.Leases {
		if !lease.Active || lease.Mode != model.ModeBare || lease.RootPID <= 0 {
			continue
		}
		if s.Proc != nil && s.Proc.Exists(lease.RootPID) {
			continue
		}
		if lease.CgroupPath != "" && !cgroupEmpty(lease.CgroupPath) {
			continue
		}
		_ = s.Store.ReleaseLease(lease.ID)
		if lease.CgroupPath != "" {
			_ = os.Remove(lease.CgroupPath)
		}
		_ = s.Store.AppendAudit(model.AuditEvent{
			Time:    now.UTC(),
			Kind:    "lease_released",
			Message: "bare lease released after process exit",
			GPU:     lease.GPU,
			LeaseID: lease.ID,
			User:    lease.Holder,
		})
	}
}

func (s *Server) cleanupExpiredLeases() {
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	now := time.Now()
	for _, lease := range state.Leases {
		if !lease.Active || now.Before(lease.ExpiresAt) {
			continue
		}
		if lease.RootPID > 0 {
			_ = syscall.Kill(-lease.RootPID, syscall.SIGTERM)
			time.Sleep(500 * time.Millisecond)
			_ = syscall.Kill(-lease.RootPID, syscall.SIGKILL)
		}
		_ = s.Store.ReleaseLease(lease.ID)
		if lease.CgroupPath != "" {
			_ = os.Remove(lease.CgroupPath)
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: "lease_expired", Message: "lease expired", GPU: lease.GPU, LeaseID: lease.ID})
	}
}

func (s *Server) authorizer() enforce.Authorizer {
	return enforce.Authorizer{
		Proc:    s.Proc,
		Runtime: s.Runtime,
		Killer:  s.Killer,
		DryRun:  s.Cfg.DryRun,
		OnAudit: func(event model.AuditEvent) {
			_ = s.Store.AppendAudit(event)
		},
	}
}

func (s *Server) createCgroup(leaseID string) (string, string, error) {
	path := filepath.Join(s.Cfg.CgroupRoot, leaseID)
	if err := os.MkdirAll(path, 0755); err != nil {
		return "", "", err
	}
	rel := strings.TrimPrefix(path, "/sys/fs/cgroup/")
	if rel == path {
		rel = filepath.Join(filepath.Base(s.Cfg.CgroupRoot), leaseID)
	}
	return path, filepath.ToSlash(rel), nil
}

func (s *Server) movePIDToCgroup(cgroupPath string, pid int) error {
	return os.WriteFile(filepath.Join(cgroupPath, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644)
}

func cgroupEmpty(cgroupPath string) bool {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs"))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == ""
}

func streamCopy(wg *sync.WaitGroup, mu *sync.Mutex, writer io.Writer, reqID, kind string, reader io.Reader) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			mu.Lock()
			writeResult(writer, protocol.Response{ID: reqID, Kind: kind, OK: true, Data: string(buf[:n])})
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func writeResult(writer io.Writer, resp protocol.Response) {
	data, _ := json.Marshal(resp)
	_, _ = writer.Write(append(data, '\n'))
}

func commandEnv(env []string) []string {
	if len(env) == 0 {
		return os.Environ()
	}
	var out []string
	for _, item := range env {
		if strings.HasPrefix(item, "KEY=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func peerCred(conn net.Conn, procRoot string) (peer, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return peer{}, errors.New("not a unix connection")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return peer{}, err
	}
	var cred *syscall.Ucred
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		cred, controlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return peer{}, err
	}
	if controlErr != nil {
		return peer{}, controlErr
	}
	groups := peerGroups(procRoot, int(cred.Pid), int(cred.Gid))
	return peer{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid), Groups: groups}, nil
}

func peerGroups(procRoot string, pid, primaryGID int) []uint32 {
	groups := []uint32{uint32(primaryGID)}
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return groups
	}
	seen := map[uint32]bool{uint32(primaryGID): true}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Groups:") {
			continue
		}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "Groups:")) {
			gid, err := strconv.ParseUint(field, 10, 32)
			if err != nil {
				continue
			}
			value := uint32(gid)
			if seen[value] {
				continue
			}
			seen[value] = true
			groups = append(groups, value)
		}
		return groups
	}
	return groups
}
