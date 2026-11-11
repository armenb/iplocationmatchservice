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

SIGINT/SIGTERM trigger a graceful shutdown (10s drain).

## HTTP API

`POST /v1/check`

```sh
curl -s localhost:8080/v1/check -d '{"ip":"81.2.69.142","allowed_countries":["GB","US"]}'
# {"match":true}
```

- `ip`: IPv4 or IPv6 (IPv4-mapped IPv6 is treated as IPv4). Send a bare address: square brackets
  (`[2001:db8::1]`) and port suffixes (`1.2.3.4:443`, `[2001:db8::1]:443`) are not currently accepted and
  return 400 / `INVALID_ARGUMENT`. Support could be added, e.g. by falling back to `netip.ParseAddrPort` in
  `parseIP`; strip them in the load balancer or caller until then.
- `allowed_countries`: non-empty list of ISO 3166-1 alpha-2 codes, case-insensitive.
- An address not present in the database returns `{"match":false}`.
- Errors return `{"error":"..."}`: `400` for a bad IP, bad country code, empty list, malformed JSON or
  unknown fields; `405` for non-POST; `413` for bodies over 1 MiB.

`GET /healthz` returns `200`.

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
filesystem, `/healthz` probes) and a `Service` exposing `http` on 80 and `grpc` (h2c) on 9090. Set the
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
