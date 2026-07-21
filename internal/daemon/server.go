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
	"unicode"
	"unicode/utf8"

	"gpuardian/internal/amdsmi"
	"gpuardian/internal/config"
	"gpuardian/internal/enforce"
	"gpuardian/internal/gpusmi"
	"gpuardian/internal/model"
	"gpuardian/internal/nvidiasmi"
	"gpuardian/internal/proc"
	"gpuardian/internal/protocol"
	"gpuardian/internal/runtime"
	"gpuardian/internal/store"
	"gpuardian/internal/telemetry"
)

type Server struct {
	Cfg                  config.Config
	Store                *store.Store
	GPU                  gpusmi.Provider
	Proc                 proc.Reader
	Runtime              runtime.Resolver
	Killer               enforce.Killer
	Interval             time.Duration
	enforceMu            sync.Mutex
	auditMu              sync.Mutex
	lastGPUError         string
	lastGPUErrorAt       time.Time
	lastKillError        string
	lastKillErrorAt      time.Time
	evictionClean        map[string]int
	bootID               string
	resolvePeer          func(net.Conn, string) (peer, error)
	connectionMu         sync.Mutex
	uidConnections       map[int]int
	connections          int
	nonRootConnections   int
	processReadMu        sync.Mutex
	processReadAt        time.Time
	processReadRows      []model.GPUProcess
	processReadErr       error
	processReadSlotsOnce sync.Once
	processReadSlots     chan struct{}
	metricsReadMu        sync.Mutex
	metricsReadAt        time.Time
	metricsReadRows      []model.GPUMetric
	metricsReadErr       error
	devicesReadMu        sync.Mutex
	devicesReadAt        time.Time
	devicesReadRows      map[int]gpusmi.DeviceInfo
	devicesReadErr       error
	Telemetry            *telemetry.Outbox
	telemetryWriteMu     sync.Mutex
	telemetryGapFrom     time.Time
	telemetryJobsMu      sync.Mutex
	observedJobs         map[string]*observedTelemetryJob
	runJobs              map[string]telemetry.JobEvent
	dockerResolveOnce    sync.Once
	dockerResolveSlots   chan struct{}
	nodeHTTPOnce         sync.Once
	nodeHTTPSlots        chan struct{}
	// allowUnsafeCgroupFallback exists only so tests can run without a cgroup v2
	// mount. Production servers leave it false and fail closed if a process
	// cannot be born directly into its managed cgroup.
	allowUnsafeCgroupFallback bool
}

const (
	openPath                    = 0x200000
	cgroup2SuperMagic           = 0x63677270
	maxRPCFrameBytes            = 1 << 20
	maxRPCConnections           = 256
	maxRPCConnectionsUID        = 32
	reservedRootRPCConnections  = 16
	maxConcurrentProcessReads   = 4
	maxConcurrentDockerResolves = 4
	maxConcurrentNodeHTTP       = 32
	maxPSRows                   = 4096
	maxPSRetainedTextBytes      = 512 << 10
	maxPSFieldBytes             = 4096
	enforcementPassTimeout      = 5 * time.Second
	rpcIdleTimeout              = 30 * time.Second
	rpcWriteTimeout             = 10 * time.Second
	maxRPCIDBytes               = 128
	maxRPCMethodBytes           = 64
	maxRPCTokenBytes            = 256
	maxGPUIndex                 = 1023
	maxGPUsPerRequest           = 64
	maxRequestValue             = 4096
	evictionCleanSamples        = 3
)

type peer struct {
	PID    int
	UID    int
	GID    int
	Groups []uint32
}

func New(cfg config.Config) *Server {
	st := store.New(cfg)
	procReader := proc.NewFSReader(cfg.ProcRoot)
	bootID, _ := readBootID(cfg.ProcRoot)
	return &Server{
		Cfg:            cfg,
		Store:          st,
		GPU:            selectGPUProvider(cfg),
		Proc:           procReader,
		Runtime:        runtime.NewCachedResolver(runtime.CLIResolver{}),
		Killer:         enforce.RealKiller{Grace: 2 * time.Second, Proc: procReader},
		Interval:       time.Second,
		evictionClean:  make(map[string]int),
		bootID:         bootID,
		uidConnections: make(map[int]int),
		observedJobs:   make(map[string]*observedTelemetryJob),
		runJobs:        make(map[string]telemetry.JobEvent),
		resolvePeer:    peerCred,
	}
}

// selectGPUProvider picks the GPU SMI provider based on GPUARDIAN_GPU_VENDOR.
// "auto" probes for amd-smi first (preserving the historical default for
// existing AMD nodes that never set the env var) and falls back to
// nvidia-smi. Explicit "amd" or "nvidia" skip probing and use that provider
// directly; an unrecognized value is treated as "auto".
func selectGPUProvider(cfg config.Config) gpusmi.Provider {
	switch strings.ToLower(strings.TrimSpace(cfg.GPUVendor)) {
	case "amd":
		return amdsmi.NewCLIProvider()
	case "nvidia":
		return nvidiasmi.NewCLIProvider()
	default:
		if _, err := exec.LookPath("amd-smi"); err == nil {
			return amdsmi.NewCLIProvider()
		}
		if _, err := exec.LookPath("nvidia-smi"); err == nil {
			return nvidiasmi.NewCLIProvider()
		}
		// Neither tool is on PATH; default to the AMD provider so the daemon
		// still starts and surfaces a clear "amd-smi process list failed"
		// audit error rather than silently doing nothing.
		return amdsmi.NewCLIProvider()
	}
}

func (s *Server) Run(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("gpuardian daemon must run as root")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := s.Store.Load(); err != nil {
		return err
	}
	s.initializeTelemetry()
	if s.Telemetry != nil {
		defer s.Telemetry.Close()
	}
	if err := os.MkdirAll(filepath.Dir(s.Cfg.SocketPath), 0755); err != nil {
		return err
	}
	lockFile, err := acquireUnixSocketLock(s.Cfg.SocketPath + ".lock")
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := prepareUnixSocket(s.Cfg.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.Cfg.SocketPath)
	if err != nil {
		return err
	}
	boundSocket, err := os.Lstat(s.Cfg.SocketPath)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer func() {
		_ = removeOwnedUnixSocket(s.Cfg.SocketPath, boundSocket)
		_ = listener.Close()
	}()
	if err := os.Chmod(s.Cfg.SocketPath, 0666); err != nil {
		return err
	}
	if s.Cfg.NodeAddr != "" {
		closeNodeHTTP, err := s.startNodeHTTP(runCtx)
		if err != nil {
			return err
		}
		defer closeNodeHTTP()
	}

	var handlers sync.WaitGroup
	var connections sync.Map
	handlers.Add(1)
	go func() {
		defer handlers.Done()
		s.monitor(runCtx)
	}()
	if s.Telemetry != nil {
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			s.metricMonitor(runCtx)
		}()
	}
	go func() {
		<-runCtx.Done()
		_ = listener.Close()
		connections.Range(func(key, _ any) bool {
			_ = key.(net.Conn).Close()
			return true
		})
	}()

	var runErr error
	for {
		conn, err := listener.Accept()
		if err != nil {
			if runCtx.Err() == nil {
				runErr = err
			}
			cancel()
			break
		}
		resolvePeer := s.resolvePeer
		if resolvePeer == nil {
			resolvePeer = peerCred
		}
		p, err := resolvePeer(conn, s.Cfg.ProcRoot)
		if err != nil || p.UID < 0 || p.GID < 0 || !s.acquireConnection(p.UID) {
			_ = conn.Close()
			continue
		}
		if runCtx.Err() != nil {
			s.releaseConnection(p.UID)
			_ = conn.Close()
			continue
		}
		connections.Store(conn, struct{}{})
		if runCtx.Err() != nil {
			connections.Delete(conn)
			s.releaseConnection(p.UID)
			_ = conn.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() {
				connections.Delete(conn)
				s.releaseConnection(p.UID)
			}()
			s.serveConn(runCtx, conn, p)
		}()
	}
	handlers.Wait()
	return runErr
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	resolvePeer := s.resolvePeer
	if resolvePeer == nil {
		resolvePeer = peerCred
	}
	p, err := resolvePeer(conn, s.Cfg.ProcRoot)
	if err != nil || p.UID < 0 || p.GID < 0 {
		_ = conn.Close()
		return
	}
	if !s.acquireConnection(p.UID) {
		_ = conn.Close()
		return
	}
	defer s.releaseConnection(p.UID)
	s.serveConn(ctx, conn, p)
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn, p peer) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64<<10), maxRPCFrameBytes)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(rpcIdleTimeout)); err != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		var req protocol.Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if writeResult(conn, protocol.Response{Kind: protocol.KindResult, OK: false, Error: err.Error()}) != nil {
				return
			}
			continue
		}
		if len(req.ID) > maxRPCIDBytes || len(req.Method) > maxRPCMethodBytes || len(req.Token) > maxRPCTokenBytes {
			if writeResult(conn, protocol.Response{Kind: protocol.KindResult, OK: false, Error: "RPC request field exceeds size limit"}) != nil {
				return
			}
			continue
		}
		if req.Method == "run" {
			_ = conn.SetReadDeadline(time.Time{})
			if len(p.Groups) == 0 {
				groups, err := peerProcessGroups(s.Cfg.ProcRoot, p.PID, p.UID, p.GID)
				if err != nil {
					_ = writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
					return
				}
				p.Groups = groups
			}
			s.handleRun(ctx, conn, p, req)
			return
		}
		result, err := s.dispatch(ctx, p, req)
		if err != nil {
			if writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()}) != nil {
				return
			}
			continue
		}
		data, _ := json.Marshal(result)
		if writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: true, Result: data}) != nil {
			return
		}
	}
}

func (s *Server) acquireConnection(uid int) bool {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.uidConnections == nil {
		s.uidConnections = make(map[int]int)
	}
	if s.connections >= maxRPCConnections {
		return false
	}
	if uid != 0 && (s.nonRootConnections >= maxRPCConnections-reservedRootRPCConnections || s.uidConnections[uid] >= maxRPCConnectionsUID) {
		return false
	}
	s.uidConnections[uid]++
	s.connections++
	if uid != 0 {
		s.nonRootConnections++
	}
	return true
}

func (s *Server) releaseConnection(uid int) {
	s.connectionMu.Lock()
	defer s.connectionMu.Unlock()
	if s.uidConnections[uid] <= 0 {
		return
	}
	if s.uidConnections[uid] <= 1 {
		delete(s.uidConnections, uid)
	} else {
		s.uidConnections[uid]--
	}
	s.connections--
	if uid != 0 {
		s.nonRootConnections--
	}
}

func (s *Server) dispatch(ctx context.Context, p peer, req protocol.Request) (any, error) {
	now := time.Now()
	switch req.Method {
	case "register":
		args, err := decodeRegisterArgs(req.Args)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Mode) == "" {
			return nil, errors.New("mode must be reserved or claimed")
		}
		if err := validateRequestValue("name", args.Name); err != nil {
			return nil, err
		}
		if err := validateRequestValue("purpose", args.Purpose); err != nil {
			return nil, err
		}
		if err := validateRequestValue("external session id", args.ExternalSessionID); err != nil {
			return nil, err
		}
		if ok, err := s.Store.ValidateRootKey(args.RootKey); err != nil {
			return nil, err
		} else if !ok {
			return nil, store.ErrInvalidRootKey
		}
		switch store.NormalizeTokenMode(args.Mode) {
		case model.TokenModeReserved:
			if err := s.validateConfiguredGPUs(args.GPUs); err != nil {
				return nil, err
			}
			startsAt := now
			var expiresAt time.Time
			if args.StartsAt != nil {
				startsAt = args.StartsAt.UTC()
			}
			if args.ExpiresAt != nil {
				expiresAt = args.ExpiresAt.UTC()
			}
			if expiresAt.IsZero() {
				ttl, err := store.ParseTTL(args.TTL, store.DefaultHardTTL, store.MaxHardTTL)
				if err != nil {
					return nil, err
				}
				expiresAt = startsAt.Add(ttl)
			}
			if !expiresAt.After(now) {
				return nil, errors.New("reservation end must be in the future")
			}
			s.enforceMu.Lock()
			defer s.enforceMu.Unlock()
			if err := s.ensureGPUsCanReserveWindow(ctx, args.GPUs, startsAt, expiresAt); err != nil {
				return nil, err
			}
			var secret, groupID string
			var token model.Token
			var reservations []model.Reservation
			if strings.TrimSpace(args.UserKeyID) != "" {
				token, groupID, reservations, err = s.Store.RegisterManagedReservations(args.RootKey, args.UserKeyID, args.Purpose, args.ExternalSessionID, args.GPUs, startsAt, expiresAt, now)
			} else {
				managed, managedErr := s.Store.ManagedKeysEnabled()
				if managedErr != nil {
					return nil, managedErr
				}
				if managed {
					return nil, errors.New("user_key_id is required; keys are managed by the web gateway")
				}
				secret, token, reservations, err = s.Store.RegisterScheduledReservationsWithSession(args.RootKey, args.Name, args.Purpose, args.ExternalSessionID, args.GPUs, startsAt, expiresAt, now)
			}
			if err != nil {
				return nil, err
			}
			s.emitReservation(token, reservations)
			ids := make([]string, 0, len(reservations))
			gpus := make([]int, 0, len(reservations))
			for _, reservation := range reservations {
				ids = append(ids, reservation.ID)
				gpus = append(gpus, reservation.GPU)
			}
			if groupID == "" {
				groupID = token.ID
			}
			return model.RegisterResult{Token: secret, TokenID: token.ID, GroupID: groupID, Mode: token.Mode, ReservationIDs: ids, GPUs: gpus, StartsAt: timePtrIfSet(startsAt), ExpiresAt: timePtrIfSet(expiresAt)}, nil
		case model.TokenModeClaimed:
			if managed, managedErr := s.Store.ManagedKeysEnabled(); managedErr != nil {
				return nil, managedErr
			} else if managed {
				return nil, errors.New("claim keys are managed by the web gateway")
			}
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
		return s.createDockerAuthorization(ctx, req.Token, token, tokenHash, p, args)
	case "allow_k8s":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.K8sAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createK8sAuthorization(ctx, req.Token, token, tokenHash, p, args)
	case "allow_user":
		token, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		var args protocol.UserAllowArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		return s.createUserAuthorization(req.Token, token, tokenHash, p, args)
	case "status":
		if p.UID == 0 {
			return s.Store.Status(now)
		}
		_, tokenHash, err := s.validateToken(req.Token, now)
		if err != nil {
			return nil, err
		}
		return s.Store.StatusForToken(tokenHash, now)
	case "ps":
		tokenHash, err := s.readScopeTokenHash(req.Token, p, now)
		if err != nil {
			return nil, err
		}
		return s.ps(ctx, now, tokenHash, p.UID == 0)
	case "who":
		var args protocol.WhoArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return nil, err
		}
		tokenHash, err := s.readScopeTokenHash(req.Token, p, now)
		if err != nil {
			return nil, err
		}
		rows, err := s.ps(ctx, now, tokenHash, p.UID == 0)
		if err != nil {
			return nil, err
		}
		var out []model.PSRow
		for _, row := range rows {
			if gpuListContains(row.GPU, args.GPU) {
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
		return map[string]string{"revoked": args.ID}, s.revoke(args.ID)
	default:
		return nil, fmt.Errorf("unknown method %q", req.Method)
	}
}

func (s *Server) readScopeTokenHash(secret string, p peer, now time.Time) (string, error) {
	if p.UID == 0 && strings.TrimSpace(secret) == "" {
		return "", nil
	}
	_, tokenHash, err := s.validateToken(secret, now)
	return tokenHash, err
}

func (s *Server) handleRun(ctx context.Context, conn net.Conn, p peer, req protocol.Request) {
	now := time.Now()
	token, tokenHash, err := s.validateToken(req.Token, now)
	if err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	args, err := decodeRunArgs(req.Args)
	if err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go cancelOnConnClose(runCtx, conn, cancel)
	result, err := s.runCommand(runCtx, conn, req.ID, req.Token, token, tokenHash, p, args)
	if err != nil {
		writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: false, Error: err.Error()})
		return
	}
	data, _ := json.Marshal(result)
	writeResult(conn, protocol.Response{ID: req.ID, Kind: protocol.KindResult, OK: true, Result: data})
}

func cancelOnConnClose(ctx context.Context, conn net.Conn, cancel context.CancelFunc) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = conn.SetReadDeadline(time.Now())
	}()
	var one [1]byte
	_, _ = conn.Read(one[:])
	close(done)
	if ctx.Err() == nil {
		cancel()
	}
}

func (s *Server) validateToken(secret string, now time.Time) (model.Token, string, error) {
	if strings.TrimSpace(secret) == "" {
		return model.Token{}, "", errors.New("KEY token is required")
	}
	return s.Store.ValidateToken(secret, now)
}

func (s *Server) createDockerAuthorization(ctx context.Context, tokenSecret string, token model.Token, tokenHash string, p peer, args protocol.DockerAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	container := strings.TrimSpace(args.Container)
	if container == "" {
		return model.AllowResult{}, errors.New("container is required")
	}
	if err := validateRequestValue("container", container); err != nil {
		return model.AllowResult{}, err
	}
	if hasWildcard(container) && p.UID != 0 {
		return model.AllowResult{}, errors.New("wildcard authorization requires root/admin access")
	}
	var containerID string
	var containerPattern string
	if hasWildcard(container) {
		containerPattern = container
	} else {
		release, err := s.acquireDockerResolve(ctx)
		if err != nil {
			return model.AllowResult{}, err
		}
		defer release()
		resolvedID, err := s.Runtime.ResolveDockerContainer(ctx, container)
		if err != nil {
			return model.AllowResult{}, fmt.Errorf("resolve docker container: %w", err)
		}
		containerID = resolvedID
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:               store.NewAuthorizationID(),
		Mode:             model.ModeDocker,
		TokenHash:        tokenHash,
		TokenMode:        store.NormalizeTokenMode(token.Mode),
		TokenVersion:     token.Version,
		Holder:           token.Name,
		UID:              p.UID,
		GID:              p.GID,
		ContainerID:      containerID,
		ContainerPattern: containerPattern,
		CreatedAt:        now.UTC(),
		ExpiresAt:        token.ExpiresAt,
		Active:           true,
	}
	if err := s.persistAuthorization(tokenSecret, token, &authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, ContainerID: containerID, ContainerPattern: containerPattern, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func (s *Server) acquireDockerResolve(ctx context.Context) (func(), error) {
	s.dockerResolveOnce.Do(func() {
		s.dockerResolveSlots = make(chan struct{}, maxConcurrentDockerResolves)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	select {
	case s.dockerResolveSlots <- struct{}{}:
		return func() { <-s.dockerResolveSlots }, nil
	default:
		return nil, errors.New("docker resolver is busy; retry shortly")
	}
}

func (s *Server) createK8sAuthorization(ctx context.Context, tokenSecret string, token model.Token, tokenHash string, p peer, args protocol.K8sAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	if strings.TrimSpace(args.Namespace) == "" {
		return model.AllowResult{}, errors.New("namespace is required")
	}
	if err := validateRequestValue("namespace", args.Namespace); err != nil {
		return model.AllowResult{}, err
	}
	if hasWildcard(args.Namespace) && p.UID != 0 {
		return model.AllowResult{}, errors.New("wildcard authorization requires root/admin access")
	}
	if _, err := exec.LookPath("crictl"); err != nil {
		if _, err2 := exec.LookPath("kubectl"); err2 != nil {
			return model.AllowResult{}, errors.New("k8s mode requires crictl or kubectl")
		}
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:           store.NewAuthorizationID(),
		Mode:         model.ModeK8s,
		TokenHash:    tokenHash,
		TokenMode:    store.NormalizeTokenMode(token.Mode),
		TokenVersion: token.Version,
		Holder:       token.Name,
		UID:          p.UID,
		GID:          p.GID,
		Namespace:    strings.TrimSpace(args.Namespace),
		CreatedAt:    now.UTC(),
		ExpiresAt:    token.ExpiresAt,
		Active:       true,
	}
	if err := s.persistAuthorization(tokenSecret, token, &authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, Namespace: authorization.Namespace, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func (s *Server) createUserAuthorization(tokenSecret string, token model.Token, tokenHash string, p peer, args protocol.UserAllowArgs) (model.AllowResult, error) {
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.AllowResult{}, err
	}
	username := strings.TrimSpace(args.User)
	if username == "" {
		return model.AllowResult{}, errors.New("user is required")
	}
	if err := validateRequestValue("user", username); err != nil {
		return model.AllowResult{}, err
	}
	if hasWildcard(username) && p.UID != 0 {
		return model.AllowResult{}, errors.New("wildcard authorization requires root/admin access")
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
		ID:           store.NewAuthorizationID(),
		Mode:         model.ModeUser,
		TokenHash:    tokenHash,
		TokenMode:    store.NormalizeTokenMode(token.Mode),
		TokenVersion: token.Version,
		Holder:       token.Name,
		UID:          uid,
		GID:          p.GID,
		Username:     username,
		CreatedAt:    now.UTC(),
		ExpiresAt:    token.ExpiresAt,
		Active:       true,
	}
	if err := s.persistAuthorization(tokenSecret, token, &authorization); err != nil {
		return model.AllowResult{}, err
	}
	return model.AllowResult{AuthorizationID: authorization.ID, Mode: authorization.Mode, Username: authorization.Username, ExpiresAt: timePtrIfSet(authorization.ExpiresAt)}, nil
}

func hasWildcard(value string) bool {
	return strings.Contains(value, "*")
}

func validateRequestValue(label, value string) error {
	if len(value) > maxRequestValue {
		return fmt.Errorf("%s must be at most %d bytes", label, maxRequestValue)
	}
	return nil
}

func (s *Server) addAuthorization(tokenSecret string, authorization *model.Authorization) error {
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	return s.addAuthorizationLocked(tokenSecret, authorization)
}

func (s *Server) persistAuthorization(tokenSecret string, token model.Token, authorization *model.Authorization) error {
	if strings.TrimSpace(tokenSecret) != "" || !token.Managed {
		return s.addAuthorization(tokenSecret, authorization)
	}
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	current, err := s.Store.ManagedTokenByID(token.ID, time.Now())
	if err != nil {
		return err
	}
	if current.Hash != token.Hash || current.Version != token.Version {
		return errors.New("managed key identity changed during authorization")
	}
	authorization.TokenHash = current.Hash
	authorization.TokenMode = model.TokenModeManaged
	authorization.TokenVersion = current.Version
	authorization.Holder = current.Name
	authorization.ExpiresAt = time.Time{}
	if err := s.Store.AddAuthorization(*authorization); err != nil {
		return err
	}
	s.emitAuthorization(current.ID, *authorization)
	return nil
}

func (s *Server) addAuthorizationLocked(tokenSecret string, authorization *model.Authorization) error {
	now := time.Now()
	token, tokenHash, err := s.validateToken(tokenSecret, now)
	if err != nil {
		return err
	}
	if tokenHash != authorization.TokenHash {
		return errors.New("token identity changed during authorization")
	}
	if err := s.ensureTokenCanAuthorize(tokenHash, token, now); err != nil {
		return err
	}
	authorization.TokenMode = store.NormalizeTokenMode(token.Mode)
	authorization.TokenVersion = token.Version
	authorization.Holder = token.Name
	authorization.ExpiresAt = token.ExpiresAt
	if err := s.Store.AddAuthorization(*authorization); err != nil {
		return err
	}
	s.emitAuthorization(token.ID, *authorization)
	return nil
}

func prepareRunCommand(ctx context.Context, args protocol.RunArgs, p peer, useCgroupFD bool, cgroupFD int) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, args.Command[0], args.Command[1:]...)
	cmd.Dir = args.Workdir
	cmd.Env = commandEnv(args.Env)
	cmd.Cancel = func() error {
		return terminateProcessGroup(cmd.Process)
	}
	cmd.WaitDelay = 3 * time.Second
	sys := &syscall.SysProcAttr{Setpgid: true}
	if useCgroupFD {
		sys.UseCgroupFD = true
		sys.CgroupFD = cgroupFD
	}
	if os.Geteuid() == 0 && p.UID >= 0 && p.GID >= 0 {
		sys.Credential = &syscall.Credential{Uid: uint32(p.UID), Gid: uint32(p.GID), Groups: p.Groups}
	}
	cmd.SysProcAttr = sys
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	return cmd, stdout, stderr, nil
}

func terminateProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	pid := process.Pid
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		_ = process.Kill()
		return err
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for processGroupAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if processGroupAlive(pid) {
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			_ = process.Kill()
			return err
		}
	}
	return nil
}

func processGroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}

func terminateStartedCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = terminateProcessGroup(cmd.Process)
	_ = cmd.Wait()
}

func (s *Server) runCommand(ctx context.Context, conn net.Conn, reqID, tokenSecret string, token model.Token, tokenHash string, p peer, args protocol.RunArgs) (model.RunResult, error) {
	if p.UID < 0 || p.GID < 0 {
		return model.RunResult{}, errors.New("verified peer credentials are required")
	}
	if err := s.ensureTokenCanAuthorize(tokenHash, token, time.Now()); err != nil {
		return model.RunResult{}, err
	}
	if err := validateRunArgs(args); err != nil {
		return model.RunResult{}, err
	}
	now := time.Now()
	authorization := model.Authorization{
		ID:           store.NewAuthorizationID(),
		Mode:         model.ModeBare,
		TokenHash:    tokenHash,
		TokenMode:    store.NormalizeTokenMode(token.Mode),
		TokenVersion: token.Version,
		Holder:       token.Name,
		UID:          p.UID,
		GID:          p.GID,
		Command:      args.Command,
		CreatedAt:    now.UTC(),
		ExpiresAt:    token.ExpiresAt,
		Active:       true,
	}
	cgroupPath, cgroupRel, err := s.createCgroup(authorization.ID)
	if err != nil {
		return model.RunResult{}, err
	}
	cleanupCgroup := true
	authorizationActive := false
	keepAuthorization := false
	defer func() {
		if authorizationActive && !keepAuthorization {
			_ = s.releaseAuthorization(authorization.ID)
		}
		if cleanupCgroup && !keepAuthorization {
			_ = removeCgroup(cgroupPath)
		}
	}()
	authorization.CgroupPath = cgroupPath
	authorization.CgroupRel = cgroupRel
	cgroupFD, useCgroupFD, err := openCgroupFD(cgroupPath)
	if err != nil {
		return model.RunResult{}, err
	}
	closeCgroupFD := func() {
		if cgroupFD >= 0 {
			_ = syscall.Close(cgroupFD)
			cgroupFD = -1
		}
	}
	defer closeCgroupFD()
	if !useCgroupFD && !s.allowUnsafeCgroupFallback {
		return model.RunResult{}, errors.New("atomic cgroup placement is unavailable; refusing to start command")
	}

	cmd, stdout, stderr, err := prepareRunCommand(ctx, args, p, useCgroupFD, cgroupFD)
	if err != nil {
		return model.RunResult{}, err
	}
	if err := s.addAuthorization(tokenSecret, &authorization); err != nil {
		return model.RunResult{}, err
	}
	authorizationActive = true
	startedWithCgroupFD := useCgroupFD
	if err := cmd.Start(); err != nil {
		if !useCgroupFD || !s.allowUnsafeCgroupFallback {
			return model.RunResult{}, err
		}
		closeCgroupFD()
		cmd, stdout, stderr, err = prepareRunCommand(ctx, args, p, false, -1)
		if err != nil {
			return model.RunResult{}, err
		}
		startedWithCgroupFD = false
		if err := cmd.Start(); err != nil {
			return model.RunResult{}, err
		}
	}
	if startedWithCgroupFD {
		closeCgroupFD()
	}
	authorization.RootPID = cmd.Process.Pid
	if !startedWithCgroupFD {
		if err := s.movePIDToCgroup(cgroupPath, authorization.RootPID); err != nil {
			terminateStartedCommand(cmd)
			return model.RunResult{}, err
		}
	}
	activateErr := func() error {
		s.enforceMu.Lock()
		defer s.enforceMu.Unlock()
		now := time.Now()
		currentToken, currentHash, err := s.validateToken(tokenSecret, now)
		if err != nil {
			return err
		}
		if currentHash != authorization.TokenHash {
			return errors.New("token identity changed while command was starting")
		}
		if err := s.ensureTokenCanAuthorize(currentHash, currentToken, now); err != nil {
			return err
		}
		activated, err := s.Store.ActivateAuthorization(authorization.ID, authorization.RootPID)
		if err == nil {
			authorization = activated
		}
		return err
	}()
	if activateErr != nil {
		terminateStartedCommand(cmd)
		if err := s.stopAndRemoveManagedCgroup(cgroupPath); err != nil {
			// Preserve the active authorization and its cgroup path so the monitor
			// can retry cgroup.kill rather than losing track of a descendant that
			// escaped the command's process group.
			keepAuthorization = true
		}
		return model.RunResult{}, activateErr
	}
	s.rememberRunJob(token, authorization, authorization.RootPID, args.Command, time.Now())
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go streamCopy(&wg, &writeMu, conn, reqID, protocol.KindStdout, stdout)
	go streamCopy(&wg, &writeMu, conn, reqID, protocol.KindStderr, stderr)
	waitErr := cmd.Wait()
	wg.Wait()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	s.updateRunJobRootExit(authorization.ID, exitCode, time.Now())
	if cgroupEmptyOrOnlyExitedRoot(cgroupPath, authorization.RootPID) {
		if err := s.releaseAuthorization(authorization.ID); err != nil {
			return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, err
		}
		authorizationActive = false
		if err := removeCgroup(cgroupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, err
		}
		cleanupCgroup = false
		s.finishRunJob(authorization.ID, "exited", time.Now())
	} else {
		keepAuthorization = true
	}
	if waitErr != nil && exitCode == 0 {
		return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, waitErr
	}
	return model.RunResult{AuthorizationID: authorization.ID, ExitCode: exitCode}, nil
}

func (s *Server) releaseAuthorization(id string) error {
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	if err := s.Store.ReleaseAuthorization(id); err != nil {
		return err
	}
	s.emitAuthorizationEnded(id, "released", time.Now())
	return nil
}

func (s *Server) ensureGPUCanReserve(ctx context.Context, gpu int) error {
	now := time.Now()
	return s.ensureGPUCanReserveWindow(ctx, gpu, now, now.Add(store.DefaultHardTTL))
}

func (s *Server) ensureGPUCanReserveWindow(ctx context.Context, gpu int, startsAt, expiresAt time.Time) error {
	return s.ensureGPUsCanReserveWindow(ctx, []int{gpu}, startsAt, expiresAt)
}

func (s *Server) ensureGPUsCanReserveWindow(ctx context.Context, gpus []int, startsAt, expiresAt time.Time) error {
	if err := s.validateConfiguredGPUs(gpus); err != nil {
		return err
	}
	startsAt = startsAt.UTC()
	expiresAt = expiresAt.UTC()
	if startsAt.IsZero() {
		startsAt = time.Now().UTC()
	}
	if !expiresAt.After(startsAt) {
		return errors.New("reservation end must be after start")
	}
	now := time.Now()
	if !expiresAt.After(now) {
		return errors.New("reservation end must be in the future")
	}
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return err
	}
	requested := make(map[int]bool, len(gpus))
	for _, gpu := range gpus {
		requested[gpu] = true
	}
	for _, reservation := range state.Reservations {
		if requested[reservation.GPU] && model.ReservationOverlaps(reservation, startsAt, expiresAt) {
			return fmt.Errorf("gpu %d already reserved by %s", reservation.GPU, reservation.ID)
		}
	}
	for _, lease := range state.Leases {
		if requested[lease.GPU] && lease.Active && now.Before(lease.ExpiresAt) && startsAt.Before(lease.ExpiresAt) {
			return fmt.Errorf("gpu %d already held by legacy lease %s", lease.GPU, lease.ID)
		}
	}
	if now.Before(startsAt) || !now.Before(expiresAt) {
		return nil
	}
	processes, err := s.GPU.Processes(ctx)
	if err != nil {
		return fmt.Errorf("GPU process list: %w", err)
	}
	auth := s.authorizer()
	for _, gpu := range gpus {
		busy, err := auth.BusyProcessesForGPU(ctx, state, processes, gpu)
		if err != nil {
			return err
		}
		if len(busy) > 0 {
			return fmt.Errorf("gpu %d is busy", gpu)
		}
	}
	return nil
}

func (s *Server) ensureTokenCanAuthorize(tokenHash string, token model.Token, now time.Time) error {
	if store.NormalizeTokenMode(token.Mode) != model.TokenModeReserved {
		return nil
	}
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return err
	}
	for _, reservation := range state.Reservations {
		if reservation.TokenHash == tokenHash && model.ReservationActiveAt(reservation, now) {
			return nil
		}
	}
	return errors.New("reserved token has no valid reservation")
}

func (s *Server) ps(ctx context.Context, now time.Time, allowedTokenHash string, all bool) ([]model.PSRow, error) {
	s.processReadSlotsOnce.Do(func() {
		s.processReadSlots = make(chan struct{}, maxConcurrentProcessReads)
	})
	select {
	case s.processReadSlots <- struct{}{}:
		defer func() { <-s.processReadSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var state model.State
	var err error
	if all {
		state, err = s.Store.EnforcementSnapshot()
	} else {
		state, err = s.Store.EnforcementSnapshotForToken(allowedTokenHash)
	}
	if err != nil {
		return nil, err
	}
	processes, err := s.processesForRead(ctx)
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
	liveRows := map[string]*psRowBuilder{}
	liveHard := map[string]bool{}
	retainedTextBytes := 0
	for _, decision := range decisions {
		if decision.Action != "allow" || decision.Reason == "bypass" {
			continue
		}
		if !all && (allowedTokenHash == "" || decision.TokenHash != allowedTokenHash) {
			continue
		}
		id := decision.AuthID
		if id == "" {
			id = decision.LeaseID
		}
		if id == "" {
			continue
		}
		holder := boundedPSField(strings.TrimSpace(decision.Holder))
		if holder == "" {
			holder = "unknown"
		}
		command := boundedPSField(strings.Join(decision.Info.Cmdline, " "))
		if command == "" {
			command = boundedPSField(decision.Process.Name)
		}
		if command == "" {
			command = fmt.Sprintf("pid %d", decision.Process.PID)
		}
		rowID := fmt.Sprintf("%s/%d", id, decision.Process.PID)
		row := liveRows[rowID]
		if row == nil {
			if len(liveRows) >= maxPSRows || retainedTextBytes+len(holder)+len(command) > maxPSRetainedTextBytes {
				return nil, errors.New("ps result exceeds response budget")
			}
			retainedTextBytes += len(holder) + len(command)
			row = &psRowBuilder{
				row: model.PSRow{
					ID:      rowID,
					User:    holder,
					Command: command,
				},
				gpus: map[int]bool{},
			}
			liveRows[rowID] = row
		}
		row.gpus[decision.Process.GPU] = true
		if decision.TokenHash != "" {
			liveHard[reservationLiveKey(decision.Process.GPU, decision.TokenHash)] = true
		}
	}
	rows := make([]model.PSRow, 0, len(liveRows)+len(state.Reservations))
	for _, row := range liveRows {
		row.row.GPU = formatGPUSet(row.gpus)
		rows = append(rows, row.row)
	}
	for _, reservation := range state.Reservations {
		if !all && reservation.TokenHash != allowedTokenHash {
			continue
		}
		if !model.ReservationActiveAt(reservation, now) || liveHard[reservationLiveKey(reservation.GPU, reservation.TokenHash)] {
			continue
		}
		holder := boundedPSField(reservation.Holder)
		command := "reserved until " + reservation.ExpiresAt.Format(time.RFC3339)
		if len(rows) >= maxPSRows || retainedTextBytes+len(holder)+len(command) > maxPSRetainedTextBytes {
			return nil, errors.New("ps result exceeds response budget")
		}
		retainedTextBytes += len(holder) + len(command)
		rows = append(rows, model.PSRow{
			ID:      reservation.ID,
			GPU:     strconv.Itoa(reservation.GPU),
			User:    holder,
			Command: command,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		left := firstGPU(rows[i].GPU)
		right := firstGPU(rows[j].GPU)
		if left != right {
			return left < right
		}
		return rows[i].ID < rows[j].ID
	})
	return rows, nil
}

func boundedPSField(value string) string {
	value = strings.ToValidUTF8(value, "?")
	var out strings.Builder
	out.Grow(min(len(value), maxPSFieldBytes))
	for _, char := range value {
		if unicode.IsControl(char) {
			char = '?'
		}
		size := utf8.RuneLen(char)
		if size < 0 || out.Len()+size > maxPSFieldBytes {
			break
		}
		out.WriteRune(char)
	}
	return out.String()
}

func (s *Server) processesForRead(ctx context.Context) ([]model.GPUProcess, error) {
	s.processReadMu.Lock()
	defer s.processReadMu.Unlock()
	if !s.processReadAt.IsZero() {
		return append([]model.GPUProcess(nil), s.processReadRows...), s.processReadErr
	}
	rows, err := s.GPU.Processes(ctx)
	s.processReadAt = time.Now()
	s.processReadRows = append(s.processReadRows[:0], rows...)
	s.processReadErr = err
	return append([]model.GPUProcess(nil), rows...), err
}

type psRowBuilder struct {
	row  model.PSRow
	gpus map[int]bool
}

func formatGPUSet(gpus map[int]bool) string {
	if len(gpus) == 0 {
		return ""
	}
	values := make([]int, 0, len(gpus))
	for gpu := range gpus {
		values = append(values, gpu)
	}
	sort.Ints(values)
	parts := make([]string, 0, len(values))
	for _, gpu := range values {
		parts = append(parts, strconv.Itoa(gpu))
	}
	return strings.Join(parts, ",")
}

func firstGPU(value string) int {
	head, _, _ := strings.Cut(value, ",")
	gpu, err := strconv.Atoi(strings.TrimSpace(head))
	if err != nil {
		return 1<<31 - 1
	}
	return gpu
}

func gpuListContains(value string, want int) bool {
	for _, part := range strings.Split(value, ",") {
		gpu, err := strconv.Atoi(strings.TrimSpace(part))
		if err == nil && gpu == want {
			return true
		}
	}
	return false
}

func validateGPUs(gpus []int) error {
	if len(gpus) == 0 {
		return errors.New("at least one gpu is required")
	}
	if len(gpus) > maxGPUsPerRequest {
		return fmt.Errorf("at most %d gpus may be requested", maxGPUsPerRequest)
	}
	seen := map[int]bool{}
	for _, gpu := range gpus {
		if gpu < 0 || gpu > maxGPUIndex {
			return fmt.Errorf("gpu must be between 0 and %d", maxGPUIndex)
		}
		if seen[gpu] {
			return fmt.Errorf("duplicate gpu %d", gpu)
		}
		seen[gpu] = true
	}
	return nil
}

func (s *Server) validateConfiguredGPUs(gpus []int) error {
	if err := validateGPUs(gpus); err != nil {
		return err
	}
	if s.Cfg.GPUCount > 0 {
		for _, gpu := range gpus {
			if gpu >= s.Cfg.GPUCount {
				return fmt.Errorf("gpu %d is outside configured inventory of %d GPUs", gpu, s.Cfg.GPUCount)
			}
		}
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
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	return s.addBypassLocked(args, now)
}

func (s *Server) addBypassLocked(args protocol.BypassAddArgs, now time.Time) (model.BypassRule, error) {
	if strings.TrimSpace(args.Reason) == "" {
		return model.BypassRule{}, errors.New("bypass reason is required")
	}
	if err := validateRequestValue("reason", args.Reason); err != nil {
		return model.BypassRule{}, err
	}
	if (args.PID > 0) == (strings.TrimSpace(args.Command) != "") {
		return model.BypassRule{}, errors.New("exactly one pid or command bypass selector is required")
	}
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
		if s.Proc == nil {
			return model.BypassRule{}, errors.New("process identity reader is unavailable")
		}
		info, err := s.Proc.Info(rule.PID)
		if err != nil || info.PID != rule.PID || info.StartTime == 0 {
			return model.BypassRule{}, fmt.Errorf("cannot verify pid %d identity", rule.PID)
		}
		rule.StartTime = info.StartTime
		if s.bootID == "" {
			return model.BypassRule{}, errors.New("cannot create pid bypass without a verified boot id")
		}
		rule.BootID = s.bootID
	case model.BypassCommand:
		if rule.Command == "" || !filepath.IsAbs(rule.Command) {
			return model.BypassRule{}, errors.New("command bypass requires absolute --command")
		}
		if rule.UID != 0 {
			return model.BypassRule{}, errors.New("command bypass is restricted to uid 0; use a PID bypass for non-root processes")
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
	s.monitorOnce(ctx)
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
	processes, err := s.GPU.Processes(ctx)
	s.processReadMu.Lock()
	s.processReadAt = time.Now()
	s.processReadRows = append(s.processReadRows[:0], processes...)
	s.processReadErr = err
	s.processReadMu.Unlock()
	if err != nil {
		s.auditGPUProcessError(err)
		s.cleanupExpiredManagedCgroups()
		return
	}
	s.auditGPUProcessRecovery()
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	authorizer := s.authorizer()
	var enforcementAudits []model.AuditEvent
	authorizer.OnAudit = func(event model.AuditEvent) {
		if s.acceptEnforcementAudit(event) {
			enforcementAudits = append(enforcementAudits, event)
		}
	}
	enforcementCtx, cancelEnforcement := context.WithTimeout(ctx, enforcementPassTimeout)
	defer cancelEnforcement()
	decisions, assessment, evictionErr := authorizer.EvictExpiredReservedProcesses(enforcementCtx, state, processes)
	s.recordEvictionAssessment(assessment)
	if len(decisions) > 0 {
		targeted := make(map[int]struct{}, len(decisions))
		for _, decision := range decisions {
			if decision.Process.PID > 0 {
				targeted[decision.Process.PID] = struct{}{}
			}
		}
		if len(targeted) > 0 {
			remaining := make([]model.GPUProcess, 0, len(processes))
			for _, process := range processes {
				if _, found := targeted[process.PID]; !found {
					remaining = append(remaining, process)
				}
			}
			processes = remaining
		}
	}
	regularDecisions, enforceErr := authorizer.Enforce(enforcementCtx, state, processes)
	decisions = append(decisions, regularDecisions...)
	now := time.Now()
	s.trackObservedTelemetryJobs(state, decisions, now)
	_ = enforceErr // Kill failures are audited by the authorizer callback.
	for _, decision := range decisions {
		switch decision.Action {
		case "claim":
			_ = s.Store.UpsertSoftClaim(decision.Claim, now)
		case "release_claim":
			_ = s.Store.ReleaseSoftClaim(decision.ClaimID)
		}
	}
	_ = s.Store.AppendAudits(enforcementAudits)
	cleanupOK := s.cleanupExpiredReservationsState(state)
	cleanupOK = s.cleanupExpiredAuthorizationsState(state) && cleanupOK
	s.cleanupFinishedBareAuthorizationsState(state)
	cleanupOK = s.cleanupExpiredLeasesState(state) && cleanupOK
	s.cleanupFinishedBareLeasesState(state)
	if evictionErr == nil && cleanupOK {
		_ = s.Store.Prune(now)
	}
}

func (s *Server) revoke(id string) error {
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	return s.Store.Revoke(id)
}

func (s *Server) recordEvictionAssessment(assessment enforce.EvictionAssessment) {
	if s.evictionClean == nil {
		s.evictionClean = make(map[string]int)
	}
	seen := make(map[string]bool, len(assessment.Authorizations)+len(assessment.Leases))
	for id, clean := range assessment.Authorizations {
		key := "authorization:" + id
		seen[key] = true
		if clean {
			s.evictionClean[key]++
		} else {
			s.evictionClean[key] = 0
		}
	}
	for id, clean := range assessment.Leases {
		key := "lease:" + id
		seen[key] = true
		if clean {
			s.evictionClean[key]++
		} else {
			s.evictionClean[key] = 0
		}
	}
	for key := range s.evictionClean {
		if !seen[key] {
			delete(s.evictionClean, key)
		}
	}
}

func (s *Server) evictionScopeReady(kind, id string) bool {
	return s.evictionClean[kind+":"+id] >= evictionCleanSamples
}

func (s *Server) auditGPUProcessError(processErr error) {
	now := time.Now().UTC()
	message := processErr.Error()
	s.auditMu.Lock()
	if message == s.lastGPUError && now.Sub(s.lastGPUErrorAt) < time.Minute {
		s.auditMu.Unlock()
		return
	}
	s.lastGPUError = message
	s.lastGPUErrorAt = now
	s.auditMu.Unlock()
	_ = s.Store.AppendAudit(model.AuditEvent{Time: now, Kind: "error", Message: "GPU process list failed: " + message})
}

func (s *Server) auditGPUProcessRecovery() {
	s.auditMu.Lock()
	if s.lastGPUError == "" {
		s.auditMu.Unlock()
		return
	}
	s.lastGPUError = ""
	s.lastGPUErrorAt = time.Time{}
	s.auditMu.Unlock()
	_ = s.Store.AppendAudit(model.AuditEvent{Time: time.Now().UTC(), Kind: "recovery", Message: "GPU process list recovered"})
}

func (s *Server) cleanupFinishedBareAuthorizations() {
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	s.cleanupFinishedBareAuthorizationsState(state)
}

func (s *Server) cleanupFinishedBareAuthorizationsState(state model.State) {
	now := time.Now()
	for _, authorization := range state.Authorizations {
		if !authorization.Active || authorization.Mode != model.ModeBare || authorization.RootPID <= 0 {
			continue
		}
		if authorization.CgroupPath != "" {
			if !cgroupEmpty(authorization.CgroupPath) {
				continue
			}
			managed, err := cgroupPathWithinRoot(s.Cfg.CgroupRoot, authorization.CgroupPath)
			if err != nil || !managed {
				continue
			}
			if err := removeCgroup(authorization.CgroupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
		} else if s.Proc != nil && s.Proc.Exists(authorization.RootPID) {
			continue
		}
		if err := s.Store.ReleaseAuthorization(authorization.ID); err != nil {
			continue
		}
		s.emitAuthorizationEnded(authorization.ID, "released", now)
		s.finishRunJob(authorization.ID, "cgroup_empty", now)
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
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	s.cleanupExpiredAuthorizationsState(state)
}

// cleanupExpiredManagedCgroups does not depend on the GPU process inventory.
// Keep this path available during telemetry outages so revocation and expiry
// still stop workloads that gpuardian placed in a managed cgroup. Non-cgroup
// scopes retain their evidence until a successful GPU sample can assess them.
func (s *Server) cleanupExpiredManagedCgroups() {
	s.enforceMu.Lock()
	defer s.enforceMu.Unlock()
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	filtered := state
	filtered.Authorizations = nil
	for _, authorization := range state.Authorizations {
		if authorization.CgroupPath != "" {
			filtered.Authorizations = append(filtered.Authorizations, authorization)
		}
	}
	filtered.Leases = nil
	for _, lease := range state.Leases {
		if lease.CgroupPath != "" {
			filtered.Leases = append(filtered.Leases, lease)
		}
	}
	s.cleanupExpiredAuthorizationsState(filtered)
	s.cleanupExpiredLeasesState(filtered)

	state, err = s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	var authorizationIDs []string
	for _, authorization := range state.Authorizations {
		if !authorization.Active && authorization.CgroupPath != "" && s.inactiveManagedCgroupReadyForPrune(authorization.CgroupPath) {
			authorizationIDs = append(authorizationIDs, authorization.ID)
		}
	}
	var leaseIDs []string
	for _, lease := range state.Leases {
		if !lease.Active && lease.CgroupPath != "" && s.inactiveManagedCgroupReadyForPrune(lease.CgroupPath) {
			leaseIDs = append(leaseIDs, lease.ID)
		}
	}
	_ = s.Store.PruneInactiveManagedScopes(authorizationIDs, leaseIDs)
	_ = s.Store.PruneUnreferencedInvalidEntitlements(time.Now())
}

func (s *Server) inactiveManagedCgroupReadyForPrune(cgroupPath string) bool {
	managed, err := cgroupPathWithinRoot(s.Cfg.CgroupRoot, cgroupPath)
	if err != nil || !managed || !cgroupEmpty(cgroupPath) {
		return false
	}
	err = removeCgroup(cgroupPath)
	return err == nil || errors.Is(err, os.ErrNotExist)
}

func (s *Server) cleanupExpiredAuthorizationsState(state model.State) bool {
	now := time.Now()
	ok := true
	tokens := tokenValidityByHash(state.Tokens, now)
	for _, authorization := range state.Authorizations {
		if !authorization.Active || (!authorization.Revoked && !authorizationExpiredAt(authorization, now) && tokenValidForCleanup(tokens, authorization.TokenHash)) {
			continue
		}
		if authorization.CgroupPath != "" {
			if err := s.stopAndRemoveManagedCgroup(authorization.CgroupPath); err != nil {
				ok = false
				continue
			}
		} else if authorization.Mode != model.ModeBare && !s.evictionScopeReady("authorization", authorization.ID) {
			ok = false
			continue
		}
		if err := s.Store.ReleaseAuthorization(authorization.ID); err != nil {
			ok = false
			continue
		}
		kind := "authorization_expired"
		message := "authorization expired"
		reason := "expired"
		if authorization.Revoked || !tokenValidForCleanup(tokens, authorization.TokenHash) {
			kind = "authorization_revoked"
			message = "authorization revoked"
			reason = "revoked"
		}
		s.emitAuthorizationEnded(authorization.ID, reason, now)
		if authorization.Mode == model.ModeBare {
			s.finishRunJob(authorization.ID, reason, now)
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: kind, Message: message, LeaseID: authorization.ID, User: authorization.Holder})
	}
	return ok
}

func (s *Server) cleanupExpiredReservations() {
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	s.cleanupExpiredReservationsState(state)
}

func (s *Server) cleanupExpiredReservationsState(state model.State) bool {
	now := time.Now()
	ok := true
	tokenIDs := make(map[string]string, len(state.Tokens))
	for _, token := range state.Tokens {
		tokenIDs[token.Hash] = token.ID
	}
	emitted := make(map[string]bool)
	for _, reservation := range state.Reservations {
		if !reservation.Active || (!reservation.Revoked && now.Before(reservation.ExpiresAt)) {
			continue
		}
		if err := s.Store.ReleaseReservation(reservation.ID); err != nil {
			ok = false
			continue
		}
		groupID := tokenIDs[reservation.TokenHash]
		if reservation.GroupID != "" {
			groupID = reservation.GroupID
		}
		if groupID != "" && !emitted[groupID] {
			reason := "expired"
			if reservation.Revoked {
				reason = "revoked"
			}
			s.emitTelemetry(telemetry.EventReservationEnded, telemetry.ReservationEnded{GroupID: groupID, EndedAt: now.UTC(), Reason: reason}, now)
			emitted[groupID] = true
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: "reservation_expired", Message: "reserved GPU reservation expired", GPU: reservation.GPU, LeaseID: reservation.ID, User: reservation.Holder})
	}
	return ok
}

func (s *Server) cleanupFinishedBareLeases() {
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	s.cleanupFinishedBareLeasesState(state)
}

func (s *Server) cleanupFinishedBareLeasesState(state model.State) {
	now := time.Now()
	for _, lease := range state.Leases {
		if !lease.Active || lease.Mode != model.ModeBare || lease.RootPID <= 0 {
			continue
		}
		if lease.CgroupPath != "" {
			if !cgroupEmpty(lease.CgroupPath) {
				continue
			}
			managed, err := cgroupPathWithinRoot(s.Cfg.CgroupRoot, lease.CgroupPath)
			if err != nil || !managed {
				continue
			}
			if err := removeCgroup(lease.CgroupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				continue
			}
		} else if s.Proc != nil && s.Proc.Exists(lease.RootPID) {
			continue
		}
		if err := s.Store.ReleaseLease(lease.ID); err != nil {
			continue
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
	state, err := s.Store.EnforcementSnapshot()
	if err != nil {
		return
	}
	s.cleanupExpiredLeasesState(state)
}

func (s *Server) cleanupExpiredLeasesState(state model.State) bool {
	now := time.Now()
	ok := true
	tokens := tokenValidityByHash(state.Tokens, now)
	for _, lease := range state.Leases {
		if !lease.Active || (now.Before(lease.ExpiresAt) && tokenValidForCleanup(tokens, lease.TokenHash)) {
			continue
		}
		if lease.CgroupPath != "" {
			if err := s.stopAndRemoveManagedCgroup(lease.CgroupPath); err != nil {
				ok = false
				continue
			}
		} else if lease.Mode != model.ModeBare && !s.evictionScopeReady("lease", lease.ID) {
			ok = false
			continue
		}
		if err := s.Store.ReleaseLease(lease.ID); err != nil {
			ok = false
			continue
		}
		_ = s.Store.AppendAudit(model.AuditEvent{Time: now.UTC(), Kind: "lease_expired", Message: "lease expired", GPU: lease.GPU, LeaseID: lease.ID})
	}
	return ok
}

func tokenValidityByHash(tokens []model.Token, now time.Time) map[string]bool {
	valid := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		valid[token.Hash] = !token.Revoked && (!timeIsSet(token.ExpiresAt) || now.Before(token.ExpiresAt))
	}
	return valid
}

func tokenValidForCleanup(valid map[string]bool, tokenHash string) bool {
	return tokenHash == "" || valid[tokenHash]
}

func authorizationExpiredAt(authorization model.Authorization, now time.Time) bool {
	return timeIsSet(authorization.ExpiresAt) && !now.Before(authorization.ExpiresAt)
}

func (s *Server) stopAndRemoveManagedCgroup(cgroupPath string) error {
	managed, err := cgroupPathWithinRoot(s.Cfg.CgroupRoot, cgroupPath)
	if err != nil {
		return err
	}
	if !managed {
		return fmt.Errorf("refusing to manage cgroup outside configured root: %s", cgroupPath)
	}
	if s.Cfg.DryRun {
		if cgroupEmpty(cgroupPath) {
			return removeCgroup(cgroupPath)
		}
		return fmt.Errorf("dry-run left nonempty managed cgroup %s untouched", cgroupPath)
	}
	if !cgroupEmpty(cgroupPath) {
		killPath := filepath.Join(cgroupPath, "cgroup.kill")
		fd, err := syscall.Open(killPath, syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return &os.PathError{Op: "open", Path: killPath, Err: err}
		}
		file := os.NewFile(uintptr(fd), killPath)
		if file == nil {
			_ = syscall.Close(fd)
			return fmt.Errorf("open %s: invalid file descriptor", killPath)
		}
		_, writeErr := file.WriteString("1")
		closeErr := file.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
		if !cgroupEmpty(cgroupPath) {
			return fmt.Errorf("cgroup %s is still draining", cgroupPath)
		}
	}
	if err := removeCgroup(cgroupPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func cgroupPathWithinRoot(root, candidate string) (bool, error) {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return false, err
	}
	candidate, err = filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return relative != "." && relative != "" && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)), nil
}

func (s *Server) authorizer() enforce.Authorizer {
	type gpuProcessKey struct {
		gpu int
		pid int
	}
	var sampleOnce sync.Once
	var sampleErr error
	present := make(map[gpuProcessKey]struct{})
	validateKill := func(ctx context.Context, process model.GPUProcess) error {
		sampleOnce.Do(func() {
			if s.GPU == nil {
				sampleErr = errors.New("GPU process provider is unavailable")
				return
			}
			var processes []model.GPUProcess
			processes, sampleErr = s.GPU.Processes(ctx)
			if sampleErr != nil {
				s.auditGPUProcessError(sampleErr)
				return
			}
			for _, current := range processes {
				if current.PID > 0 {
					present[gpuProcessKey{gpu: current.GPU, pid: current.PID}] = struct{}{}
				}
			}
		})
		if sampleErr != nil {
			return sampleErr
		}
		if _, ok := present[gpuProcessKey{gpu: process.GPU, pid: process.PID}]; !ok {
			return errors.New("pid is no longer present in the GPU process inventory")
		}
		return nil
	}
	return enforce.Authorizer{
		Proc:         s.Proc,
		Runtime:      s.Runtime,
		Killer:       s.Killer,
		BootID:       s.bootID,
		ValidateKill: validateKill,
		DryRun:       s.Cfg.DryRun,
		OnAudit:      s.appendEnforcementAudit,
	}
}

func (s *Server) appendEnforcementAudit(event model.AuditEvent) {
	if !s.acceptEnforcementAudit(event) {
		return
	}
	_ = s.Store.AppendAudit(event)
}

func (s *Server) acceptEnforcementAudit(event model.AuditEvent) bool {
	if event.Kind == "kill_failed" {
		key := fmt.Sprintf("%d:%s", event.PID, event.Message)
		now := time.Now().UTC()
		s.auditMu.Lock()
		if key == s.lastKillError && now.Sub(s.lastKillErrorAt) < time.Minute {
			s.auditMu.Unlock()
			return false
		}
		s.lastKillError = key
		s.lastKillErrorAt = now
		s.auditMu.Unlock()
	}
	return true
}

func (s *Server) createCgroup(leaseID string) (string, string, error) {
	path := filepath.Join(s.Cfg.CgroupRoot, leaseID)
	if err := os.MkdirAll(path, 0750); err != nil {
		return "", "", err
	}
	rel := strings.TrimPrefix(path, "/sys/fs/cgroup/")
	if rel == path {
		rel = filepath.Join(filepath.Base(s.Cfg.CgroupRoot), leaseID)
	}
	return path, filepath.ToSlash(rel), nil
}

func openCgroupFD(cgroupPath string) (int, bool, error) {
	if _, err := os.Stat(filepath.Join(cgroupPath, "cgroup.procs")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return -1, false, nil
		}
		return -1, false, err
	}
	fd, err := syscall.Open(cgroupPath, openPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, false, &os.PathError{Op: "open", Path: cgroupPath, Err: err}
	}
	return fd, true, nil
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

func cgroupEmptyOrOnlyExitedRoot(cgroupPath string, rootPID int) bool {
	if cgroupEmpty(cgroupPath) {
		return true
	}
	data, err := os.ReadFile(filepath.Join(cgroupPath, "cgroup.procs"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || processExists(rootPID) {
		return false
	}
	for _, field := range fields {
		pid, err := strconv.Atoi(field)
		if err != nil || pid != rootPID {
			return false
		}
	}
	return true
}

func removeCgroup(cgroupPath string) error {
	err := os.Remove(cgroupPath)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return err
	}
	var filesystem syscall.Statfs_t
	if statErr := syscall.Statfs(cgroupPath, &filesystem); statErr != nil || uint64(filesystem.Type) == cgroup2SuperMagic {
		return err
	}
	controlPath := filepath.Join(cgroupPath, "cgroup.procs")
	info, statErr := os.Lstat(controlPath)
	if statErr != nil || !info.Mode().IsRegular() {
		return err
	}
	if removeErr := os.Remove(controlPath); removeErr != nil {
		return err
	}
	return os.Remove(cgroupPath)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func acquireUnixSocketLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open socket lock %s: invalid file descriptor", path)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("socket lock %s is not a regular file", path)
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("gpuardian daemon already holds %s", path)
		}
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func prepareUnixSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", path)
	}
	conn, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("unix socket %s is already accepting connections", path)
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("refusing to replace possibly active unix socket %s: %w", path, dialErr)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeOwnedUnixSocket(path string, expected os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("refusing to remove replaced unix socket %s", path)
	}
	return os.Remove(path)
}

func streamCopy(wg *sync.WaitGroup, mu *sync.Mutex, writer io.Writer, reqID, kind string, reader io.Reader) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			mu.Lock()
			writeErr := writeResult(writer, protocol.Response{ID: reqID, Kind: kind, OK: true, Data: string(buf[:n])})
			mu.Unlock()
			if writeErr != nil {
				if closer, ok := writer.(io.Closer); ok {
					_ = closer.Close()
				}
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func writeResult(writer io.Writer, resp protocol.Response) error {
	if conn, ok := writer.(interface{ SetWriteDeadline(time.Time) error }); ok {
		if err := conn.SetWriteDeadline(time.Now().Add(rpcWriteTimeout)); err != nil {
			return err
		}
		defer conn.SetWriteDeadline(time.Time{})
	}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	if len(data) > maxRPCFrameBytes {
		return fmt.Errorf("RPC response exceeds %d bytes", maxRPCFrameBytes)
	}
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		data = data[n:]
	}
	return nil
}

func commandEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, "KEY=") ||
			strings.HasPrefix(item, "ROOT_KEY=") ||
			strings.HasPrefix(item, "GPUARDIAN_WEB_PASSWORD=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func peerCred(conn net.Conn, _ string) (peer, error) {
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
	return peer{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid)}, nil
}

func readBootID(procRoot string) (string, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	file, err := os.Open(filepath.Join(procRoot, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if len(value) != 36 {
		return "", errors.New("invalid kernel boot id")
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return "", errors.New("invalid kernel boot id")
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return "", errors.New("invalid kernel boot id")
		}
	}
	return strings.ToLower(value), nil
}

func peerProcessGroups(procRoot string, pid, uid, primaryGID int) ([]uint32, error) {
	if procRoot == "" {
		procRoot = "/proc"
	}
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, fmt.Errorf("read peer process groups: %w", err)
	}
	verifiedUID := false
	var groupIDs []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			if len(fields) < 2 || fields[1] != strconv.Itoa(uid) {
				return nil, errors.New("peer process identity changed before group lookup")
			}
			verifiedUID = true
		case "Groups:":
			groupIDs = fields[1:]
		}
	}
	if !verifiedUID {
		return nil, errors.New("peer process status is missing uid")
	}
	return normalizePeerGroups(primaryGID, groupIDs), nil
}

func normalizePeerGroups(primaryGID int, groupIDs []string) []uint32 {
	groups := []uint32{uint32(primaryGID)}
	seen := map[uint32]bool{uint32(primaryGID): true}
	for _, field := range groupIDs {
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
