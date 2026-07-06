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
	"path/filepath"
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
		secret, token, err := s.Store.RegisterToken(args.RootKey, args.Name, args.TTL, now)
		if err != nil {
			return nil, err
		}
		return model.RegisterResult{Token: secret, ExpiresAt: token.ExpiresAt}, nil
	case "docker_allow":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.DockerAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createDockerLease(ctx, token, tokenHash, p, args)
	case "k8s_allow":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.K8sAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createK8sLease(ctx, token, tokenHash, p, args)
	case "status":
		return s.Store.Status(now)
	case "ps":
		status, err := s.Store.Status(now)
		if err != nil {
			return nil, err
		}
		return status.Leases, nil
	case "who":
		var args protocol.WhoArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		status, err := s.Store.Status(now)
		if err != nil {
			return nil, err
		}
		var leases []model.Lease
		for _, lease := range status.Leases {
			if lease.GPU == args.GPU {
				leases = append(leases, lease)
			}
		}
		return leases, nil
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
	case "bypass_add":
		if p.UID != 0 {
			return nil, errors.New("admin command requires uid 0")
		}
		var args protocol.BypassAddArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.addBypass(args, now)
	case "revoke":
		if p.UID != 0 {
			return nil, errors.New("admin command requires uid 0")
		}
		var args protocol.RevokeArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
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

func (s *Server) createDockerLease(ctx context.Context, token model.Token, tokenHash string, p peer, args protocol.DockerAllowArgs) (model.AllowResult, error) {
	if args.GPU < 0 {
		return model.AllowResult{}, errors.New("gpu must be >= 0")
	}
	containerID, err := s.Runtime.ResolveDockerContainer(ctx, args.Container)
	if err != nil {
		return model.AllowResult{}, fmt.Errorf("resolve docker container: %w", err)
	}
	now := time.Now()
	lease := model.Lease{
		ID:          store.NewLeaseID(),
		GPU:         args.GPU,
		Mode:        model.ModeDocker,
		TokenHash:   tokenHash,
		Holder:      token.Name,
		UID:         p.UID,
		GID:         p.GID,
		ContainerID: containerID,
		CreatedAt:   now.UTC(),
		ExpiresAt:   token.ExpiresAt,
		Active:      true,
	}
	if err := s.ensureLeaseCanStart(ctx, lease); err != nil {
		return model.AllowResult{}, err
	}
	if err := s.Store.AddLease(lease); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{LeaseID: lease.ID, Mode: lease.Mode, GPU: lease.GPU, ContainerID: containerID, ExpiresAt: lease.ExpiresAt}, nil
}

func (s *Server) createK8sLease(ctx context.Context, token model.Token, tokenHash string, p peer, args protocol.K8sAllowArgs) (model.AllowResult, error) {
	if args.GPU < 0 {
		return model.AllowResult{}, errors.New("gpu must be >= 0")
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
	lease := model.Lease{
		ID:        store.NewLeaseID(),
		GPU:       args.GPU,
		Mode:      model.ModeK8s,
		TokenHash: tokenHash,
		Holder:    token.Name,
		UID:       p.UID,
		GID:       p.GID,
		Namespace: strings.TrimSpace(args.Namespace),
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := s.ensureLeaseCanStart(ctx, lease); err != nil {
		return model.AllowResult{}, err
	}
	if err := s.Store.AddLease(lease); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{LeaseID: lease.ID, Mode: lease.Mode, GPU: lease.GPU, Namespace: lease.Namespace, ExpiresAt: lease.ExpiresAt}, nil
}

func (s *Server) runCommand(ctx context.Context, conn net.Conn, reqID string, token model.Token, tokenHash string, p peer, args protocol.RunArgs) (model.RunResult, error) {
	if args.GPU < 0 {
		return model.RunResult{}, errors.New("gpu must be >= 0")
	}
	if len(args.Command) == 0 {
		return model.RunResult{}, errors.New("command is required")
	}
	now := time.Now()
	lease := model.Lease{
		ID:        store.NewLeaseID(),
		GPU:       args.GPU,
		Mode:      model.ModeBare,
		TokenHash: tokenHash,
		Holder:    token.Name,
		UID:       p.UID,
		GID:       p.GID,
		Command:   args.Command,
		CreatedAt: now.UTC(),
		ExpiresAt: token.ExpiresAt,
		Active:    true,
	}
	if err := s.ensureLeaseCanStart(ctx, lease); err != nil {
		return model.RunResult{}, err
	}
	cgroupPath, cgroupRel, err := s.createCgroup(lease.ID)
	if err != nil {
		return model.RunResult{}, err
	}
	lease.CgroupPath = cgroupPath
	lease.CgroupRel = cgroupRel
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
	lease.RootPID = cmd.Process.Pid
	if err := s.movePIDToCgroup(cgroupPath, lease.RootPID); err != nil {
		_ = cmd.Process.Kill()
		return model.RunResult{}, err
	}
	if err := s.Store.AddLease(lease); err != nil {
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
	_ = s.Store.ReleaseLease(lease.ID)
	_ = os.Remove(cgroupPath)
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr != nil && exitCode == 0 {
		return model.RunResult{LeaseID: lease.ID, ExitCode: exitCode}, waitErr
	}
	return model.RunResult{LeaseID: lease.ID, ExitCode: exitCode}, nil
}

func (s *Server) ensureLeaseCanStart(ctx context.Context, tentative model.Lease) error {
	status, err := s.Store.Status(time.Now())
	if err != nil {
		return err
	}
	for _, lease := range status.Leases {
		if lease.GPU == tentative.GPU {
			return fmt.Errorf("gpu %d already held by lease %s", tentative.GPU, lease.ID)
		}
	}
	state, err := s.Store.Snapshot()
	if err != nil {
		return err
	}
	processes, err := s.AMD.Processes(ctx)
	if err != nil {
		return fmt.Errorf("amd-smi process list: %w", err)
	}
	auth := s.authorizer()
	busy, err := auth.BusyProcessesForLease(ctx, state, processes, &tentative)
	if err != nil {
		return err
	}
	if len(busy) > 0 {
		first := busy[0]
		return fmt.Errorf("gpu %d is busy: pid=%d cmd=%s", tentative.GPU, first.Process.PID, strings.Join(first.Info.Cmdline, " "))
	}
	return nil
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
	s.cleanupExpiredLeases()
	state, err := s.Store.Snapshot()
	if err != nil {
		return
	}
	processes, err := s.AMD.Processes(ctx)
	if err != nil {
		_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "error", Message: "amd-smi process list failed: " + err.Error()})
		return
	}
	_, _ = s.authorizer().Enforce(ctx, state, processes)
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
