---
name: user-reserve-gpus
description: How an end-user creates a GPUardian account, signs in, reserves/claims GPUs through the web UI, and retrieves their fixed gk_ key. Use whenever a user asks to sign up, create an account, reserve or claim or book GPUs, schedule GPU time, or get their key — even if they just say "I need a GPU" or "how do I get access".
---

# Reserve GPUs as an end-user

This is the regular-user (non-admin) flow for getting GPU access through the
web gateway. A regular user never needs a node root key (`rk_...`) — only
their account and their fixed key (`gk_...`).

## 1. Create an account and sign in

Open the gateway URL (e.g. `https://gpuardian.example.com:8443`).

- If **`Create account`** is visible (the operator has enabled
  `GPUARDIAN_WEB_ALLOW_REGISTRATION=1`): pick a username and a password
  **≥ 12 bytes**. Self-registered accounts are always regular users and are
  signed in immediately.
- If account creation is **disabled**: ask an admin to create the account for
  you (they do it in the `Users` tab).

There is no CLI subcommand to create a web-gateway account — account creation
is web-API-only (`POST /api/register` when registration is enabled, or
`POST /api/users` by an admin).

## 2. Reserve GPUs (web UI)

1. Sign in → select a node → open the **`Schedule`** tab.
2. Select one or more **available** GPUs.
3. Choose a start/end time and enter a **purpose**.
4. Submit the reservation.

## 3. Get your fixed key (`gk_...`)

Open the **`Key`** tab → **`Show key`** → copy the `gk_...` secret.

Key facts about the fixed key:

- **One key per account**, shared across all synced nodes and all your
  reservations. Reserving another window does **not** change the key.
- Use **`Regenerate`** only when the credential must be replaced. The
  previous version stops working once the managed-key snapshot reaches each
  node — it is not instant.
- **Revoking a reservation does not change the fixed key.** The key is
  independent of any single reservation.
- Node badges in the UI show the key-snapshot sync state per node.

The `gk_...` key is the credential for the **node CLI**
(`KEY=gk_... gpuardian run ...`). Gateway SDK clients use a scoped `ga_...`
access token instead. See the `user-cli-ops` skill for running workloads.

## Gotchas

- **Password must be ≥ 12 bytes** (hard gate, `minPasswordBytes`); ≤ 1024 bytes.
- **Self-registered accounts are always regular users** — never admin, even
  if the operator left registration open.
- **Managed-key sync must reach a node** before reservations/`allow` work for
  a new user. Account creation triggers the sync, but the node daemon must
  support the `managed_user_keys_v1` capability or reservations fail with
  "node daemon must be upgraded for fixed user keys". This is the operator's
  concern, not yours — if you hit it, tell the operator.
- **Regular users see only their own resources** in the web UI (reservations,
  keys, history). Admins see everything.

## Read before sensitive edits

- `README.md` — "Create an account and sign in" and "Reserve GPUs" sections.
- `AGENTS.md` — key prefix conventions (`gk_` user, `rk_` admin).
