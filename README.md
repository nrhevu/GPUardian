# RocGuard

RocGuard reserves and enforces access to AMD GPUs on shared Linux servers. It
provides:

- a root node daemon that observes GPU processes and enforces reservations;
- a CLI for running and authorizing workloads; and
- a Dockerized web gateway for accounts, scheduling, keys, and multiple nodes.

RocGuard uses monitor-and-kill enforcement; it is not kernel-level device
isolation. A user with root, sudo, or root-equivalent Docker access can bypass
it.

This README is the complete production installation and user guide. Production
requires HTTPS for both the node API and web gateway. For an isolated no-TLS
development environment, use [DEVELOPMENT.md](DEVELOPMENT.md).

## Deployment layout

| Component | Where it runs | Manager | Port |
| --- | --- | --- | --- |
| `rocguard daemon` | Every AMD GPU node | systemd | HTTPS `8192` |
| RocGuard web gateway | One gateway host | Docker Compose | HTTPS `8443` |

The node daemon must run directly on the host because it reads `/proc`, uses
AMD tooling, manages cgroups, and launches workloads. Only the web gateway runs
in Docker.

## Requirements

- Linux with cgroup v2 and systemd on every GPU node
- AMD ROCm tooling with a working `amd-smi` command on every GPU node
- Root access on the GPU nodes and gateway host
- Docker Engine with the Compose plugin on the gateway host
- OpenSSL
- Go 1.25 or newer and Node.js LTS with npm for local builds
- A CA capable of issuing TLS server certificates

Use the latest security-patched Go and Node.js releases supported by your
organization. The minimum Go version is also recorded in `go.mod`.

Before starting, choose stable DNS names or IP addresses for:

- every GPU node, for example `gpu-node-01.example.com`; and
- the gateway, for example `rocguard.example.com`.

The selected names or IP addresses must appear in the corresponding TLS
certificate Subject Alternative Name (SAN).

## Production installation

Run repository commands from the repository root. Commands marked for a GPU
node must be repeated on every node. Every shell block in this installation
section can be pasted as-is on the named host. Examples in later user sections
still require your real key, workload command, container, user, or namespace.
The only required installation input is certificate material issued by your
CA.

### 1. Build and test

On the gateway host, verify the source tree and build the web image:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go test ./...
go build -buildvcs=false -o rocguard ./cmd/rocguard
sudo docker compose -f compose.web.yml build
```

On every GPU node, from a repository checkout for the same revision, build and
install the daemon and CLI:

```bash
go test ./...
go build -buildvcs=false -o rocguard ./cmd/rocguard
sudo install -o root -g root -m 0755 rocguard /usr/local/bin/rocguard
sudo install -d -o root -g root -m 0755 /etc/rocguard
```

### 2. Obtain TLS certificates

Request certificates from your organization's internal CA or a public CA. You
need the following files:

| File | Installed on | Required SAN |
| --- | --- | --- |
| `node.crt` and `node.key` | Each GPU node | That node's API DNS name or IP |
| `web.crt` and `web.key` | Gateway host | The gateway DNS name or IP |
| Issuing CA certificate | Gateway and user devices | Not applicable |

Each `.crt` should contain the server certificate followed by any required
intermediate certificates. Never copy a node's private `node.key` to the
gateway.

Have your certificate administrator place the files at these exact temporary
paths before continuing:

On every GPU node:

```text
/tmp/rocguard-install/node.crt
/tmp/rocguard-install/node.key
```

On the gateway host:

```text
/tmp/rocguard-install/web.crt
/tmp/rocguard-install/web.key
/tmp/rocguard-install/rocguard-ca.crt
```

Install each node certificate and key on its node with this block:

```bash
sudo test -s /tmp/rocguard-install/node.crt
sudo test -s /tmp/rocguard-install/node.key
sudo install -d -o root -g root -m 0755 /etc/rocguard
sudo install -o root -g root -m 0644 \
  /tmp/rocguard-install/node.crt /etc/rocguard/node.crt
sudo install -o root -g root -m 0600 \
  /tmp/rocguard-install/node.key /etc/rocguard/node.key
```

Install the CA that issued the node certificates in the gateway host's system
trust store. On Debian or Ubuntu, paste:

```bash
sudo test -s /tmp/rocguard-install/rocguard-ca.crt
sudo install -o root -g root -m 0644 \
  /tmp/rocguard-install/rocguard-ca.crt \
  /usr/local/share/ca-certificates/rocguard-ca.crt
sudo update-ca-certificates
```

The production Compose file mounts the gateway host's
`/etc/ssl/certs/ca-certificates.crt` into the container. On another Linux
distribution, update the Compose mount to that distribution's generated CA
bundle path.

Install the gateway CA in each user's browser or operating-system trust store.
Keep `Skip TLS verify` disabled when registering production nodes.

### 3. Install the node daemon

Run this section on every GPU node.

The following single block creates the private state, generates a unique root
key, writes the systemd unit, starts the daemon, and verifies it:

```bash
sudo install -d -o root -g root -m 0700 /var/lib/rocguard
sudo install -d -o root -g root -m 0700 /var/log/rocguard
sudo sh -c 'umask 077; test -f /var/lib/rocguard/root.key || printf "rk_%s\n" "$(openssl rand -hex 32)" > /var/lib/rocguard/root.key'
sudo chmod 0600 /var/lib/rocguard/root.key
sudo tee /etc/systemd/system/rocguard.service >/dev/null <<'EOF'
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
Environment=ROCGUARD_NODE_ID=/var/lib/rocguard/node.id
Environment=ROCGUARD_TELEMETRY_DIR=/var/lib/rocguard/telemetry
Environment=ROCGUARD_ROOT_KEY=/var/lib/rocguard/root.key
Environment=ROCGUARD_AUDIT_LOG=/var/log/rocguard/audit.log
Environment=ROCGUARD_NODE_ADDR=0.0.0.0:8192
Environment=ROCGUARD_NODE_TLS_CERT=/etc/rocguard/node.crt
Environment=ROCGUARD_NODE_TLS_KEY=/etc/rocguard/node.key
ExecStart=/usr/local/bin/rocguard daemon
Restart=always
RestartSec=2
NoNewPrivileges=true
LockPersonality=true
ProtectClock=true
ProtectHostname=true
ProtectKernelLogs=true
ProtectKernelModules=true
ProtectKernelTunables=true
RestrictSUIDSGID=true
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now rocguard
sudo systemctl status rocguard --no-pager
sudo rocguard status
sudo ss -lntp | grep ':8192'
```

Use the host firewall or security group to allow TCP `8192` only from the
gateway host. Do not expose the node API to user networks or the public
Internet.

Do not add systemd restrictions such as `PrivateDevices`,
`ProtectControlGroups`, `ProtectHome`, `ProtectSystem`, or a service-wide
restrictive `UMask` without testing `rocguard run` and GPU enforcement end to
end. These settings also affect workloads launched by the daemon.

### 4. Install the web gateway

Run this section once on the gateway host from the repository root.

The following block validates and installs the staged gateway certificates,
creates persistent state, generates the first administrator password, enables
regular-user self-registration, and starts the gateway:

```bash
sudo test -s /tmp/rocguard-install/web.crt
sudo test -s /tmp/rocguard-install/web.key
sudo install -d -o root -g root -m 0755 /etc/rocguard
sudo install -d -o 65532 -g 65532 -m 0700 /var/lib/rocguard-web
sudo install -o root -g root -m 0600 /dev/null /etc/rocguard/web.env
sudo install -o root -g root -m 0644 \
  /tmp/rocguard-install/web.crt /etc/rocguard/web.crt
sudo install -o root -g 65532 -m 0640 \
  /tmp/rocguard-install/web.key /etc/rocguard/web.key
WEB_PASSWORD="$(openssl rand -hex 32)"
printf 'ROCGUARD_WEB_USER=admin\nROCGUARD_WEB_PASSWORD=%s\nROCGUARD_WEB_ALLOW_REGISTRATION=1\n' \
  "$WEB_PASSWORD" | sudo tee /etc/rocguard/web.env >/dev/null
printf 'Initial RocGuard admin password: %s\n' "$WEB_PASSWORD"
unset WEB_PASSWORD
sudo docker compose -f compose.web.yml up -d
sudo docker compose -f compose.web.yml ps
sudo docker compose -f compose.web.yml logs --tail=100 gateway
```

Store the displayed password in an approved password manager. To disable
self-registration later, paste:

```bash
sudo sed -i 's/^ROCGUARD_WEB_ALLOW_REGISTRATION=.*/ROCGUARD_WEB_ALLOW_REGISTRATION=0/' \
  /etc/rocguard/web.env
sudo docker compose -f compose.web.yml up -d --force-recreate
```

Allow TCP `8443` only from networks that should access RocGuard. Open:

```text
https://<gateway-host>:8443
```

Sign in as `admin`, immediately change the generated password, then remove the
bootstrap password from the container environment:

```bash
sudo sed -i '/^ROCGUARD_WEB_PASSWORD=/d' /etc/rocguard/web.env
sudo docker compose -f compose.web.yml up -d --force-recreate
```

### 5. Register every GPU node

On each GPU node, read its root key:

```bash
sudo cat /var/lib/rocguard/root.key
```

Treat this value as an administrator secret. In the web gateway, open `Nodes`,
select `Add node`, and enter:

```text
Name: gpu-node-01
Endpoint API: https://gpu-node-01.example.com:8192
Root key: contents of /var/lib/rocguard/root.key on that node
Skip TLS verify: disabled
```

Use a node's actual certificate DNS name or IP. Do not enter `0.0.0.0`,
`127.0.0.1`, or the web gateway port `8443`.

If registration fails:

- `connection refused`: confirm the daemon is running, port `8192` is correct,
  and the firewall permits the gateway.
- `unknown authority`: install the node's issuing CA on the gateway host and
  restart the container.
- certificate hostname error: issue a certificate whose SAN matches the
  endpoint.
- `401` or `403`: confirm the root key came from the same node.

### 6. Protect gateway state

Back up these files as secrets:

```text
/var/lib/rocguard-web/session.key
/var/lib/rocguard-web/servers.json
/var/lib/rocguard-web/users.json
```

Reservation history is stored separately in
`/var/lib/rocguard-web/history.db`. While the gateway is running, back it up
with SQLite's Online Backup API or `VACUUM INTO`; do not copy only the live
database file because committed data may still be in the WAL. A filesystem
copy is safe after the gateway has been stopped cleanly. Restore the database
only while the gateway is stopped, then preserve ownership UID/GID `65532` and
mode `0600`. Keep this directory on a local filesystem; NFS is not supported.

They must remain owned by UID/GID `65532` with mode `0600`. Losing
`session.key` signs out all browser sessions. Never place these files, node
root keys, certificate private keys, or `/etc/rocguard/web.env` in source
control.

## Using RocGuard

### Create an account and sign in

Open the gateway URL. If `Create account` is visible, choose a username and a
password containing at least 12 bytes. Self-registered accounts are always
regular users and are signed in immediately.

If account creation is disabled, ask an administrator to create the account in
the `Users` tab.

### Reserve GPUs

1. Select a node.
2. Open `Schedule`.
3. Select one or more available GPUs.
4. Choose the start and end time.
5. Enter a purpose and submit.
6. Open the reservation, select `Show key`, and copy the key.

A reserved key works only for its selected GPUs and reservation window. Keep
the key private and revoke reservations that are no longer needed.

### Run a workload

Run normal commands through the RocGuard wrapper:

```bash
KEY=rg_xxx rocguard run -- python train.py
KEY=rg_xxx rocguard run -- torchrun --nproc_per_node=8 train.py
```

Everything after `--` is the workload command. Child processes inherit the
authorization.

Do not use `rocguard run -- docker run ...`; Docker places the real workload
in a different cgroup. Authorize the container instead:

```bash
KEY=rg_xxx rocguard allow docker --container trainer
```

Other exact authorization scopes:

```bash
KEY=rg_xxx rocguard allow k8s --namespace training
KEY=rg_xxx rocguard allow user --name alice
```

Use the narrowest exact scope possible. Wildcard scopes are admin-only because
they can authorize more workloads than intended.

### Inspect status and keys

```bash
KEY=rg_xxx rocguard status
KEY=rg_xxx rocguard ps
KEY=rg_xxx rocguard token info
```

In the web `Key` tab:

- `Show key` reveals a key you own; administrators can reveal stored keys.
- `Revoke` removes a key or reservation you own; administrators can revoke any
  item.

Regular users never need a node root key.

## Administration

### Root key

The root key is the node's highest-privilege secret:

```bash
sudo cat /var/lib/rocguard/root.key
```

Use a different root key on every node. Never expose it in shell history,
logs, screenshots, or user documentation.

### CLI reference

```text
rocguard help
rocguard daemon [--dry-run]
rocguard register (--reserved | --claimed)
KEY=... rocguard run -- <command>
KEY=... rocguard allow docker --container <name-or-id>
KEY=... rocguard allow k8s --namespace <name>
KEY=... rocguard allow user --name <name>
KEY=... rocguard status
KEY=... rocguard ps
KEY=... rocguard token info
ROOT_KEY=... rocguard show-keys
ROOT_KEY=... rocguard bypass add --pid <pid> --ttl <duration> --reason <text>
ROOT_KEY=... rocguard bypass add --command <path> --uid 0 --ttl <duration> --reason <text>
ROOT_KEY=... rocguard revoke <id>
```

Command-path bypasses are restricted to UID `0` because unprivileged mount
namespaces can spoof executable paths. Prefer a PID bypass when possible.

### Node configuration

```text
ROCGUARD_SOCKET=/run/rocguard.sock
ROCGUARD_STATE=/var/lib/rocguard/state.json
ROCGUARD_NODE_ID=/var/lib/rocguard/node.id
ROCGUARD_TELEMETRY_DIR=/var/lib/rocguard/telemetry
ROCGUARD_ROOT_KEY=/var/lib/rocguard/root.key
ROCGUARD_AUDIT_LOG=/var/log/rocguard/audit.log
ROCGUARD_NODE_ADDR=0.0.0.0:8192
ROCGUARD_NODE_TLS_CERT=/etc/rocguard/node.crt
ROCGUARD_NODE_TLS_KEY=/etc/rocguard/node.key
ROCGUARD_NODE_ALLOW_INSECURE=0
ROCGUARD_GPU_COUNT=0
ROCGUARD_DRY_RUN=0
```

Production Compose owns all web listener, TLS, cookie, state-path, and UI
settings. `/etc/rocguard/web.env` should contain only operator options:

```text
ROCGUARD_WEB_USER=admin
ROCGUARD_WEB_PASSWORD=<one-time-bootstrap-password>
ROCGUARD_WEB_ALLOW_REGISTRATION=1
ROCGUARD_WEB_DB=/var/lib/rocguard-web/history.db
```

Production Compose forces browser-facing HTTP and HTTP node endpoints off.

## Web gateway operations

Run these commands from the repository root:

```bash
# Status
sudo docker compose -f compose.web.yml ps

# Logs
sudo docker compose -f compose.web.yml logs -f gateway

# Restart
sudo docker compose -f compose.web.yml restart gateway

# Stop
sudo docker compose -f compose.web.yml down

# Start
sudo docker compose -f compose.web.yml up -d
```

The bind-mounted state in `/var/lib/rocguard-web`, including `history.db`,
remains when the container is recreated or removed.

## Uninstall

On the gateway host, run from the repository root:

```bash
sudo docker compose -f compose.web.yml down
```

On every GPU node:

```bash
sudo systemctl disable --now rocguard
sudo rm -f /etc/systemd/system/rocguard.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/rocguard
sudo rm -f /run/rocguard.sock
```

These commands retain configuration, state, keys, users, and logs. To remove
all RocGuard data permanently, delete the applicable paths on the gateway and
GPU nodes:

```bash
sudo rm -rf /etc/rocguard
sudo rm -rf /var/lib/rocguard
sudo rm -rf /var/lib/rocguard-web
sudo rm -rf /var/log/rocguard
```

Also remove the firewall rules for ports `8192` and `8443`.

## Development

Development uses separate ports, keys, state, cgroups, and a no-TLS Docker
Compose project. Follow [DEVELOPMENT.md](DEVELOPMENT.md). Never use the
development configuration for production.

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE).
