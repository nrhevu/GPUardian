<!-- markdownlint-disable MD001 MD041 -->
<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/nrhevu/GPUardian/main/web/ui/public/gpuardian-logo.svg">
    <img alt="GPUardian" src="https://raw.githubusercontent.com/nrhevu/GPUardian/main/web/ui/public/gpuardian-logo.svg" width=25%>
  </picture>
</p>

<h3 align="center">
Fair GPU access on shared servers
</h3>

---

## About 

GPUardian reserves and enforces access to AMD and NVIDIA GPUs on shared Linux
servers. It provides:

- a root node daemon that observes GPU processes and enforces reservations;
- a CLI for running and authorizing workloads; and
- a Dockerized web gateway for accounts, scheduling, keys, and multiple nodes.

GPUardian uses monitor-and-kill enforcement; it is not kernel-level device
isolation. A user with root, sudo, or root-equivalent Docker access can bypass
it.

This README is the complete production installation and user guide. Production
requires HTTPS for both the node API and web gateway. For a production install
without TLS on a trusted network, see
[docs/NO-TLS-INSTALL.md](docs/NO-TLS-INSTALL.md). For an isolated no-TLS
development environment, use [DEVELOPMENT.md](DEVELOPMENT.md).

## Deployment layout

| Component | Where it runs | Manager | Port |
| --- | --- | --- | --- |
| `gpuardian daemon` | Every AMD or NVIDIA GPU node | systemd | HTTPS `8192` |
| GPUardian web gateway | One gateway host | Docker Compose | HTTPS `8443` |

The node daemon must run directly on the host because it reads `/proc`, uses
the GPU vendor's SMI tooling, manages cgroups, and launches workloads. Only
the web gateway runs in Docker.

## Requirements

- Linux with cgroup v2 and systemd on every GPU node
- GPU vendor tooling on every GPU node:
  - AMD: ROCm tooling with a working `amd-smi` command
  - NVIDIA: the NVIDIA driver with a working `nvidia-smi` command
- Root access on the GPU nodes and gateway host
- Docker Engine with the Compose plugin on the gateway host
- OpenSSL
- Go 1.25 or newer and Node.js LTS with npm for local builds
- A CA capable of issuing TLS server certificates

Use the latest security-patched Go and Node.js releases supported by your
organization. The minimum Go version is also recorded in `go.mod`.

Before starting, choose stable DNS names or IP addresses for:

- every GPU node, for example `gpu-node-01.example.com`; and
- the gateway, for example `gpuardian.example.com`.

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
go build -buildvcs=false -o gpuardian ./cmd/gpuardian
sudo docker compose -f compose.web.yml build
```

On every GPU node, from a repository checkout for the same revision, build and
install the daemon and CLI:

```bash
go test ./...
go build -buildvcs=false -o gpuardian ./cmd/gpuardian
sudo install -o root -g root -m 0755 gpuardian /usr/local/bin/gpuardian
sudo install -d -o root -g root -m 0755 /etc/gpuardian
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
/tmp/gpuardian-install/node.crt
/tmp/gpuardian-install/node.key
```

On the gateway host:

```text
/tmp/gpuardian-install/web.crt
/tmp/gpuardian-install/web.key
/tmp/gpuardian-install/gpuardian-ca.crt
```

Install each node certificate and key on its node with this block:

```bash
sudo test -s /tmp/gpuardian-install/node.crt
sudo test -s /tmp/gpuardian-install/node.key
sudo install -d -o root -g root -m 0755 /etc/gpuardian
sudo install -o root -g root -m 0644 \
  /tmp/gpuardian-install/node.crt /etc/gpuardian/node.crt
sudo install -o root -g root -m 0600 \
  /tmp/gpuardian-install/node.key /etc/gpuardian/node.key
```

Install the CA that issued the node certificates in the gateway host's system
trust store. On Debian or Ubuntu, paste:

```bash
sudo test -s /tmp/gpuardian-install/gpuardian-ca.crt
sudo install -o root -g root -m 0644 \
  /tmp/gpuardian-install/gpuardian-ca.crt \
  /usr/local/share/ca-certificates/gpuardian-ca.crt
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
sudo install -d -o root -g root -m 0700 /var/lib/gpuardian
sudo install -d -o root -g root -m 0700 /var/log/gpuardian
sudo sh -c 'umask 077; test -f /var/lib/gpuardian/root.key || printf "rk_%s\n" "$(openssl rand -hex 32)" > /var/lib/gpuardian/root.key'
sudo chmod 0600 /var/lib/gpuardian/root.key
sudo tee /etc/systemd/system/gpuardian.service >/dev/null <<'EOF'
[Unit]
Description=GPUardian daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
Environment=GPUARDIAN_SOCKET=/run/gpuardian.sock
Environment=GPUARDIAN_STATE=/var/lib/gpuardian/state.json
Environment=GPUARDIAN_NODE_ID=/var/lib/gpuardian/node.id
Environment=GPUARDIAN_TELEMETRY_DIR=/var/lib/gpuardian/telemetry
Environment=GPUARDIAN_ROOT_KEY=/var/lib/gpuardian/root.key
Environment=GPUARDIAN_AUDIT_LOG=/var/log/gpuardian/audit.log
Environment=GPUARDIAN_NODE_ADDR=0.0.0.0:8192
Environment=GPUARDIAN_NODE_TLS_CERT=/etc/gpuardian/node.crt
Environment=GPUARDIAN_NODE_TLS_KEY=/etc/gpuardian/node.key
ExecStart=/usr/local/bin/gpuardian daemon
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
sudo systemctl enable --now gpuardian
sudo systemctl status gpuardian --no-pager
sudo gpuardian status
sudo ss -lntp | grep ':8192'
```

Use the host firewall or security group to allow TCP `8192` only from the
gateway host. Do not expose the node API to user networks or the public
Internet.

Do not add systemd restrictions such as `PrivateDevices`,
`ProtectControlGroups`, `ProtectHome`, `ProtectSystem`, or a service-wide
restrictive `UMask` without testing `gpuardian run` and GPU enforcement end to
end. These settings also affect workloads launched by the daemon.

### 4. Install the web gateway

Run this section once on the gateway host from the repository root.

The following block validates and installs the staged gateway certificates,
creates persistent state, generates the first administrator password, enables
regular-user self-registration, and starts the gateway:

```bash
sudo test -s /tmp/gpuardian-install/web.crt
sudo test -s /tmp/gpuardian-install/web.key
sudo install -d -o root -g root -m 0755 /etc/gpuardian
sudo install -d -o 65532 -g 65532 -m 0700 /var/lib/gpuardian-web
sudo install -o root -g root -m 0600 /dev/null /etc/gpuardian/web.env
sudo install -o root -g root -m 0644 \
  /tmp/gpuardian-install/web.crt /etc/gpuardian/web.crt
sudo install -o root -g 65532 -m 0640 \
  /tmp/gpuardian-install/web.key /etc/gpuardian/web.key
WEB_PASSWORD="$(openssl rand -hex 32)"
printf 'GPUARDIAN_WEB_USER=admin\nGPUARDIAN_WEB_PASSWORD=%s\nGPUARDIAN_WEB_ALLOW_REGISTRATION=1\n' \
  "$WEB_PASSWORD" | sudo tee /etc/gpuardian/web.env >/dev/null
printf 'Initial GPUardian admin password: %s\n' "$WEB_PASSWORD"
unset WEB_PASSWORD
sudo docker compose -f compose.web.yml up -d
sudo docker compose -f compose.web.yml ps
sudo docker compose -f compose.web.yml logs --tail=100 gateway
```

Store the displayed password in an approved password manager. To disable
self-registration later, paste:

```bash
sudo sed -i 's/^GPUARDIAN_WEB_ALLOW_REGISTRATION=.*/GPUARDIAN_WEB_ALLOW_REGISTRATION=0/' \
  /etc/gpuardian/web.env
sudo docker compose -f compose.web.yml up -d --force-recreate
```

Allow TCP `8443` only from networks that should access GPUardian. Open:

```text
https://<gateway-host>:8443
```

Sign in as `admin`, immediately change the generated password, then remove the
bootstrap password from the container environment:

```bash
sudo sed -i '/^GPUARDIAN_WEB_PASSWORD=/d' /etc/gpuardian/web.env
sudo docker compose -f compose.web.yml up -d --force-recreate
```

### 5. Register every GPU node

On each GPU node, read its root key:

```bash
sudo cat /var/lib/gpuardian/root.key
```

Treat this value as an administrator secret. In the web gateway, open `Nodes`,
select `Add node`, and enter:

```text
Name: gpu-node-01
Endpoint API: https://gpu-node-01.example.com:8192
Root key: contents of /var/lib/gpuardian/root.key on that node
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

### 6. (Optional) Put TLS in front with a reverse proxy

If you want encryption for user-to-gateway traffic without managing GPUardian's
own certificates, place a reverse proxy in front of the gateway:

```
User --HTTPS--> nginx :443 --HTTP--> gateway :8443 (loopback)
```

Example nginx snippet (terminate TLS, proxy to the gateway on loopback):

```nginx
server {
    listen 443 ssl;
    server_name gpuardian.example.com;

    ssl_certificate     /etc/nginx/ssl/gpuardian.crt;
    ssl_certificate_key /etc/nginx/ssl/gpuardian.key;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    }
}
```

Set `GPUARDIAN_WEB_TRUST_PROXY=1` in `web.env` so the gateway honors the
`X-Forwarded-*` headers, and bind the gateway to loopback only by changing
`GPUARDIAN_WEB_ADDR` to `127.0.0.1:8443` in the Compose file. The node API
(`8192`) should still be restricted to the gateway host via firewall; if the
gateway and nodes are on different hosts, use a VPN or private network for that
traffic.

### 7. Protect gateway state

Back up these files as secrets:

```text
/var/lib/gpuardian-web/session.key
/var/lib/gpuardian-web/user-key.key
/var/lib/gpuardian-web/servers.json
/var/lib/gpuardian-web/users.json
```

Reservation history is stored separately in
`/var/lib/gpuardian-web/history.db`. While the gateway is running, back it up
with SQLite's Online Backup API or `VACUUM INTO`; do not copy only the live
database file because committed data may still be in the WAL. A filesystem
copy is safe after the gateway has been stopped cleanly. Restore the database
only while the gateway is stopped, then preserve ownership UID/GID `65532` and
mode `0600`. Keep this directory on a local filesystem; NFS is not supported.

They must remain owned by UID/GID `65532` with mode `0600`. Losing
`session.key` signs out all browser sessions. Losing `user-key.key` makes the
encrypted fixed keys in `users.json` unrecoverable, so those two files must be
backed up and restored together. Never place these files, node root keys,
certificate private keys, or `/etc/gpuardian/web.env` in source control.

## Using GPUardian

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
6. Open `Key` and copy your fixed key if you have not saved it already.

Each account has one fixed key shared by every synchronized node and every
reservation. The key uses the account's reservation entitlement on reserved
GPUs and can claim a currently unreserved GPU. Reserving another window does
not create or change the key. Keep it private; use `Regenerate` only when the
credential must be replaced, because the previous version stops working after
the managed-key snapshot reaches each node.

### Run a workload

Run normal commands through the GPUardian wrapper:

```bash
KEY=gk_xxx gpuardian run -- python train.py
KEY=gk_xxx gpuardian run -- torchrun --nproc_per_node=8 train.py
```

Everything after `--` is the workload command. Child processes inherit the
authorization.

Do not use `gpuardian run -- docker run ...`; Docker places the real workload
in a different cgroup. Authorize the container instead:

```bash
KEY=gk_xxx gpuardian allow docker --container trainer
```

Other exact authorization scopes:

```bash
KEY=gk_xxx gpuardian allow k8s --namespace training
KEY=gk_xxx gpuardian allow user --name alice
```

Use the narrowest exact scope possible. Wildcard scopes are admin-only because
they can authorize more workloads than intended.

### Inspect status and keys

```bash
KEY=gk_xxx gpuardian status
KEY=gk_xxx gpuardian ps
KEY=gk_xxx gpuardian token info
```

In the web `Key` tab:

- `Show key` reveals your fixed key; administrators can reveal a user's key.
- `Regenerate` replaces the fixed key and invalidates its previous version.
- Node badges show whether the current key snapshot has synchronized.

Revoking a reservation ends only that reservation. It does not change the
account's fixed key.

Regular users never need a node root key.

## Administration

### Root key

The root key is the node's highest-privilege secret:

```bash
sudo cat /var/lib/gpuardian/root.key
```

Use a different root key on every node. Never expose it in shell history,
logs, screenshots, or user documentation.

### CLI reference

```text
gpuardian help
gpuardian daemon [--dry-run]
gpuardian register (--reserved | --claimed)
KEY=... gpuardian run -- <command>
KEY=... gpuardian allow docker --container <name-or-id>
KEY=... gpuardian allow k8s --namespace <name>
KEY=... gpuardian allow user --name <name>
KEY=... gpuardian status
KEY=... gpuardian ps
KEY=... gpuardian token info
ROOT_KEY=... gpuardian show-keys
ROOT_KEY=... gpuardian bypass add --pid <pid> --ttl <duration> --reason <text>
ROOT_KEY=... gpuardian bypass add --command <path> --uid 0 --ttl <duration> --reason <text>
ROOT_KEY=... gpuardian revoke <id>
```

Command-path bypasses are restricted to UID `0` because unprivileged mount
namespaces can spoof executable paths. Prefer a PID bypass when possible.

### Node configuration

```text
GPUARDIAN_SOCKET=/run/gpuardian.sock
GPUARDIAN_STATE=/var/lib/gpuardian/state.json
GPUARDIAN_NODE_ID=/var/lib/gpuardian/node.id
GPUARDIAN_TELEMETRY_DIR=/var/lib/gpuardian/telemetry
GPUARDIAN_ROOT_KEY=/var/lib/gpuardian/root.key
GPUARDIAN_AUDIT_LOG=/var/log/gpuardian/audit.log
GPUARDIAN_NODE_ADDR=0.0.0.0:8192
GPUARDIAN_NODE_TLS_CERT=/etc/gpuardian/node.crt
GPUARDIAN_NODE_TLS_KEY=/etc/gpuardian/node.key
GPUARDIAN_NODE_ALLOW_INSECURE=0
GPUARDIAN_GPU_COUNT=0
GPUARDIAN_GPU_VENDOR=auto
GPUARDIAN_DRY_RUN=0
```

`GPUARDIAN_GPU_VENDOR` selects the GPU SMI backend the daemon uses to sample
processes and telemetry. `auto` (the default) probes for `amd-smi` first and
falls back to `nvidia-smi`; set it explicitly to `amd` or `nvidia` to skip
probing, which is recommended on nodes where only one vendor's tooling is
installed.

Production Compose owns all web listener, TLS, cookie, state-path, and UI
settings. `/etc/gpuardian/web.env` should contain only operator options:

```text
GPUARDIAN_WEB_USER=admin
GPUARDIAN_WEB_PASSWORD=<one-time-bootstrap-password>
GPUARDIAN_WEB_ALLOW_REGISTRATION=1
GPUARDIAN_WEB_DB=/var/lib/gpuardian-web/history.db
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

The bind-mounted state in `/var/lib/gpuardian-web`, including `history.db`,
remains when the container is recreated or removed.

## Uninstall

On the gateway host, run from the repository root:

```bash
sudo docker compose -f compose.web.yml down
```

On every GPU node:

```bash
sudo systemctl disable --now gpuardian
sudo rm -f /etc/systemd/system/gpuardian.service
sudo systemctl daemon-reload
sudo rm -f /usr/local/bin/gpuardian
sudo rm -f /run/gpuardian.sock
```

These commands retain configuration, state, keys, users, and logs. To remove
all GPUardian data permanently, delete the applicable paths on the gateway and
GPU nodes:

```bash
sudo rm -rf /etc/gpuardian
sudo rm -rf /var/lib/gpuardian
sudo rm -rf /var/lib/gpuardian-web
sudo rm -rf /var/log/gpuardian
```

Also remove the firewall rules for ports `8192` and `8443`.

## Development

Development uses separate ports, keys, state, cgroups, and a no-TLS Docker
Compose project. Follow [DEVELOPMENT.md](DEVELOPMENT.md). Never use the
development configuration for production. For a production install without TLS,
see [docs/NO-TLS-INSTALL.md](docs/NO-TLS-INSTALL.md).

## License

Licensed under the Apache License 2.0. See [LICENSE](LICENSE).
