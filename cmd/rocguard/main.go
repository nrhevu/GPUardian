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
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rocguardd/internal/config"
	"rocguardd/internal/daemon"
	"rocguardd/internal/model"
	"rocguardd/internal/protocol"
	"rocguardd/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rocguard:", err)
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
	case "daemon":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return daemon.New(cfg).Run(ctx)
	case "show-key":
		key, err := store.New(cfg).ReadOrCreateRootKey()
		if err != nil {
			return err
		}
		fmt.Println(key)
		return nil
	case "register":
		return register(cfg)
	case "run":
		return runCommand(cfg, args[1:])
	case "docker":
		return dockerCommand(cfg, args[1:])
	case "k8s":
		return k8sCommand(cfg, args[1:])
	case "status":
		return printRPC(cfg, "status", tokenFromEnv(), nil)
	case "ps":
		return printRPC(cfg, "ps", tokenFromEnv(), nil)
	case "who":
		return whoCommand(cfg, args[1:])
	case "token":
		return tokenCommand(cfg, args[1:])
	case "bypass":
		return bypassCommand(cfg, args[1:])
	case "revoke":
		if len(args) != 2 {
			return errors.New("usage: rocguard revoke <token-or-lease-id>")
		}
		return printRPC(cfg, "revoke", tokenFromEnv(), protocol.RevokeArgs{ID: args[1]})
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func register(cfg config.Config) error {
	reader := bufio.NewReader(os.Stdin)
	rootKey, err := prompt(reader, "Root key: ")
	if err != nil {
		return err
	}
	name, err := prompt(reader, "Name: ")
	if err != nil {
		return err
	}
	ttl, err := prompt(reader, "TTL [2h]: ")
	if err != nil {
		return err
	}
	raw, err := callRPC(cfg, "register", "", protocol.RegisterArgs{RootKey: rootKey, Name: name, TTL: ttl}, false)
	if err != nil {
		return err
	}
	var result model.RegisterResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	fmt.Printf("Token: %s\nExpires at: %s\n", result.Token, result.ExpiresAt.Format(time.RFC3339))
	return nil
}

func runCommand(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gpu := fs.Int("gpu", -1, "GPU id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := fs.Args()
	if len(command) > 0 && command[0] == "--" {
		command = command[1:]
	}
	if *gpu < 0 || len(command) == 0 {
		return errors.New("usage: KEY=... rocguard run --gpu <id> -- <command>")
	}
	workdir, _ := os.Getwd()
	raw, err := callRPC(cfg, "run", requiredToken(), protocol.RunArgs{
		GPU:     *gpu,
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

func dockerCommand(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "allow" {
		return errors.New("usage: KEY=... rocguard docker allow --gpu <id> --container <name-or-id>")
	}
	fs := flag.NewFlagSet("docker allow", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gpu := fs.Int("gpu", -1, "GPU id")
	container := fs.String("container", "", "container name or id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return printRPC(cfg, "docker_allow", requiredToken(), protocol.DockerAllowArgs{GPU: *gpu, Container: *container})
}

func k8sCommand(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "allow" {
		return errors.New("usage: KEY=... rocguard k8s allow --gpu <id> --namespace <name>")
	}
	fs := flag.NewFlagSet("k8s allow", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gpu := fs.Int("gpu", -1, "GPU id")
	namespace := fs.String("namespace", "", "namespace")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	return printRPC(cfg, "k8s_allow", requiredToken(), protocol.K8sAllowArgs{GPU: *gpu, Namespace: *namespace})
}

func whoCommand(cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	gpu := fs.Int("gpu", -1, "GPU id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *gpu < 0 {
		return errors.New("usage: rocguard who --gpu <id>")
	}
	return printRPC(cfg, "who", tokenFromEnv(), protocol.WhoArgs{GPU: *gpu})
}

func tokenCommand(cfg config.Config, args []string) error {
	if len(args) != 1 || args[0] != "info" {
		return errors.New("usage: rocguard token info")
	}
	return printRPC(cfg, "token_info", requiredToken(), protocol.TokenInfoArgs{})
}

func bypassCommand(cfg config.Config, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return errors.New("usage: sudo rocguard bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>")
	}
	fs := flag.NewFlagSet("bypass add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pid := fs.Int("pid", 0, "pid")
	command := fs.String("command", "", "absolute command path")
	uid := fs.Int("uid", 0, "uid")
	ttl := fs.String("ttl", "2h", "ttl")
	reason := fs.String("reason", "", "reason")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	kind := model.BypassPID
	if *command != "" {
		kind = model.BypassCommand
	}
	return printRPC(cfg, "bypass_add", tokenFromEnv(), protocol.BypassAddArgs{
		Type:    kind,
		PID:     *pid,
		Command: *command,
		UID:     *uid,
		TTL:     *ttl,
		Reason:  *reason,
	})
}

func printRPC(cfg config.Config, method, token string, args any) error {
	raw, err := callRPC(cfg, method, token, args, false)
	if err != nil {
		return err
	}
	var pretty any
	if err := json.Unmarshal(raw, &pretty); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	out, _ := json.MarshalIndent(pretty, "", "  ")
	fmt.Println(string(out))
	return nil
}

func callRPC(cfg config.Config, method, token string, args any, stream bool) (json.RawMessage, error) {
	conn, err := net.Dial("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", cfg.SocketPath, err)
	}
	defer conn.Close()
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
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return nil, err
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

func prompt(reader *bufio.Reader, label string) (string, error) {
	fmt.Print(label)
	value, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func requiredToken() string {
	token := tokenFromEnv()
	if token == "" {
		fmt.Fprintln(os.Stderr, "rocguard: KEY token is required")
		os.Exit(1)
	}
	return token
}

func tokenFromEnv() string {
	return os.Getenv("KEY")
}

func usage() {
	fmt.Print(`rocguard commands:
  rocguard daemon
  sudo rocguard show-key
  rocguard register
  KEY=... rocguard run --gpu <id> -- <command>
  KEY=... rocguard docker allow --gpu <id> --container <name-or-id>
  KEY=... rocguard k8s allow --gpu <id> --namespace <name>
  rocguard status
  rocguard ps
  rocguard who --gpu <id>
  rocguard token info
  sudo rocguard bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>
  sudo rocguard revoke <token-or-lease-id>
`)
}
