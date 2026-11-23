# Kuma Docker Recovery

A minimal HTTP webhook receiver that restarts explicitly allowlisted Docker containers when an Uptime Kuma monitor reports DOWN.

## Configure

1. Copy `config/config.example.json` to `${app_data}/kuma-docker-recovery/config.json` on the Docker host.
2. Set a long random `webhook_token`.
3. Edit `containers`: keys must exactly match the Uptime Kuma monitor names; values must match Docker container names.
4. Deploy with Docker Compose / Portainer from this repository.

Generate a token with:

```bash
openssl rand -hex 32
```

## Uptime Kuma

Create a Webhook notification. Set the URL to:

```text
http://kuma-docker-recovery:8080/webhook/kuma/YOUR_SECRET_TOKEN
```

Use a custom JSON body with the following shape (Uptime Kuma's webhook template fields may vary by version):

```json
{
  "monitor": { "name": "Radarr" },
  "heartbeat": { "status": 0, "msg": "Monitor is DOWN" }
}
```

The receiver acts only on `heartbeat.status = 0` (DOWN). Status 1 (UP) and 2 (PENDING) are ignored.

Attach this notification to the monitors you want to recover. Confirm the actual JSON sent by your installed Kuma version before enabling it broadly.

## Test

Health endpoint:

```bash
curl http://localhost:8080/healthz
```

Webhook test from a container on the same Docker network:

```bash
curl -X POST \
  "http://kuma-docker-recovery:8080/webhook/kuma/YOUR_SECRET_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"monitor":{"name":"Radarr"},"heartbeat":{"status":0,"msg":"test"}}'
```

## Safety behaviour

- Only container names present in the configuration allowlist can be restarted.
- A five-minute cooldown is applied per monitor by default.
- At most three restart attempts per monitor are allowed in a 30-minute window.
- Cooldown and attempt history are held in memory and reset if this receiver is recreated.
- Docker socket access is highly privileged. Do not expose this endpoint to the public internet. Keep it on a trusted Docker network and use a strong token.
- A Docker socket mounted `:ro` would still permit Docker API operations; this service needs the socket to restart containers.

## Build

The Dockerfile uses BuildKit target platform variables and produces a static binary for ARM64 or AMD64. For a Git-based Portainer stack, keep the Dockerfile and `main.go` in the repository alongside the Compose file.
