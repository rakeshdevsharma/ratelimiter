# Rate limiter behind Nginx `auth_request`

```
client ──► nginx :8080 ──(auth_request subrequest over unix socket, keepalive)──► Go /check
                │                                                                   │
                │◄────────── 204 allow / 403 rate limited / 500 error ───────────────┘
                ▼
        backend (or 429 to the client)
```

Each limiter instance is independent: state is in memory, per process, and is not shared between
instances. A client whose requests reach N gateway instances gets up to N × the limit.

## Layout

- `store/`: the generic `store.Store[T]` interface and an in-memory implementation
  (`store.NewMemoryStore[T]()`). It knows nothing about rate limiting. The in-memory store is split
  into 16 shards, each with its own `RWMutex`, chosen by an FNV-1a hash of the key, so requests for
  keys in different shards don't wait on one lock.
- `ratelimit/`: the strategies (`fixedwindow`, `slidingwindow`, `tokenbucket`, `leakybucket`),
  `Config` and its validation, and `RateLimitMeta`, the per-IP state the strategies keep in a
  `store.Store[ratelimit.RateLimitMeta]`. `ratelimit.New` takes any implementation of that interface.
- `cmd/ratelimit/`: the server. `cmd/loadtest/`: the load tester.
- `nginx/ratelimiter.conf`: the Nginx config for local runs.

## Run

```sh
go run ./cmd/ratelimit -strategy=tokenbucket -limit=10 -window=2s   # burst 10, refill 5/s
go run ./cmd/ratelimit -strategy=fixedwindow -limit=10 -window=1s
```

Flags:
- `-strategy` (required): `tokenbucket`, `leakybucket`, `fixedwindow` or `slidingwindow`.
- `-socket`: unix socket path, default `/tmp/ratelimiter.sock`.
- `-limit`: max requests per window, or the bucket capacity. Default 10.
- `-window`: window length. Default `1s`.

Token and leaky bucket refill/drain at `limit/window` per second, so `-limit` sets the burst
capacity and `-window` sets how long a full refill/drain takes. Both must be > 0; the server
refuses to start otherwise.

Test the Go server directly, without Nginx:

```sh
curl -i --unix-socket /tmp/ratelimiter.sock http://x/check -H 'X-Real-IP: 1.2.3.4'
# 204 = allowed, 403 = rate limited, 500 = store error
```

Run the tests:

```sh
go test -race ./...
```

## Nginx (Homebrew)

```sh
brew install nginx
cp nginx/ratelimiter.conf /opt/homebrew/etc/nginx/servers/
nginx -t && nginx            # or: nginx -s reload
for i in $(seq 1 30); do curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/; done
```

To check keepalive, run something like `ab -n 5000 -c 20 http://localhost:8080/`. The Go log's
`new connection (total N)` should stay small, not grow by one per request.
`lsof -U | grep ratelimiter.sock` should show a small, stable set of connections.

Why 403 and not 429: `auth_request` only accepts 2xx, 401 and 403. Any other status becomes a 500.
So the `/check` handler in `cmd/ratelimit/main.go` maps the limiter's 429 to 403, and
`error_page 403 = @too_many` turns it back into 429.

### What happens when the limiter fails

`Check` returns 200 (allow), 429 (limited) or 500 (the store returned an error). The handler passes
500 straight through. Nginx also produces a 500 when the limiter is down, the socket is missing, or
`proxy_read_timeout` (100ms) expires.

The current Nginx config does **not** handle that 500, so clients get a 500 (fail closed). Failing
open is part of the [future deployment work](#future-work-deployment-as-two-containers-in-one-pod).

## Load testing

`cmd/loadtest` sends traffic through Nginx from many fake client IPs (`10.0.0.1`, `10.0.0.2`, ...).

Faking IPs works like this:
- Nginx normally sets `X-Real-IP` from `$remote_addr`, which is always `127.0.0.1` for a local tester.
- The config enables the **realip** module (`set_real_ip_from 127.0.0.1; real_ip_header X-Forwarded-For;`).
  For requests from localhost only, Nginx takes the client IP from `X-Forwarded-For`.
  Real load balancers use the same trust pattern.
- The tester sends a different `X-Forwarded-For` for each simulated client.
- `nginx -V 2>&1 | grep realip` confirms the module is compiled in. Homebrew's build includes it.

```sh
# every IP at a fixed rate; -timeline shows per-second allowed/limited
go run ./cmd/loadtest -mode=steady -ips=5 -rps=20 -duration=5s -timeline

# each IP fires -burst requests at the same instant (tests bucket capacity)
go run ./cmd/loadtest -mode=burst -ips=3 -burst=50

# 2 heavy IPs well over the limit, 8 light IPs under it; warns if a light IP is limited
go run ./cmd/loadtest -mode=mixed -ips=10 -heavy=2 -heavy-rps=50 -light-rps=2
```

What to expect with the server started as `-limit=10 -window=1s`:

| Strategy | steady `-rps=20` timeline | burst `-burst=50` |
|---|---|---|
| fixedwindow | ~10 allowed per second per IP, resets on window boundaries | 10 allowed, 40 × 429 |
| slidingwindow | ~10 per rolling second; no 2x spike at window edges | 10 allowed, 40 × 429 |
| tokenbucket | first second up to 10 (burst) + refill, then ~`limit/window`/s | 10 allowed, 40 × 429 |
| leakybucket | steady ~`limit/window`/s | ~10 (bucket capacity) allowed |

In mixed mode, light IPs must never be limited. A `WARN` line means limits are shared between keys,
or there is a bug. `other/err` counts mean something broke: an HTTP 500 means the limiter was
unreachable, too slow, or returned a store error.

## Things to watch for

- **Concurrent first requests:** two first requests from one IP must get the same entry, or one
  count is lost. `store.Get` creates missing entries itself and re-checks under the shard's write
  lock. Any new `Store` implementation must keep that guarantee.
- **Store errors:** every strategy returns 500 when `store.Get` fails, and never touches a nil
  entry. Whether that means fail open or fail closed is decided in Nginx (see above).
- **`RateLimitMeta` contains a `sync.Mutex`,** so always pass it by pointer. `go vet` flags copies.
- **Fixed window boundaries:** `fixedwindow` can allow up to 2 × limit across two adjacent
  windows. With large limits that is a lot of extra traffic; prefer `slidingwindow` or `tokenbucket`.

## Known issues

- **`MemoryStore` never evicts entries.** Every distinct client IP gets a permanent `RateLimitMeta`,
  so memory grows without bound. Because Nginx trusts `X-Forwarded-For` from `127.0.0.1` (see the
  realip setup above), a local client can also send many random IPs and inflate the map on purpose.
  A fix would be idle-TTL eviction (a background sweep that drops entries whose last request is older
  than a few windows) or an LRU with a fixed size cap.
- **`slidingwindow` lets slightly more than the limit through.** It truncates the weighted
  previous-window count with `int()` before comparing, so a count of 9.9 is treated as 9.
  Keep the estimate as a `float64`.
- **Requests without `X-Real-IP` share one bucket.** The handler uses the key `"unknown"` for them,
  so one such client can get all of them limited.

## TODO: store review

- [ ] **Neighbouring shards still share a CPU cache line.** `memoryStore.shards` is an array of
  `*shard`, and each `shard` is a separate 32-byte allocation; consecutive shards are allocated 32
  bytes apart, so two shards sit in one 64-byte line. Every `RLock`/`RUnlock` writes to the mutex,
  so readers of one shard slow down readers of its neighbour (false sharing), which cancels part
  of the benefit of sharding. Store the shards by value (`[shardCount]shard[T]`) and pad each one to
  a full line (`_ [64 - 32]byte`; 128 bytes on Apple Silicon).
- [ ] **The shard hash has no seed.** FNV-1a sends a given key to the same shard in every process,
  so anyone can work out offline which IPs share a shard. A client that controls its source
  addresses, such as one rotating through an IPv6 /64, can put all its traffic on one shard's lock
  and slow down every legitimate client in that shard. Use `hash/maphash` with a per-process
  `maphash.MakeSeed()`; it is also about 2.7× faster than FNV here (3.3 ns vs 9.1 ns per hash).

## Future work: deployment as two containers in one pod

Each gateway replica is one pod (Kubernetes) or task (ECS) with two containers: Nginx and the
limiter. They share a volume that holds the unix socket. The limiter is not a separate service:
that would need TCP and an extra network hop, and there is no state to share between instances.

```
┌──────────────────────────── Pod (one per gateway replica) ───────────────────────────┐
│                                                                                       │
│  initContainers (native sidecar, restartPolicy: Always)     containers                │
│  ┌──────────────────────────┐                     ┌──────────────────────────────┐    │
│  │ ratelimiter (Go)         │   unix socket       │ nginx                        │    │
│  │ distroless/static,       │◄────────────────────│ nginx-unprivileged           │◄───┼── :8080 from LB
│  │ uid 65532                │  /run/ratelimiter/  │ uid 101                      │    │
│  │ listens rl.sock (0660)   │      rl.sock        │ auth_request → unix socket   │    │
│  └──────────┬───────────────┘                     └──────────────┬───────────────┘    │
│             └──────────── emptyDir "rl-sock" (group = fsGroup) ──┘                     │
└───────────────────────────────────────────────────────────────────────────────────────┘
```

### Why two containers and not one

| | Two containers, one pod | One container, two processes |
|---|---|---|
| Restart on crash | Each container restarts on its own | Needs a supervisor (s6-overlay, supervisord) |
| Health checks | Per process | One check for both, or custom scripts |
| CPU/memory limits | Separate; limiter memory growth can't starve Nginx | Shared |
| Logs | Separate streams | Mixed together |
| Images | Official Nginx image plus a small static Go image, upgraded separately | One custom image rebuilt for either change |

### How it works

- **Socket on a shared volume.** An `emptyDir` is mounted at `/run/ratelimiter` in both containers.
  It survives container restarts and is deleted only with the pod, so the limiter's existing
  "remove stale socket on startup" step is still needed.
- **Permissions.** The containers run as different users. `securityContext.fsGroup: 2000` on the
  pod makes the volume group-owned by 2000 with the setgid bit, and gives every container 2000 as a
  supplementary group, so socket mode `0660` is enough and `0666` is no longer needed.
- **Start and stop order.** The limiter is an `initContainer` with `restartPolicy: Always` (a native
  sidecar; on by default since Kubernetes 1.29). It starts first, Nginx starts only after the
  limiter's `startupProbe` passes, and on shutdown Nginx stops first while the limiter keeps serving.
  On older clusters, run both as plain containers and rely on fail-open during startup.
- **Probes.** Kubelet's `httpGet`/`tcpSocket` probes connect to the pod IP and can't reach a unix
  socket, and the distroless image has no curl, so the limiter checks itself with an `exec` probe
  (`/ratelimiter -probe`). Nginx gets an `httpGet` probe on `/nginx-health`, which skips
  `auth_request`. Nginx readiness must not depend on the limiter: with fail-open, a dead limiter
  should not take the gateway out of the load balancer.
- **Resources.** Separate requests/limits per container. Set `GOMEMLIMIT` (ideally ~90% of the
  limiter's memory limit) so the Go GC works harder before the container is OOM-killed.
  `GOMAXPROCS` already follows the CPU limit (Go 1.25+).
- **Signals.** Kubernetes sends SIGTERM; the limiter already shuts down within 5s and removes the
  socket. The Nginx images use `STOPSIGNAL SIGQUIT` for a graceful drain.
  `terminationGracePeriodSeconds` (default 30s) must cover both.
- **Independent limits.** Each pod enforces its own limit. Either hash on source IP at the load
  balancer so each IP always reaches the same pod, or set the per-pod limit to the global limit ÷
  the number of pods.

### Code changes

`cmd/ratelimit/main.go`:

1. A `-socket-mode` flag (default `0o660`) instead of the hard-coded `os.Chmod(*socket, 0o666)`.
   The pod passes `-socket=/run/ratelimiter/rl.sock`; the existing `-socket` flag needs no change.
2. A `-probe` flag: `GET /healthz` over `-socket` with a 1s timeout, exit 0 on 200 and 1 otherwise,
   so the same binary serves as its own `exec` probe:

   ```go
   func runProbe(socket string) int {
       c := &http.Client{
           Timeout: time.Second,
           Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
               return (&net.Dialer{}).DialContext(ctx, "unix", socket)
           }},
       }
       resp, err := c.Get("http://unix/healthz")
       if err != nil {
           return 1
       }
       resp.Body.Close()
       if resp.StatusCode != http.StatusOK {
           return 1
       }
       return 0
   }
   ```
3. Optional: make the 5s shutdown timeout a flag, kept shorter than the grace period.

`nginx/ratelimiter.conf`:

```nginx
upstream ratelimiter {
    server unix:/run/ratelimiter/rl.sock;    # was /tmp/ratelimiter.sock
    keepalive 32;
}
server {
    listen 8080;                             # the unprivileged image can't bind :80
    # remove set_real_ip_from 127.0.0.1; trust only the LB's CIDR (plus real_ip_recursive on), if any
    location = /nginx-health { access_log off; return 200; }   # no auth_request
    location / {
        auth_request /_ratelimit;
        error_page 403 = @too_many;
        error_page 500 = @bypass;            # fail open: limiter down, slow or store error
        proxy_pass http://backend;
    }
    location @bypass {
        proxy_pass http://backend;           # same upstream and proxy settings as location /
    }
    ...
}
```

Catch only `500`. Nginx itself produces 502 and 504 when the **backend** fails; sending those to
`@bypass` would send the request to the backend a second time, and could run a POST twice.
Fail-open is silent, so log `$upstream_status` for the `/_ratelimit` subrequest to see when the
limiter was bypassed. Consider lowering `proxy_read_timeout` to 20–50ms: it is the most latency a
hung limiter can add to each request.

New `Dockerfile` (limiter):

```dockerfile
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /ratelimiter ./cmd/ratelimit

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ratelimiter /ratelimiter
ENTRYPOINT ["/ratelimiter"]
```

New `nginx/Dockerfile`: `FROM nginxinc/nginx-unprivileged:<pinned version>` and copy the config into
`/etc/nginx/conf.d/`, or skip it and mount the config from a ConfigMap.

New `deploy/k8s.yaml` (the important parts):

```yaml
spec:
  terminationGracePeriodSeconds: 30
  securityContext:
    fsGroup: 2000
    runAsNonRoot: true
  volumes:
    - name: rl-sock
      emptyDir: {}
  initContainers:
    - name: ratelimiter
      image: ratelimiter:<tag>
      restartPolicy: Always                      # native sidecar
      args: ["-strategy=tokenbucket", "-limit=1000", "-window=1s",
             "-socket=/run/ratelimiter/rl.sock"]
      env:
        - name: GOMEMLIMIT                       # full limit; set an explicit ~90% value if preferred
          valueFrom:
            resourceFieldRef: { resource: limits.memory }
      volumeMounts: [{ name: rl-sock, mountPath: /run/ratelimiter }]
      startupProbe:  { exec: { command: ["/ratelimiter", "-probe", "-socket=/run/ratelimiter/rl.sock"] }, periodSeconds: 1, failureThreshold: 10 }
      livenessProbe: { exec: { command: ["/ratelimiter", "-probe", "-socket=/run/ratelimiter/rl.sock"] }, periodSeconds: 10 }
      resources:
        requests: { cpu: 500m, memory: 512Mi }
        limits:   { memory: 1Gi }
  containers:
    - name: nginx
      image: gateway-nginx:<tag>
      ports: [{ containerPort: 8080 }]
      volumeMounts: [{ name: rl-sock, mountPath: /run/ratelimiter }]
      readinessProbe: { httpGet: { path: /nginx-health, port: 8080 } }
      livenessProbe:  { httpGet: { path: /nginx-health, port: 8080 } }
      resources:
        requests: { cpu: "1", memory: 256Mi }
        limits:   { memory: 512Mi }
```

For local runs with Docker Compose: two services sharing a named volume at `/run/ratelimiter`, the
same `group_add` on both, a limiter `healthcheck` of `["/ratelimiter", "-probe", ...]`, and
`depends_on: { ratelimiter: { condition: service_healthy } }` on Nginx.

None of this has been built or tested yet.
