# GPUardian no-TLS installation

This guide installs GPUardian **without TLS** — both the node API and the web
gateway serve plaintext HTTP. It is a shortcut for trusted networks where
provisioning TLS certificates is impractical.

> **Security warning.** Without TLS, the per-node root key (`rk_...`), user
> fixed keys (`gk_...`), login passwords, and session cookies all travel in
> plaintext over the network. Anyone who can sniff traffic between the gateway
> and a node, or between a user and the gateway, can capture these secrets and
> gain full access. Only use this guide when **one** of the following is true:
>
> - All traffic stays on `127.0.0.1` (loopback only).
> - The network is a fully trusted private VLAN with no untrusted users.
> - A reverse proxy (nginx, Traefik, Caddy) terminates TLS in front of the
>   gateway, and the node API is reachable only over a private network or VPN.
>
> For the standard TLS-protected installation, follow [the main README](../README.md).

## What changes versus the TLS install

| Setting | TLS install | No-TLS install |
| --- | --- | --- |
| Node API | HTTPS `8192` | HTTP `8192` |
| Web gateway | HTTPS `8443` | HTTP `8443` |
| `GPUARDIAN_NODE_ALLOW_INSECURE` | `0` (unset) | `1` |
| `GPUARDIAN_WEB_ALLOW_INSECURE` | `0` | `1` |
| `GPUARDIAN_WEB_ALLOW_INSECURE_NODES` | `0` | `1` |
| `GPUARDIAN_WEB_SECURE_COOKIES` | `1` (default) | `0` |
| TLS cert/key files | required | not needed |
| Node registration `Skip TLS verify` | disabled | **enabled** |

## Prerequisites

Same as the TLS install minus the CA and certificates:

- Linux with cgroup v2 and systemd on every GPU node
- GPU vendor tooling (`amd-smi` or `nvidia-smi`) on every GPU node
- Root access on the GPU nodes and gateway host
- Docker Engine with the Compose plugin on the gateway host
- OpenSSL (for generating the root key and admin password)
- Go 1.25+ and Node.js LTS with npm for local builds

Choose stable DNS names or IP addresses for every GPU node and the gateway, as
in the TLS install. You still need these for node registration and firewall
rules — they just won't appear in any certificate SAN.

## 1. Build and install the binary

On the gateway host **and** every GPU node, from the repository root:

```bash
npm --prefix web/ui ci
npm --prefix web/ui run build
go test ./...
go build -buildvcs=false -o gpuardian ./cmd/gpuardian
sudo install -o root -g root -m 0755 gpuardian /usr/local/bin/gpuardian
sudo install -d -o root -g root -m 0755 /etc/gpuardian
```

Build the gateway image on the gateway host:

```bash
sudo docker compose -f compose.web.yml build
```

## 2. Install the node daemon (no TLS)

Run on every GPU node. This block creates state, generates the root key, writes
a systemd unit **without TLS cert/key lines and with insecure mode enabled**,
starts the daemon, and verifies it:

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
Environment=GPUARDIAN_NODE_ALLOW_INSECURE=1
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

The two differences from the TLS unit are:

- `GPUARDIAN_NODE_TLS_CERT` and `GPUARDIAN_NODE_TLS_KEY` lines are **removed**.
- `GPUARDIAN_NODE_ALLOW_INSECURE=1` is **added** so the daemon accepts plaintext
  HTTP instead of refusing to start.

Use the host firewall to allow TCP `8192` only from the gateway host. Even
without TLS, restricting access to the node API is important — the root key
travels on every gateway-to-node call.

## 3. Install the web gateway (no TLS)

Run once on the gateway host from the repository root.

First, create a no-TLS Compose override file so you don't edit
`compose.web.yml` directly:

```bash
sudo tee /etc/gpuardian/compose.web-no-tls.yml >/dev/null <<'EOF'
name: gpuardian-web

services:
  gateway:
    image: gpuardian-web:local
    build:
      context: .
      dockerfile: Dockerfile.web
    restart: unless-stopped
    init: true
    user: "65532:65532"
    env_file:
      - /etc/gpuardian/web.env
    environment:
      GPUARDIAN_WEB_ADDR: 0.0.0.0:8443
      GPUARDIAN_WEB_TLS_CERT: ""
      GPUARDIAN_WEB_TLS_KEY: ""
      GPUARDIAN_WEB_ALLOW_INSECURE: "1"
      GPUARDIAN_WEB_ALLOW_INSECURE_NODES: "1"
      GPUARDIAN_WEB_SECURE_COOKIES: "0"
      GPUARDIAN_WEB_TRUST_PROXY: "0"
      GPUARDIAN_WEB_USERS: /var/lib/gpuardian-web/users.json
      GPUARDIAN_WEB_REGISTRY: /var/lib/gpuardian-web/servers.json
      GPUARDIAN_WEB_SESSION_KEY: /var/lib/gpuardian-web/session.key
      GPUARDIAN_WEB_USER_KEY: /var/lib/gpuardian-web/user-key.key
      GPUARDIAN_WEB_DB: /var/lib/gpuardian-web/history.db
      GPUARDIAN_WEB_UI_DIR: /usr/local/share/gpuardian/ui
    ports:
      - "8443:8443"
    volumes:
      - /var/lib/gpuardian-web:/var/lib/gpuardian-web
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=16m
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    pids_limit: 256
    stop_grace_period: 10s
    logging:
      driver: json-file
      options:
        max-size: 10m
        max-file: "3"
EOF
```

Key differences from `compose.web.yml`:

- `GPUARDIAN_WEB_TLS_CERT` / `GPUARDIAN_WEB_TLS_KEY` are set to empty strings.
- `GPUARDIAN_WEB_ALLOW_INSECURE` and `GPUARDIAN_WEB_ALLOW_INSECURE_NODES` are
  `"1"` so the gateway serves HTTP and allows HTTP node endpoints.
- `GPUARDIAN_WEB_SECURE_COOKIES` is `"0"` so session cookies work over HTTP.
- The `web.crt` / `web.key` volume mounts are **removed** (no cert files).
- The host CA-bundle mount is **removed** (no node CA to trust).

Then create the state directory, `web.env`, and start the gateway:

```bash
sudo install -d -o 65532 -g 65532 -m 0700 /var/lib/gpuardian-web
sudo install -o root -g root -m 0600 /dev/null /etc/gpuardian/web.env
WEB_PASSWORD="$(openssl rand -hex 32)"
printf 'GPUARDIAN_WEB_USER=admin\nGPUARDIAN_WEB_PASSWORD=%s\nGPUARDIAN_WEB_ALLOW_REGISTRATION=1\n' \
  "$WEB_PASSWORD" | sudo tee /etc/gpuardian/web.env >/dev/null
printf 'Initial GPUardian admin password: %s\n' "$WEB_PASSWORD"
unset WEB_PASSWORD
sudo docker compose -f /etc/gpuardian/compose.web-no-tls.yml up -d
sudo docker compose -f /etc/gpuardian/compose.web-no-tls.yml ps
sudo docker compose -f /etc/gpuardian/compose.web-no-tls.yml logs --tail=100 gateway
```

Store the displayed password in a password manager. Open:

```text
http://<gateway-host>:8443
```

Sign in as `admin`, change the generated password, then remove the bootstrap
password:

```bash
sudo sed -i '/^GPUARDIAN_WEB_PASSWORD=/d' /etc/gpuardian/web.env
sudo docker compose -f /etc/gpuardian/compose.web-no-tls.yml up -d --force-recreate
```

Allow TCP `8443` only from networks that should access GPUardian.

## 4. Register every GPU node

On each node, read its root key:

```bash
sudo cat /var/lib/gpuardian/root.key
```

In the web gateway, open `Nodes`, select `Add node`, and enter:

```text
Name: gpu-node-01
Endpoint API: http://gpu-node-01.example.com:8192
Root key: contents of /var/lib/gpuardian/root.key on that node
Skip TLS verify: enabled
```

Two differences from the TLS registration:

- The endpoint uses **`http://`** (not `https://`).
- **`Skip TLS verify` is enabled** — there is no certificate to verify.

Use the node's actual DNS name or IP. Do not enter `0.0.0.0`, `127.0.0.1`, or
the gateway port `8443`.

## 5. (Optional) Put TLS in front with a reverse proxy

See [the main README](../README.md#6-optional-put-tls-in-front-with-a-reverse-proxy)
for the reverse-proxy setup. The same nginx snippet and `TRUST_PROXY` notes
apply to a no-TLS gateway — the proxy terminates TLS, the gateway stays HTTP.

## Switching to TLS later

You can migrate a no-TLS deployment to TLS without losing state:

1. Obtain TLS certificates as described in the main README.
2. Install `node.crt` / `node.key` on each node and `web.crt` / `web.key` on
   the gateway host.
3. Remove `GPUARDIAN_NODE_ALLOW_INSECURE=1` from the node systemd unit and add
   back the `GPUARDIAN_NODE_TLS_CERT` / `GPUARDIAN_NODE_TLS_KEY` lines.
4. Switch the gateway back to `compose.web.yml` (or update your no-TLS Compose
   file to set the TLS env vars and re-add the cert mounts).
5. Restart the node daemons and gateway.
6. Re-register each node with `https://` endpoint and `Skip TLS verify`
   disabled.

The persistent state under `/var/lib/gpuardian-web` (users, keys, history) is
not affected by the TLS mode.
