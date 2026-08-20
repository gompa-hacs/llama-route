# Pool Load Balancing and Authentication

A complete example of running llama-swap as a gateway in front of multiple
always-on inference servers, with admin login and inference API key auth.

## Topology

```
Client ──► llama-swap (gateway with this config)
                  │
             ┌────┼────────┐
             ▼    ▼        ▼
          GPU-1  GPU-2   GPU-3
        llama-server x3   (same models on each)
```

Each GPU node runs its own `llama-server` instances. llama-swap:

- **Load-balances** new sessions to the least-busy backend.
- **Sticks** follow-up requests to the same backend (KV cache stays warm).
- **Authenticates** inference requests via API keys and the dashboard via admin login.

## What this example shows

| Feature | Where in config |
|---------|----------------|
| Admin dashboard login | `admin.password`, `admin.sessionTTL` |
| UI-managed API keys | `admin.keysFile` |
| Static inference API keys | `apiKeys[]` |
| Pool with 3 backends | `models.llama-70b.pool` |
| Context-size-aware backends | `pool.backends[].contextSize` |
| Affinity TTL tuning | `pool.affinityTTL` |
| Custom affinity rules | `models.embeddings.pool.affinity` |
| Request filters on pools | `models.llama-70b.filters.setParams` |
| Mixed pool + local models | `models.local-tool` (swapped locally) |
| Scheduler priorities | `routing.scheduler.settings.fifo.priority` |

## How to use

1. Copy `config.yaml` as your llama-swap config.
2. Set the environment variables:
   ```bash
   export LLAMASWAP_ADMIN_PASSWORD="your-dashboard-password"
   export INFERENCE_API_KEY="sk-your-inference-key"
   ```
3. On each GPU node, start `llama-server` for every model:
   ```bash
   # GPU-1: llama 70B
   llama-server --host 0.0.0.0 --port 8080 -m Llama-3.1-70B.gguf -ngl 99
   # GPU-1: llama 8B
   llama-server --host 0.0.0.0 --port 8081 -m Llama-3.1-8B.gguf -ngl 99
   # GPU-1: embeddings
   llama-server --host 0.0.0.0 --port 8082 -m all-MiniLM-L6-v2.gguf -ngl 99
   ```
4. Update `backends[].proxy` URLs in `config.yaml` to match your node IPs and ports.
5. Start llama-swap:
   ```bash
   llama-swap --config config.yaml
   ```

## Making requests

```bash
curl http://llama-swap-host:8080/v1/chat/completions \
  -H "Authorization: Bearer $INFERENCE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama-70b",
    "prompt_cache_key": "user-session-123",
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

The `prompt_cache_key` field is used as the affinity key so follow-up
requests from the same conversation hit the same backend.

## Dashboard

Open `http://llama-swap-host:8080/ui/` and log in with the admin password.
From the dashboard you can:

- Monitor active sessions and backend load.
- Create inference API keys from the **API Keys** tab.
- Test models in the **Playground** (paste a key into *client key*).

## Dispatch order

When a request arrives for model X, llama-swap checks:

1. **Pool** — Is X a pooled model (multiple always-on backends)?
2. **Local** — Is X a locally-managed model (group/matrix routing)?
3. **Peer** — Is X provided by a remote peer?

If none match, the request returns 404.

## Security notes

- Always use `${env.VAR_NAME}` macros for passwords and API keys — never store
  them in plaintext in the config file.
- Protect `admin.keysFile` (mode `0600`); it contains bcrypt hashes of UI-created keys.
- UI key secrets are shown **once** at creation; they cannot be retrieved later.
- For production, terminate TLS in nginx in front of llama-swap (see
  [`../nginx/`](../nginx/)), bind with `--listen localhost:8080`, and set both
  `apiKeys` and `admin.password`.
