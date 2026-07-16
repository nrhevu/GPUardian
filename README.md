# RocGuard

RocGuard is a lightweight AMD GPU reservation and enforcement tool for shared
Linux servers.

It provides:

- a root daemon: `rocguard daemon`
- a user CLI: `rocguard run`, `rocguard allow`, `rocguard status`
- an optional web gateway: `rocguard web`
- scheduled reservation keys and flexible claim keys

RocGuard is monitor-kill enforcement, not kernel device isolation. Users with
root, sudo, or root-equivalent Docker access can bypass it.

## Requirements

- Linux with cgroup v2
- AMD ROCm tooling with `amd-smi`
- Go 1.22+
- Node.js and npm for building the web UI
- Root access for the daemon
- Optional: Docker, `crictl`, or `kubectl` for container scopes

## Build

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go build -buildvcs=false -o rocguard ./cmd/rocguard
```

## Core Ideas

### Reserve

Use a reserved key when you know the GPU and time window in advance.

- Pick one or more GPUs.
- Pick `Start` and `End`.
- RocGuard returns a key.
- The key only works during that reservation window.
- Max reservation duration is 8 hours.

Run with:

```bash
KEY=rg_xxx rocguard run -- python train.py
```

### Claim

Use a claim key for flexible workflows without a fixed schedule.

- The key does not expire by schedule.
- When an authorized process starts using GPU memory, RocGuard claims that GPU.
- Other unauthorized processes on the claimed GPU are killed.
- If the GPU is already busy before the claim starts, RocGuard rejects the new
  claimed process and leaves the existing workload alone.

### Run vs Allow

Use `rocguard run` for normal commands:

```bash
KEY=rg_xxx rocguard run -- torchrun --nproc_per_node=8 train.py
```

Use `rocguard allow` when another system starts the workload:

```bash
KEY=rg_xxx rocguard allow docker --container trainer
KEY=rg_xxx rocguard allow k8s --namespace training
KEY=rg_xxx rocguard allow user --name alice
```

Do not rely on `rocguard run -- docker run ...` for Docker workloads. Docker
puts the real workload in a different container cgroup, so use
`rocguard allow docker` instead.

## Root Key

The root key is the admin secret for the local daemon. Regular users should not
need it.

Default path:

```text
/var/lib/rocguard/root.key
```

Create it once:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/rocguard
sudo sh -c 'umask 077; test -f /var/lib/rocguard/root.key || printf "rk_%s\n" "$(openssl rand -hex 32)" > /var/lib/rocguard/root.key'
sudo chmod 600 /var/lib/rocguard/root.key
```

Read it as an admin:

```bash
sudo cat /var/lib/rocguard/root.key
```

If `ROCGUARD_ROOT_KEY` is set, use that file path instead.

## Installation

The node daemon runs as root. One web gateway can manage one or more nodes, and
browsers connect only to the gateway.

### 1. Build

From the repository root:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go build -buildvcs=false -o rocguard ./cmd/rocguard
```

### 2. Install the binary and UI

```bash
sudo install -o root -g root -m 0755 rocguard /usr/local/bin/rocguard
sudo install -d -o root -g root -m 0755 /etc/rocguard
sudo install -d -o root -g root -m 0700 /var/lib/rocguard
sudo install -d -o root -g root -m 0755 /var/log/rocguard
sudo install -d -o root -g root -m 0755 /usr/local/share/rocguard/ui
sudo cp -a web/ui/dist/. /usr/local/share/rocguard/ui/
```

### 3. Create the node root key

Create this once on every GPU node. Do not share it with regular users.

```bash
sudo sh -c 'umask 077; test -f /var/lib/rocguard/root.key || printf "rk_%s\n" "$(openssl rand -hex 32)" > /var/lib/rocguard/root.key'
sudo chmod 600 /var/lib/rocguard/root.key
```

Read it later with:

```bash
sudo cat /var/lib/rocguard/root.key
```

### 4. Install the node daemon

Create `/etc/systemd/system/rocguard.service`:

```ini
[Unit]
Description=RocGuard daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
Environment=ROCGUARD_SOCKET=/run/rocguard.sock
Environment=ROCGUARD_STATE=/var/lib/rocguard/state.json
Environment=ROCGUARD_ROOT_KEY=/var/lib/rocguard/root.key
Environment=ROCGUARD_AUDIT_LOG=/var/log/rocguard/audit.log
Environment=ROCGUARD_NODE_ADDR=0.0.0.0:8192
ExecStart=/usr/local/bin/rocguard daemon
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable and verify it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now rocguard
sudo systemctl status rocguard --no-pager
rocguard status
```

The Unix socket `/run/rocguard.sock` is available to local users. The node API
on port `8192` requires the root key as a bearer token. Allow this port only
from the gateway host.

For a node API outside a trusted private network, add both TLS variables to the
service and use an `https://` endpoint in the gateway:

```ini
Environment=ROCGUARD_NODE_TLS_CERT=/etc/rocguard/tls.crt
Environment=ROCGUARD_NODE_TLS_KEY=/etc/rocguard/tls.key
```

### 5. Install the web gateway

Skip this step on nodes that will be managed by a gateway elsewhere. Create the
gateway credentials file:

```bash
sudo install -o root -g root -m 0600 /dev/null /etc/rocguard/web.env
sudoedit /etc/rocguard/web.env
```

Add the initial admin account:

```text
ROCGUARD_WEB_USER=admin
ROCGUARD_WEB_PASSWORD=change-me
```

Quote passwords that contain spaces, for example
`ROCGUARD_WEB_PASSWORD="nexus titan"`.

Create `/etc/systemd/system/rocguard-web.service`:

```ini
[Unit]
Description=RocGuard web gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
EnvironmentFile=/etc/rocguard/web.env
Environment=ROCGUARD_WEB_ADDR=0.0.0.0:8080
Environment=ROCGUARD_WEB_USERS=/var/lib/rocguard/web-users.json
Environment=ROCGUARD_WEB_REGISTRY=/var/lib/rocguard/web-servers.json
Environment=ROCGUARD_WEB_UI_DIR=/usr/local/share/rocguard/ui
ExecStart=/usr/local/bin/rocguard web
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

Enable it:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now rocguard-web
sudo systemctl status rocguard-web --no-pager
```

Open `http://<gateway-host>:8080`. If a host firewall is enabled, allow TCP
port `8080` from the networks that should access the UI. Do not expose node port
`8192` to all users.

The first admin is created from `ROCGUARD_WEB_USER` and
`ROCGUARD_WEB_PASSWORD` only when the users file is empty. Admin can add nodes
and manage users. Regular users can manage only their own keys and
reservations. Add each node with:

```text
Name: a display name
Endpoint API: http://<node-host>:8192
Root key: contents of /var/lib/rocguard/root.key on that node
```

The gateway registry and users files use mode `0600` because they contain node
root keys and password hashes.

## Upgrade

Build the new version, replace the installed files, then restart both services:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go build -buildvcs=false -o rocguard ./cmd/rocguard
sudo install -o root -g root -m 0755 rocguard /usr/local/bin/rocguard
sudo cp -a web/ui/dist/. /usr/local/share/rocguard/ui/
sudo systemctl restart rocguard rocguard-web
```

Existing state, root keys, users, and registered servers remain in
`/var/lib/rocguard`.

## Uninstall

Stop the services and remove the installed program files:

```bash
sudo systemctl disable --now rocguard-web rocguard
sudo rm -f /etc/systemd/system/rocguard-web.service
sudo rm -f /etc/systemd/system/rocguard.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/rocguard
sudo rm -rf /usr/local/share/rocguard
sudo rm -f /run/rocguard.sock
```

The commands above keep configuration, users, keys, reservations, and logs so
RocGuard can be installed again later. To remove all RocGuard data permanently:

```bash
sudo rm -rf /etc/rocguard
sudo rm -rf /var/lib/rocguard
sudo rm -rf /var/log/rocguard
```

Also remove any firewall rules created for ports `8080` or `8192`.

## Quick Start

Start the daemon:

```bash
sudo ./rocguard daemon
```

Register a scheduled reservation key:

```bash
./rocguard register --reserved
```

Register a claim key:

```bash
./rocguard register --claimed
```

Run a command:

```bash
KEY=rg_xxx ./rocguard run -- python train.py
```

Check state:

```bash
./rocguard status
./rocguard ps
KEY=rg_xxx ./rocguard token info
ROOT_KEY=rk_xxx ./rocguard show-keys
```

## Docker, Kubernetes, and User Scopes

Docker:

```bash
KEY=rg_xxx rocguard allow docker --container trainer
KEY=rg_xxx rocguard allow docker --container 'trainer-*'
```

For exact Docker names, the container must already exist so RocGuard can resolve
it to an immutable container ID. Wildcards are matched against container names
during enforcement.

Kubernetes:

```bash
KEY=rg_xxx rocguard allow k8s --namespace training
KEY=rg_xxx rocguard allow k8s --namespace 'training-*'
```

User:

```bash
KEY=rg_xxx rocguard allow user --name alice
KEY=rg_xxx rocguard allow user --name 'team-*'
```

Keep `allow` scopes as narrow as possible.

## Admin Commands

Show stored keys:

```bash
ROOT_KEY=rk_xxx rocguard show-keys
```

Revoke a token, reservation, authorization, or bypass:

```bash
ROOT_KEY=rk_xxx rocguard revoke <id>
```

Bypass a trusted PID:

```bash
ROOT_KEY=rk_xxx rocguard bypass add --pid 1234 --ttl 24h --reason gpuagent
```

Bypass a trusted command for one UID:

```bash
ROOT_KEY=rk_xxx rocguard bypass add --command /usr/bin/gpuagent --uid 0 --ttl 24h --reason gpuagent
```

## Command Reference

```text
rocguard help
rocguard daemon [--dry-run]
rocguard web [--addr <host:port>] [--registry <path>] [--ui-dir <path>]
rocguard register (--reserved | --claimed)
KEY=... rocguard run -- <command>
KEY=... rocguard allow docker --container <name-or-id>
KEY=... rocguard allow k8s --namespace <name>
KEY=... rocguard allow user --name <name>
rocguard status
rocguard ps
KEY=... rocguard token info
ROOT_KEY=... rocguard show-keys
ROOT_KEY=... rocguard bypass add (--pid <pid> | --command <path> --uid <uid>) --ttl <duration> --reason <text>
ROOT_KEY=... rocguard revoke <id>
```

## Configuration

Common environment variables:

```text
ROCGUARD_SOCKET=/run/rocguard.sock
ROCGUARD_STATE=/var/lib/rocguard/state.json
ROCGUARD_ROOT_KEY=/var/lib/rocguard/root.key
ROCGUARD_AUDIT_LOG=/var/log/rocguard/audit.log
ROCGUARD_NODE_ADDR=
ROCGUARD_NODE_TLS_CERT=
ROCGUARD_NODE_TLS_KEY=
ROCGUARD_WEB_ADDR=127.0.0.1:8080
ROCGUARD_WEB_USER=admin
ROCGUARD_WEB_PASSWORD=
ROCGUARD_WEB_USERS=/var/lib/rocguard/web-users.json
ROCGUARD_WEB_REGISTRY=/var/lib/rocguard/web-servers.json
ROCGUARD_WEB_UI_DIR=web/ui/dist
ROCGUARD_GPU_COUNT=0
ROCGUARD_DRY_RUN=0
```

Development-only overrides:

```text
ROCGUARD_CGROUP_ROOT
ROCGUARD_PROC_ROOT
```

## Development

Run tests:

```bash
GOCACHE=/tmp/rocguard-go-build go test ./...
```

Build the UI:

```bash
npm --prefix web/ui run build
```

Build the CLI:

```bash
GOCACHE=/tmp/rocguard-go-build go build -buildvcs=false -o rocguard ./cmd/rocguard
```

## Safety Notes

- Do not test on production GPUs with active workloads unless you are ready for
  unauthorized processes to be killed.
- Keep Docker socket access restricted. The `docker` group is effectively
  root-equivalent.
- Do not share the root key with regular users.
- Revoke keys and reservations that are no longer needed.
