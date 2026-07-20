# RocGuard development guide

This guide runs a development node and web gateway on a host that may already
run the production RocGuard daemon. The development environment uses separate
ports, state, keys, sockets, cgroups, Docker project names, and web storage.

This is the only supported plaintext/no-TLS workflow. Both development APIs
use HTTP and bind browser access to host loopback. Production must instead
follow the [production installation](README.md#production-installation), with
HTTPS for both node and gateway.

The development daemon must use `--dry-run`. Never run two enforcing daemons
against the same GPUs. The production daemon can still enforce its own policy
against GPU workloads started during development, so use this setup to develop
the API and UI—not to test real kill behavior. Test enforcement on a dedicated
node or during a maintenance window.

## Requirements

- The toolchains listed in [README.md](README.md#requirements)
- Docker Engine with the Compose plugin
- A production daemon on port `8192`, if one is already running
- Ports `8193` and `18080` available for development

All commands below run from the repository root.

## 1. Build and test

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go test ./...
go build -buildvcs=false -o rocguard ./cmd/rocguard
```

## 2. Create isolated development state

```bash
mkdir -p .dev
chmod 0700 .dev

umask 077
test -f .dev/root.key || printf 'rk_%s\n' "$(openssl rand -hex 32)" > .dev/root.key

WEB_PASSWORD="$(openssl rand -hex 32)"
printf 'ROCGUARD_WEB_USER=admin\nROCGUARD_WEB_PASSWORD=%s\nROCGUARD_WEB_ALLOW_REGISTRATION=1\n' \
  "$WEB_PASSWORD" > .dev/web.env
printf 'Development admin password: %s\n' "$WEB_PASSWORD"
unset WEB_PASSWORD

sudo install -d -o 65532 -g 65532 -m 0700 .dev/web
```

The repository ignores `.dev/`. Do not copy its root key, admin password,
session key, users, or registered-node secrets into source control.

## 3. Start the development daemon

Leave the production daemon on its existing paths and port `8192`. In a
separate terminal, start the development daemon on port `8193`:

```bash
sudo env \
  ROCGUARD_NODE_ADDR=0.0.0.0:8193 \
  ROCGUARD_NODE_ALLOW_INSECURE=1 \
  ROCGUARD_SOCKET="$PWD/.dev/rocguard.sock" \
  ROCGUARD_STATE="$PWD/.dev/state.json" \
  ROCGUARD_NODE_ID="$PWD/.dev/node.id" \
  ROCGUARD_TELEMETRY_DIR="$PWD/.dev/telemetry" \
  ROCGUARD_ROOT_KEY="$PWD/.dev/root.key" \
  ROCGUARD_AUDIT_LOG="$PWD/.dev/audit.log" \
  ROCGUARD_CGROUP_ROOT=/sys/fs/cgroup/rocguard-dev \
  ./rocguard daemon --dry-run
```

Keep that terminal open. Confirm which processes and ports are active:

```bash
ps -eo pid,user,args | grep '[r]ocguard.*daemon'
sudo ss -lntp | grep -E ':8192|:8193'
```

The development API is plaintext and binds all host interfaces so the Docker
bridge can reach it. Block port `8193` from external networks with the host
firewall, and stop the development daemon when it is not in use.

Expected separation:

| Resource | Production | Development |
| --- | --- | --- |
| Node API | `8192` | `8193` |
| Socket | `/run/rocguard.sock` | `.dev/rocguard.sock` |
| State | `/var/lib/rocguard/state.json` | `.dev/state.json` |
| Node ID | `/var/lib/rocguard/node.id` | `.dev/node.id` |
| Telemetry outbox | `/var/lib/rocguard/telemetry` | `.dev/telemetry` |
| Root key | `/var/lib/rocguard/root.key` | `.dev/root.key` |
| Cgroup | `/sys/fs/cgroup/rocguard` | `/sys/fs/cgroup/rocguard-dev` |
| Enforcement | enabled | `--dry-run` |

## 4. Start the development web gateway

The development Compose file builds the same hardened non-root image as the
deployment files, but uses a separate Docker project, local `.dev/web` state,
and loopback port `18080`. It also maps `host.docker.internal` to the Linux
Docker host so the container can reach the development node API.

```bash
sudo docker compose -f compose.web-dev.yml up -d --build
sudo docker compose -f compose.web-dev.yml ps
sudo docker compose -f compose.web-dev.yml logs --tail=100 gateway
```

Open [http://127.0.0.1:18080](http://127.0.0.1:18080). For development on a
remote server, keep the Compose port bound to loopback and create an SSH tunnel
from your workstation:

```bash
ssh -L 18080:127.0.0.1:18080 <development-host>
```

Then open `http://127.0.0.1:18080` on the workstation.

## 5. Register the development node

Read the isolated development root key:

```bash
cat .dev/root.key
```

Add this node in the web gateway:

```text
Name: local-development
Endpoint API: http://host.docker.internal:8193
Root key: contents of .dev/root.key
Skip TLS verify: disabled
```

Do not use `0.0.0.0` or `127.0.0.1` as the endpoint. Inside the web container,
those addresses refer to the container itself. `0.0.0.0` is only a server bind
address.

If registration reports `connection refused`, verify that the development
daemon terminal is still running and listening on `8193`. An HTTP `401`
response means the network path works but the supplied root key is wrong.

## 6. Use the development CLI

Point local CLI commands at the isolated development socket:

```bash
sudo env ROCGUARD_SOCKET="$PWD/.dev/rocguard.sock" ./rocguard status
sudo env ROCGUARD_SOCKET="$PWD/.dev/rocguard.sock" ./rocguard ps
```

Do not use the development socket or root key for production operations.

## 7. Stop and reset

Stop the web gateway:

```bash
sudo docker compose -f compose.web-dev.yml down
```

Stop the development daemon with `Ctrl-C` in its terminal. The production
daemon remains running because it has a separate process, socket, state, and
port.

To start again with the same development accounts and registered nodes, keep
`.dev/`. To reset, first stop both development components, then remove only the
development directory:

```bash
sudo rm -rf .dev
```

Never remove `/var/lib/rocguard`, `/run/rocguard.sock`, or the production
service while resetting this development environment.
