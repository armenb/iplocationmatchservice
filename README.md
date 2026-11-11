# iplocationmatch

Answers one question: *is this IP address in one of these countries?* over HTTP and gRPC, using a
read-only, memory-mapped MaxMind GeoLite2-Country `.mmdb` file and
[`maxminddb-golang/v2`](https://github.com/oschwald/maxminddb-golang). Lookups are lock-free and safe
for concurrent use.

## Database

The GeoLite2 database is licensed by MaxMind and is not committed. Download it once (free account
and license key required):

```sh
make fetch-db MAXMIND_ACCOUNT_ID=123456 MAXMIND_LICENSE_KEY=xxxx   # -> data/GeoLite2-Country.mmdb
```

`fetch-db` needs GNU tar (it uses the GNU-only `--wildcards` flag). The Makefile uses `tar` where that is
GNU tar (Linux) and `gtar` otherwise. On macOS, whose built-in `tar` is BSD tar, install it first with
`brew install gnu-tar`. Override the choice with `make fetch-db TAR=/path/to/gnu-tar ...`.

Or copy your own `GeoLite2-Country.mmdb` to that path. The file is opened read-only; replace it and
restart the service to pick up a new release. That is fine for local development; for production, see
[DESIGN.md](DESIGN.md) for the canary-rollout approach to updating the mapping data.

## Run

```sh
make run          # or: bin/iplocationmatch -db data/GeoLite2-Country.mmdb
```

| Flag | Env | Default |
|---|---|---|
| `-db` | `IPLOCATIONMATCH_DB` | `data/GeoLite2-Country.mmdb` |
| `-http-addr` | `IPLOCATIONMATCH_HTTP_ADDR` | `:8080` |
| `-grpc-addr` | `IPLOCATIONMATCH_GRPC_ADDR` | `:9090` |
| `-statsd-addr` | `IPLOCATIONMATCH_STATSD_ADDR` | empty (metrics off) |
| `-max-allowed-countries` | `IPLOCATIONMATCH_MAX_ALLOWED_COUNTRIES` | `50` |

SIGINT/SIGTERM trigger a graceful shutdown (10s drain); a second signal during the drain kills the process.

## HTTP API

`POST /v1/check`

```sh
curl -s localhost:8080/v1/check -d '{"ip":"81.2.69.142","allowed_countries":["GB","US"]}'
# {"match":true}
```

- `ip`: IPv4 or IPv6 (IPv4-mapped IPv6 is treated as IPv4). Send a bare address with no surrounding whitespace: square brackets
  (`[2001:db8::1]`) and port suffixes (`1.2.3.4:443`, `[2001:db8::1]:443`) are not currently accepted and
  return 400 / `INVALID_ARGUMENT`. Support could be added, e.g. by falling back to `netip.ParseAddrPort` in
  `parseIP`; strip them in the load balancer or caller until then.
- `allowed_countries`: non-empty list of ISO 3166-1 alpha-2 codes, case-insensitive, with at most 50
  entries by default (`-max-allowed-countries`; every country in Europe plus a handful of others). An empty
  list is a `400`, not "no restriction": a caller with no restriction can skip the call. A longer list is also
  a `400`, so a request that needs more is a prompt to talk to the customer about the limit or about an
  "all except these" variant; see [DESIGN.md](DESIGN.md).
- **Unknown locations are never allowed (fail closed).** An address the database has no country for
  (private or reserved ranges, unmapped networks, some anycast ranges) returns `{"match":false}`, the same
  as a known address in another country. See [DESIGN.md](DESIGN.md) for why.
- The body must be one JSON object with exactly these keys. Key names are case-sensitive, none may repeat, and
  unknown keys are rejected. It is parsed as JSON whatever the `Content-Type`, so `curl -d` works without a header.
- Errors return `{"error":"..."}`: `400` for a bad IP, bad country code, empty or too long list, malformed JSON,
  repeated or unknown keys; `413` for bodies over 1 MiB. A non-POST method gets `405` and an unknown path `404`, both
  as plain text.

`GET /healthz` returns `200`. It is not logged or counted.

The full contract, with examples, is in [`api/openapi.yaml`](api/openapi.yaml).

## gRPC API

Defined in [`proto/iplocationmatch/v1/iplocationmatch.proto`](proto/iplocationmatch/v1/iplocationmatch.proto):
`iplocationmatch.v1.IPLocationMatchService/CheckIP(CheckIPRequest{ip, allowed_countries}) -> CheckIPResponse{match}`.
Invalid input returns `INVALID_ARGUMENT`.

```sh
grpcurl -plaintext -proto proto/iplocationmatch/v1/iplocationmatch.proto -import-path proto \
  -d '{"ip":"81.2.69.142","allowed_countries":["GB"]}' localhost:9090 iplocationmatch.v1.IPLocationMatchService/CheckIP
```

Generated code lives in `gen/` and is committed; regenerate with `make proto`.

### Naming: `CheckIP` vs `/v1/check`

The gRPC method `CheckIP` and the HTTP route `POST /v1/check` are the same operation, named by each
protocol's own convention rather than matched one-to-one:

- gRPC methods are verb-noun names on a service (`IPLocationMatchService/CheckIP`).
- HTTP routes are a short path under a version prefix (`/v1/check`). The service name is deliberately
  location-generic, so the route avoids repeating "ip" or "country"; the request body already names
  the `ip` and `allowed_countries` it checks.

## Observability

**Request log.** Every request except `/healthz` is logged as one JSON line on stderr, for both HTTP and gRPC:

```json
{"time":"2026-11-11T15:35:40-05:00","level":"INFO","msg":"request","transport":"http","code":"200","result":"no_match","country":"US","ip":"8.8.8.8","db_version":"2026-11-10","dur_us":200}
```

- `code` is the HTTP status or gRPC code name; `result` is one of `match`, `no_match`, `unknown_ip`,
  `invalid` (client error) or `error` (server error, logged at `ERROR` with an `err` field).
- `country` is the country the IP resolved to; it says why a user was denied. It is omitted when the
  location is unknown (`unknown_ip`) or the request failed before the lookup, and is never a metric tag
  (about 250 values).
- `db_version` is the database build date, so you can tell which data a pod is serving. It is also logged
  once at startup.
- **Keep it compact:** log volume scales with request volume, which can be one call per HTTP resource a
  user loads. The line has a fixed, short set of fields; it deliberately omits the country list and the
  request body, and truncates the caller-supplied `ip` to 64 bytes. The IP is personal data in some
  jurisdictions; drop the `ip` field in `internal/telemetry` if your policy requires.

**Metrics.** Set `-statsd-addr` to send [DogStatsD](https://docs.datadoghq.com/developers/dogstatsd/) over UDP
(plain statsd has no tags; DogStatsD adds them as `|#key:value`). Metrics are queued and sent in batches off
the request path, and are dropped rather than ever blocking a request.

| Metric | Type | Tags |
|---|---|---|
| `iplocationmatch.requests` | counter | `transport:http\|grpc`, `result:<as above>`, `db_version:<date>` |
| `iplocationmatch.request.duration` | timer (ms) | `transport`, `db_version` |

With the Datadog agent on Kubernetes, point `-statsd-addr` at the node-local agent (for example
`$(HOST_IP):8125`, with `HOST_IP` from the downward API `status.hostIP`).

## Container and Kubernetes

```sh
make fetch-db       # the image build copies the database; it never downloads it
make docker-build   # -> iplocationmatch:dev (override with IMAGE=...)
```

The `Dockerfile` is a multi-stage build into `gcr.io/distroless/static:nonroot` (UID 65532). It copies
`data/GeoLite2-Country.mmdb` by name, so the build fails if that file is missing; `make docker-build`
checks first and prints a clearer message. Keep the image in a private registry: GeoLite2 is licensed by
MaxMind.

`deploy/k8s/` has a `Deployment` (2 replicas, zero-unavailable rolling updates, non-root, read-only root
filesystem, `/healthz` probes, and a 5-second `preStop` sleep so routes to a terminating pod are removed before the
server stops accepting connections; the kubelet runs it, since the distroless image has no shell) and a `Service` exposing `http` on 80 and `grpc` (h2c) on 9090. Set the
`image:` in `deployment.yaml` to your registry path and an immutable tag.

## Dependency provenance

`github.com/oschwald/maxminddb-golang/v2` (v2.9.0) is the `.mmdb` reader. Checked 2026-11-11:

- It is the original repository, not a fork (GitHub API: `fork: false`). License: ISC.
- The maintainer, Gregory Oschwald, lists `@maxmind` as his employer on his GitHub profile and is a public
  member of the `maxmind` GitHub organization.
- The package's own README states: "This is not an official MaxMind API." It is therefore a
  MaxMind-affiliated, community-hosted package under a personal account, not a MaxMind-published one.
  This review found no MaxMind-published Go reader to compare against.
- MaxMind's own `maxmind/mmdbwriter` (used here only by tests) depends on it.

`go.sum` pins content hashes; update the dependency deliberately, not via blanket `go get -u`.

`govulncheck ./...` on 2026-11-11 reported no vulnerabilities after bumping Go to 1.27.4 (GO-2026-6859,
`net/http` `MaxBytesReader`, which this service uses) and `golang.org/x/net` to v0.62.0
(GO-2026-6860, GO-2026-6866; not called by this code).

## Tests

```sh
make test               # unit tests (race detector)
make test-integration        # builds and runs the real binary; queries HTTP + gRPC over TCP (fixture DB)
make test-integration-real   # same suite, additionally against the real DB at $(DB) (run fetch-db first)
```

`test-integration-real` sets `IPLOCATIONMATCH_TEST_DB`; it fails if that file is missing. It asserts well-known
public addresses (`8.8.8.8` and `2001:4860:4860::8888` in `US`), which depend on the database release.

Tests never need the MaxMind database: `internal/testdb` writes a small GeoLite2-Country-shaped
`.mmdb` with `mmdbwriter`.
