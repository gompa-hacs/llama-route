# nginx reverse proxy (personal / internet-facing)

Run llama-swap bound to **localhost** and put nginx in front for TLS, rate
limits, and streaming. This is the recommended way to expose llama-swap on the
public internet without Tailscale/VPN.

## Topology

```
Internet (HTTPS :443)
        │
        ▼
     nginx (TLS + rate limits)
        │
        ▼
  127.0.0.1:8080  llama-swap  (--listen localhost:8080)
```

## llama-swap config

Use **both** layers for a public deployment:

```yaml
admin:
  password: "${env.LLAMASWAP_ADMIN_PASSWORD}"
  sessionTTL: 12h

apiKeys:
  - "${env.INFERENCE_API_KEY}"

# Optional: only needed if nginx is not on loopback.
# Defaults already trust 127.0.0.0/8 and ::1.
# http:
#   trustedProxies:
#     - 127.0.0.1/32
#     - ::1/128

models:
  # … your models …
```

Start llama-swap on loopback only:

```bash
export LLAMASWAP_ADMIN_PASSWORD='…'
export INFERENCE_API_KEY='…'   # e.g. openssl rand -hex 32
llama-swap --config config.yaml --listen localhost:8080
```

- **Inference** (`/v1/*`, …): `Authorization: Bearer $INFERENCE_API_KEY`
- **Dashboard** (`/ui`, `/api/events`, logs, …): admin password → session cookie
- Inference API keys **do not** unlock the dashboard when `admin.password` is set

## Install the nginx site

1. Copy [`nginx.conf`](nginx.conf) into your sites directory (path varies):
   - Debian/Ubuntu: `/etc/nginx/sites-available/llama-swap`
   - Then: `ln -s /etc/nginx/sites-available/llama-swap /etc/nginx/sites-enabled/`
2. Replace `YOUR_DOMAIN` and certificate paths (or use Certbot).
3. Test and reload:

```bash
sudo nginx -t && sudo systemctl reload nginx
```

### Certbot (Let’s Encrypt)

```bash
sudo certbot --nginx -d YOUR_DOMAIN
```

## What the nginx config does

| Feature | Why |
| ------- | --- |
| TLS terminate | Session cookies get the `Secure` flag via `X-Forwarded-Proto: https` |
| `proxy_pass http://127.0.0.1:8080` | llama-swap never listens on the public interface |
| `proxy_buffering off` on SSE / chat streams | Prevents broken streaming ([#236](https://github.com/mostlygeek/llama-swap/issues/236)) |
| Long `proxy_read_timeout` | Large generations / model loads |
| Rate limits on login + inference | Softens brute-force and abuse |
| Forwarded headers | Correct client IP in logs + Secure cookies |

## Quick checks

```bash
# Health (public)
curl -fsS https://YOUR_DOMAIN/health

# Inference (needs API key)
curl -fsS https://YOUR_DOMAIN/v1/models \
  -H "Authorization: Bearer $INFERENCE_API_KEY"

# Dashboard login
open https://YOUR_DOMAIN/ui/
```

## Notes

- llama-swap strips the client `Authorization` / `x-api-key` before talking to
  upstream `llama-server` backends so your gateway key is not forwarded.
- If nginx runs on another host, set `http.trustedProxies` to that nginx IP/CIDR
  and firewall so only nginx can reach llama-swap.
- `/health` stays public for probes; do not put secrets there.
