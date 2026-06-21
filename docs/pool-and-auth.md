# Pool load balancing and authentication

This guide covers two related features for running llama-swap as a gateway in
front of multiple always-on inference servers:

1. **`pool`** — sticky least-inflight load balancing across llama.cpp (or any
   OpenAI-compatible) backends for a single model name.
2. **`admin` + API keys** — separate authentication for the web dashboard and
   for programmatic inference clients.

## Pool load balancing

Use a `pool` when you already have **multiple inference servers running the
same model** (for example one `llama-server` per GPU node) and want llama-swap
to:

- Spread **new** sessions across backends by **least in-flight** load.
- Keep follow-up requests from the **same client/session on the same backend**
  so llama.cpp KV / prefix cache stays warm.

Pool models **do not** use `cmd`, process spawning, or the swap scheduler. They
are pure reverse proxies.

### Minimal example

```yaml
models:
  llama-70b:
    name: "Llama 70B"
    pool:
      backends:
        - proxy: http://10.0.0.1:8080
        - proxy: http://10.0.0.2:8080
        - proxy: http://10.0.0.3:8080
```

Each `proxy` value is the **base URL** of an always-on server (typically
`llama-server`). Request paths such as `/v1/chat/completions` are forwarded
unchanged.

### Full pool options

```yaml
models:
  llama-70b:
    pool:
      # Load balancing strategy. Only sticky_least_inflight is supported today.
      strategy: sticky_least_inflight

      # How long an affinity mapping is kept after the last request.
      # Default: 30m
      affinityTTL: 45m

      backends:
        - proxy: http://gpu-node-1:8080
        - proxy: http://gpu-node-2:8080

      # Optional: override the default affinity key extraction order.
      # Rules are tried top-to-bottom; first non-empty value wins.
      affinity:
        - json: prompt_cache_key
        - json: user
        - json: metadata.user_id
        - header: X-Conversation-Id
        - apiKey: true
```

| Field | Default | Description |
| ----- | ------- | ----------- |
| `strategy` | `sticky_least_inflight` | Assign new sessions to the least-busy backend; stick follow-ups to that backend. |
| `affinityTTL` | `30m` | Idle time before an affinity mapping expires. |
| `backends` | *(required)* | List of upstream base URLs. |
| `affinity` | See below | Ordered list of rules for sticky session keys. |

### How routing works

```
Client request for model "llama-70b"
        │
        ▼
  Extract affinity key from request
        │
        ├─ Key known ──► use mapped backend (refresh TTL)
        │
        └─ Key unknown ──► pick backend with lowest in-flight count
                           (store mapping if a key was found)
        │
        ▼
  Reverse proxy to chosen backend
```

Dispatch order for any model: **pool → local (swap) → peer**.

### Affinity keys (sticky sessions)

There is **no automatic session ID** in the OpenAI Chat Completions API. For
stickiness to work, clients should send one of these fields (or you accept
stateless load balancing when none are present).

Default rule order (used when `pool.affinity` is omitted):

| Priority | Source | Notes |
| -------- | ------ | ----- |
| 1 | JSON `id_slot` | llama.cpp native slot id |
| 2 | JSON `previous_response_id` | OpenAI Responses API |
| 3 | JSON `conversation.id` | OpenAI Responses API |
| 4 | JSON `prompt_cache_key` | OpenAI (preferred cache bucket id) |
| 5 | JSON `user` | OpenAI Chat Completions (legacy but common) |
| 6 | JSON `safety_identifier` | OpenAI |
| 7 | JSON `metadata.user_id` | Anthropic Messages API |
| 8 | JSON `metadata.session_id` | Common app convention |
| 9 | JSON `metadata.conversation_id` | Common app convention |
| 10 | Header `X-Conversation-Id` | Custom / gateway injection |
| 11 | Header `X-Session-Id` | Custom / gateway injection |
| 12 | API key | Coarse (one key → one backend) |

**IP address is intentionally not used** — many users can share one NAT address,
and one user can change IP between requests.

When **no** affinity key is found, each request is routed independently with
least-inflight (no stickiness). This is correct for anonymous one-shot calls.

### Tips for llama.cpp backends

- Enable prompt caching on the server (`cache_prompt: true` in requests; default
  on recent llama-server builds).
- Ensure each backend serves the **same model** under the **same model name**
  expected by clients (or use `useModelName` filters if needed).
- For best cache hit rates, have clients send `prompt_cache_key` or `user` with
  a stable per-conversation value.

### Pool vs peers

| | `pool` | `peers` |
| - | ------ | ------- |
| Same model on multiple URLs | Yes | No (one peer per model name) |
| Process management | No | No |
| Use case | Load balance identical backends | Route different models to different hosts |

---

## Authentication

llama-swap supports **two layers** of authentication:

| Layer | Protects | Mechanism |
| ----- | -------- | --------- |
| **Inference** | `/v1/*` and other model inference routes | API key (`Authorization: Bearer …`, `x-api-key`, or Basic password field) |
| **Dashboard** | `/ui/*`, `/api/*`, `/logs*`, `/metrics`, `/running`, `/unload`, `/upstream/*` | Admin session cookie when `admin.password` is set |

`/health` and `/wol-health` remain public for probes.

### Admin login (dashboard)

```yaml
admin:
  # Required to enable the login page and protect dashboard routes.
  # Supports ${env.VAR_NAME} macros.
  password: "${env.LLAMASWAP_ADMIN_PASSWORD}"

  # Admin session lifetime. Default: 24h
  sessionTTL: 24h

  # File for UI-created API keys (hashed, never plaintext). Default: llama-swap-keys.json
  keysFile: /var/lib/llama-swap/keys.json
```

When `admin.password` is set:

1. Opening `/ui/` shows a **login page**.
2. A successful login sets an **HttpOnly session cookie**
   (`llama-swap-session`).
3. Dashboard API calls and SSE (`/api/events`) use the cookie automatically
   (same-origin).

Logout is available from the UI header or `POST /api/auth/logout`.

### Inference API keys

Keys can come from **two places** (both are checked):

1. **Static keys** in config (unchanged from before):

```yaml
apiKeys:
  - "sk-my-static-key"
  - "${env.INFERENCE_API_KEY}"
```

2. **UI-managed keys** stored in `admin.keysFile` (bcrypt hashes only).

Create keys from the web UI: **API Keys** tab (`/ui/#/keys`), or via API:

```bash
# After logging in (session cookie) or with a valid API key:
curl -X POST http://localhost:8080/api/admin/keys \
  -H 'Content-Type: application/json' \
  -d '{"name":"my-client"}' \
  --cookie 'llama-swap-session=...'

# Response includes "secret" once — store it immediately.
```

Revoke:

```bash
curl -X DELETE http://localhost:8080/api/admin/keys/{id} \
  --cookie 'llama-swap-session=...'
```

UI-managed keys are prefixed with `sk-ls-`.

### Auth behaviour matrix

| Config | Inference routes | Dashboard (`/ui`, `/api`, …) |
| ------ | ---------------- | ------------------------------ |
| Nothing set | Open (default-allow) | Open |
| `apiKeys` only | Requires API key | Requires API key |
| `admin.password` only | Open | Requires login (or API key) |
| Both | Requires API key | Requires login (or API key) |

### Playground and inference from the browser

The dashboard login cookie **does not** replace inference API keys. When
inference auth is enabled (`apiKeys` and/or UI-managed keys exist):

1. Create a key on the **API Keys** page, or use a static config key.
2. Paste it into **Playground / client key** on that page (stored in browser
   local storage).
3. Playground requests send `Authorization: Bearer …` automatically.

### Auth API endpoints

| Method | Path | Auth | Description |
| ------ | ---- | ---- | ----------- |
| `GET` | `/api/auth/session` | Public | Returns `{ adminRequired, authenticated, inferenceRequired }` |
| `POST` | `/api/auth/login` | Public | Body: `{ "password": "…" }` — sets session cookie |
| `POST` | `/api/auth/logout` | Public | Clears session cookie |
| `GET` | `/api/admin/keys` | Dashboard | List keys (no secrets) |
| `POST` | `/api/admin/keys` | Dashboard | Create key; returns `secret` once |
| `DELETE` | `/api/admin/keys/{id}` | Dashboard | Revoke key |

---

## Combined example: gateway in front of a llama.cpp cluster

```yaml
admin:
  password: "${env.LLAMASWAP_ADMIN_PASSWORD}"
  sessionTTL: 12h
  keysFile: /var/lib/llama-swap/keys.json

apiKeys: []   # optional static keys; UI can add more

models:
  llama-70b:
    name: "Llama 70B"
    pool:
      affinityTTL: 30m
      backends:
        - proxy: http://192.168.1.101:8080
        - proxy: http://192.168.1.102:8080
        - proxy: http://192.168.1.103:8080
    filters:
      setParams:
        cache_prompt: true
```

Clients call `http://llama-swap:8080/v1/chat/completions` with:

```json
{
  "model": "llama-70b",
  "prompt_cache_key": "conversation-uuid-here",
  "messages": [ … ]
}
```

Operators use `http://llama-swap:8080/ui/` after logging in to monitor activity,
manage keys, and use the playground.

---

## Security notes

- Store `admin.password` via environment macros, not plaintext in shared configs.
- Protect `admin.keysFile` (mode `0600`); it contains bcrypt hashes.
- UI key secrets are shown **once** at creation; they cannot be retrieved later.
- Rotate keys by revoking and creating new ones.
- For production, terminate TLS in front of llama-swap (reverse proxy) so session
  cookies and API keys are not sent in cleartext.
