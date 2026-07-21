package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"gpuardian/internal/config"
	"gpuardian/internal/daemon"
	"gpuardian/internal/model"
	"gpuardian/internal/protocol"
	webserver "gpuardian/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gpuardian:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := config.Default()
	if len(args) == 0 {
		usage()
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		usage()
		return nil
	case "daemon":
		return daemonCommand(cfg, args[1:])
	case "web":
		return webCommand(cfg, args[1:])
	case "show-keys":
		rootKey, err := rootKeyFromEnvOrPrompt()
		if err != nil {
			return err
		}
		return printKeyStatusRPC(cfg, protocol.RootKeyArgs{RootKey: rootKey})
	case "register":
		return register(cfg, args[1:])
	case "run":
		return runCommand(cfg, args[1:])
	case "allow":
		return allowCommand(cfg, args[1:])
	case "status":
		return printStatusRPC(cfg)
	case "ps":
		return psCommand(cfg)
	case "token":
		return tokenCommand(cfg, args[1:])
	case "bypass":
		return bypassCommand(cfg, args[1:])
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: gpuardian revoke <token-or-reservation-or-authorization-or-bypass-id>")
		}
		rootKey, err := rootKeyFromEnvOrPrompt()
		if err != nil {
			return err
		}
		return printRPC(cfg, "revoke", "", protocol.RevokeArgs{RootKey: rootKey, ID: args[1]})
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func webCommand(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", cfg.WebAddr, "web listen address")
	registry := fs.String("registry", cfg.WebRegistry, "server registry path")
	uiDir := fs.String("ui-dir", cfg.WebUIDir, "React/Vite dist directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.WebAddr = *addr
	cfg.WebRegistry = *registry
	cfg.WebUIDir = *uiDir
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return webserver.New(cfg).Run(ctx)
}

func daemonCommand(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "record decisions without killing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dryRun {
		cfg.DryRun = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return daemon.New(cfg).Run(ctx)
}

func register(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	reserved := fs.Bool("reserved", false, "create a reserved GPU key")
	claimed := fs.Bool("claimed", false, "create a claimed-mode key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	mode := ""
	for _, selected := range []struct {
		ok   bool
		mode string
	}{
		{*reserved, model.TokenModeReserved},
		{*claimed, model.TokenModeClaimed},
	} {
		if !selected.ok {
			continue
		}
		if mode != "" && mode != selected.mode {
			return errors.New("usage: gpuardian register (--reserved | --claimed)")
		}
		mode = selected.mode
	}
	if mode == "" {
		return errors.New("usage: gpuardian register (--reserved | --claimed)")
	}
	reader := bufio.NewReader(os.Stdin)
	rootKey, err := promptSecret(reader, "Root key: ")
	if err != nil {
		return err
	}
	name, err := prompt(reader, "Name: ")
	if err != nil {
		return err
	}
	registerArgs := protocol.RegisterArgs{RootKey: rootKey, Name: name}
	if mode == model.TokenModeReserved {
		registerArgs.Mode = model.TokenModeReserved
		gpuText, err := prompt(reader, "GPUs: ")
		if err != nil {
			return err
		}
		gpus, err := parseGPUList(gpuText)
		if err != nil {
			return err
		}
		ttl, err := prompt(reader, "TTL [2h]: ")
		if err != nil {
			return err
		}
		registerArgs.GPUs = gpus
		registerArgs.TTL = ttl
	} else {
		registerArgs.Mode = model.TokenModeClaimed
	}
	raw, err := callRPC(cfg, "register", "", registerArgs, false)
	if err != nil {
		return err
	}
	var result model.RegisterResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	fmt.Printf("Token: %s\nMode: %s\n", result.Token, result.Mode)
	if len(result.ReservationIDs) > 0 {
		fmt.Printf("Reservations: %s\nGPUs: %s\n", strings.Join(result.ReservationIDs, ","), formatIntList(result.GPUs))
	}
	if result.ExpiresAt != nil {
		fmt.Printf("Expires at: %s\n", result.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

func parseGPUList(value string) ([]int, error) {
	parts := strings.Split(value, ",")
	gpus := make([]int, 0, len(parts))
	seen := map[int]bool{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		gpu, err := strconv.Atoi(part)
		if err != nil || gpu < 0 {
			return nil, errors.New("gpu must be >= 0")
		}
		if seen[gpu] {
			return nil, fmt.Errorf("duplicate gpu %d", gpu)
		}
		seen[gpu] = true
		gpus = append(gpus, gpu)
	}
	if len(gpus) == 0 {
		return nil, errors.New("at least one gpu is required")
	}
	return gpus, nil
}

func formatIntList(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ",")
}

func runCommand(cfg config.Config, args []string) error {
	command := args
	if len(command) > 0 && command[0] != "--" && strings.HasPrefix(command[0], "-") {
		return errors.New("usage: KEY=... gpuardian run -- <command>")
	}
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if len(command) == 0 {
		return errors.New("usage: KEY=... gpuardian run -- <command>")
	}
	if !strings.ContainsRune(command[0], filepath.Separator) {
		resolved, err := exec.LookPath(command[0])
		if err != nil {
			return fmt.Errorf("resolve command %q using caller PATH: %w", command[0], err)
		}
		command = append([]string(nil), command...)
		command[0] = resolved
	}
	workdir, _ := os.Getwd()
	raw, err := callRPC(cfg, "run", requiredToken(), protocol.RunArgs{
		Command: command,
		Workdir: workdir,
		Env:     os.Environ(),
	}, true)
	if err != nil {
		return err
	}
	var result model.RunResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
	return nil
}

func allowCommand(cfg config.Config, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: KEY=... gpuardian allow (docker|k8s|user) ...")
	}
	switch args[0] {
	case "docker":
		fs := flag.NewFlagSet("allow docker", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		container := fs.String("container", "", "container name or id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return printRPC(cfg, "allow_docker", requiredToken(), protocol.DockerAllowArgs{Container: *container})
	case "k8s":
		fs := flag.NewFlagSet("allow k8s", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		namespace := fs.String("namespace", "", "namespace")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return printRPC(cfg, "allow_k8s", requiredToken(), protocol.K8sAllowArgs{Namespace: *namespace})
	case "user":
		fs := flag.NewFlagSet("allow user", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		username := fs.String("name", "", "username")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return printRPC(cfg, "allow_user", requiredToken(), protocol.UserAllowArgs{User: *username})
	default:
		return fmt.Errorf("unknown allow scope %q", args[0])
	}
}

func tokenCommand(cfg config.Config, args []string) error {
	if len(args) != 1 || args[0] != "info" {
		return errors.New("usage: gpuardian token info")
	}
	return printRPC(cfg, "token_info", requiredToken(), protocol.TokenInfoArgs{})
}

func bypassCommand(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: gpuardian bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>")
	}
	fs := flag.NewFlagSet("bypass add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pid := fs.Int("pid", 0, "pid")
	command := fs.String("command", "", "absolute command path")
	uid := fs.Int("uid", -1, "uid")
	ttl := fs.String("ttl", "2h", "ttl")
	reason := fs.String("reason", "", "reason")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	hasPID := *pid > 0
	hasCommand := strings.TrimSpace(*command) != ""
	if hasPID == hasCommand {
		return errors.New("exactly one of --pid or --command is required")
	}
	if strings.TrimSpace(*reason) == "" {
		return errors.New("--reason is required")
	}
	if hasCommand {
		if !filepath.IsAbs(*command) {
			return errors.New("--command must be an absolute path")
		}
		if *uid != 0 {
			return errors.New("--command bypass is restricted to --uid 0; use --pid for non-root processes")
		}
	}
	kind := model.BypassPID
	if hasCommand {
		kind = model.BypassCommand
	}
	rootKey, err := rootKeyFromEnvOrPrompt()
	if err != nil {
		return err
	}
	return printRPC(cfg, "bypass_add", "", protocol.BypassAddArgs{
		RootKey: rootKey,
		Type:    kind,
		PID:     *pid,
		Command: *command,
		UID:     *uid,
		TTL:     *ttl,
		Reason:  *reason,
	})
}

func psCommand(cfg config.Config) error {
	raw, err := callRPC(cfg, "ps", tokenFromEnv(), nil, false)
	if err != nil {
		return err
	}
	var rows []model.PSRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	return writePSRows(os.Stdout, rows)
}

func writePSRows(out io.Writer, rows []model.PSRow) error {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "id\tgpu\tuser\tcommand")
	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", terminalSafeCell(row.ID), terminalSafeCell(row.GPU), terminalSafeCell(row.User), terminalSafeCell(row.Command))
	}
	return writer.Flush()
}

func terminalSafeCell(value string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return '?'
		}
		return r
	}, value)
}

func printRPC(cfg config.Config, method, token string, args any) error {
	raw, err := callRPC(cfg, method, token, args, false)
	if err != nil {
		return err
	}
	return printJSON(raw)
}

func printStatusRPC(cfg config.Config) error {
	raw, err := callRPC(cfg, "status", tokenFromEnv(), nil, false)
	if err != nil {
		return err
	}
	var status model.Status
	if err := json.Unmarshal(raw, &status); err == nil {
		filterStatus(&status)
		out, _ := json.MarshalIndent(status, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	return printJSON(raw)
}

func printKeyStatusRPC(cfg config.Config, args protocol.RootKeyArgs) error {
	raw, err := callRPC(cfg, "show_keys", "", args, false)
	if err != nil {
		return err
	}
	var status model.KeyStatus
	if err := json.Unmarshal(raw, &status); err == nil {
		filterKeyStatus(&status)
		out, _ := json.MarshalIndent(status, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	return printJSON(raw)
}

func printJSON(raw json.RawMessage) error {
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	out, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(out))
	return nil
}

func filterStatus(status *model.Status) {
	status.Tokens = filterTokens(status.Tokens, status.Now)
	status.Reservations = filterReservations(status.Reservations, status.Now)
	status.Authorizations = filterAuthorizations(status.Authorizations, status.Now)
	status.Bypasses = filterBypasses(status.Bypasses, status.Now)

	activeAuthorizationIDs := map[string]bool{}
	for _, authorization := range status.Authorizations {
		activeAuthorizationIDs[authorization.ID] = true
	}
	filteredClaims := status.SoftClaims[:0]
	for _, claim := range status.SoftClaims {
		if activeAuthorizationIDs[claim.AuthorizationID] {
			filteredClaims = append(filteredClaims, claim)
		}
	}
	status.SoftClaims = filteredClaims
}

func filterKeyStatus(status *model.KeyStatus) {
	status.Tokens = filterTokens(status.Tokens, status.Now)
	status.Reservations = filterReservations(status.Reservations, status.Now)
	status.Authorizations = filterAuthorizations(status.Authorizations, status.Now)
	status.Bypasses = filterBypasses(status.Bypasses, status.Now)
}

func filterTokens(tokens []model.TokenView, now time.Time) []model.TokenView {
	filtered := tokens[:0]
	for _, token := range tokens {
		if !token.Revoked && (token.ExpiresAt == nil || now.Before(*token.ExpiresAt)) {
			filtered = append(filtered, token)
		}
	}
	return filtered
}

func filterReservations(reservations []model.ReservationView, now time.Time) []model.ReservationView {
	filtered := reservations[:0]
	for _, reservation := range reservations {
		if reservation.Active && !reservation.Revoked && now.Before(reservation.ExpiresAt) {
			filtered = append(filtered, reservation)
		}
	}
	return filtered
}

func filterAuthorizations(authorizations []model.AuthorizationView, now time.Time) []model.AuthorizationView {
	filtered := authorizations[:0]
	for _, authorization := range authorizations {
		if authorization.Active && !authorization.Revoked && (authorization.ExpiresAt == nil || now.Before(*authorization.ExpiresAt)) {
			filtered = append(filtered, authorization)
		}
	}
	return filtered
}

func filterBypasses(bypasses []model.BypassRule, now time.Time) []model.BypassRule {
	filtered := bypasses[:0]
	for _, bypass := range bypasses {
		if !bypass.Revoked && now.Before(bypass.ExpiresAt) {
			filtered = append(filtered, bypass)
		}
	}
	return filtered
}

func rootKeyFromEnvOrPrompt() (string, error) {
	if value := strings.TrimSpace(os.Getenv("ROOT_KEY")); value != "" {
		return value, nil
	}
	return promptSecret(bufio.NewReader(os.Stdin), "Root key: ")
}

func callRPC(cfg config.Config, method, token string, args any, stream bool) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", cfg.SocketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.SocketPath, err)
	}
	defer conn.Close()
	if !stream {
		if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return nil, err
		}
	} else if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return nil, err
	}
	var rawArgs json.RawMessage
	if args != nil {
		rawArgs, err = json.Marshal(args)
		if err != nil {
			return nil, err
		}
	}
	req := protocol.Request{
		ID:     strconv.FormatInt(time.Now().UnixNano(), 36),
		Method: method,
		Token:  token,
		Args:   rawArgs,
	}
	data, _ := json.Marshal(req)
	if err := writeAll(conn, append(data, '\n')); err != nil {
		return nil, err
	}
	if stream {
		if err := conn.SetWriteDeadline(time.Time{}); err != nil {
			return nil, err
		}
	}
	decoder := json.NewDecoder(conn)
	for {
		var resp protocol.Response
		if err := decoder.Decode(&resp); err != nil {
			return nil, err
		}
		if resp.ID != req.ID {
			continue
		}
		switch resp.Kind {
		case protocol.KindStdout:
			if stream {
				fmt.Fprint(os.Stdout, resp.Data)
			}
		case protocol.KindStderr:
			if stream {
				fmt.Fprint(os.Stderr, resp.Data)
			}
		default:
			if !resp.OK {
				return nil, errors.New(resp.Error)
			}
			return resp.Result, nil
		}
	}
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func prompt(reader *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func promptSecret(reader *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	fd := os.Stdin.Fd()
	state, ok, err := getTermios(fd)
	if err != nil {
		return "", err
	}
	if !ok {
		return readPromptValue(reader)
	}

	noEcho := *state
	noEcho.Lflag &^= syscall.ECHO
	if err := setTermios(fd, &noEcho); err != nil {
		return "", err
	}
	restored := false
	defer func() {
		if !restored {
			_ = setTermios(fd, state)
		}
	}()
	value, readErr := readPromptValue(reader)
	restoreErr := setTermios(fd, state)
	restored = restoreErr == nil
	fmt.Println()
	if readErr != nil {
		return "", readErr
	}
	if restoreErr != nil {
		return "", restoreErr
	}
	return value, nil
}

func readPromptValue(reader *bufio.Reader) (string, error) {
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func getTermios(fd uintptr) (*syscall.Termios, bool, error) {
	var state syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&state)), 0, 0, 0)
	if errno == 0 {
		return &state, true, nil
	}
	if errno == syscall.ENOTTY || errno == syscall.EINVAL {
		return nil, false, nil
	}
	return nil, false, errno
}

func setTermios(fd uintptr, state *syscall.Termios) error {
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(state)), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func requiredToken() string {
	token := tokenFromEnv()
	if token == "" {
		fmt.Fprintln(os.Stderr, "gpuardian: KEY token is required")
		os.Exit(1)
	}
	return token
}

func tokenFromEnv() string {
	return os.Getenv("KEY")
}

func usage() {
	fmt.Print(usageText())
}

func usageText() string {
	return `gpuardian commands:
  gpuardian help
  gpuardian daemon [--dry-run]
  gpuardian web [--addr <host:port>] [--registry <path>] [--ui-dir <path>]
  gpuardian register (--reserved | --claimed)
  KEY=... gpuardian run -- <command>
  KEY=... gpuardian allow docker --container <name-or-id>
  KEY=... gpuardian allow k8s --namespace <name>
  KEY=... gpuardian allow user --name <name>
  KEY=... gpuardian status  (root may omit KEY)
  KEY=... gpuardian ps      (root may omit KEY)
  KEY=... gpuardian token info
  ROOT_KEY=... gpuardian show-keys
  ROOT_KEY=... gpuardian bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>
  ROOT_KEY=... gpuardian revoke <token-or-reservation-or-authorization-or-bypass-id>
`
}
