DB ?= data/GeoLite2-Country.mmdb

# fetch-db needs GNU tar (--wildcards). Use `tar` where it is GNU tar (Linux),
# otherwise `gtar` (macOS: brew install gnu-tar). Override with `make TAR=...`.
TAR ?= $(shell tar --version 2>/dev/null | grep -q 'GNU tar' && echo tar || echo gtar)

.PHONY: build test test-integration test-integration-real proto fetch-db docker-build run

build:
	go build -o bin/iplocationmatch ./cmd/iplocationmatch

test:
	go test -race ./...

test-integration:
	go test -race -count=1 -tags integration ./test/integration

# Also runs the suite against the real database at $(DB) (see fetch-db).
test-integration-real:
	IPLOCATIONMATCH_TEST_DB=$(abspath $(DB)) go test -race -count=1 -tags integration ./test/integration

# Regenerates gen/ from proto/. Needs buf, protoc-gen-go and protoc-gen-go-grpc on PATH.
proto:
	buf lint && buf generate

# Downloads GeoLite2-Country from MaxMind (free account required).
#   make fetch-db MAXMIND_ACCOUNT_ID=123456 MAXMIND_LICENSE_KEY=xxxx
fetch-db:
	@test -n "$(MAXMIND_ACCOUNT_ID)" -a -n "$(MAXMIND_LICENSE_KEY)" || { echo "set MAXMIND_ACCOUNT_ID and MAXMIND_LICENSE_KEY"; exit 1; }
	@mkdir -p data && tmp=$$(mktemp -d) && \
	curl -fsSL -u "$(MAXMIND_ACCOUNT_ID):$(MAXMIND_LICENSE_KEY)" \
	  "https://download.maxmind.com/geoip/databases/GeoLite2-Country/download?suffix=tar.gz" -o $$tmp/db.tgz && \
	$(TAR) -xzf $$tmp/db.tgz -C $$tmp --strip-components=1 --wildcards '*.mmdb' && \
	install -m 0444 $$tmp/GeoLite2-Country.mmdb $(DB) && rm -rf $$tmp && echo "wrote $(DB)"

# Builds the container image. The database is copied in, not downloaded: run fetch-db first.
IMAGE ?= iplocationmatch:dev
docker-build:
	@test -f data/GeoLite2-Country.mmdb || { echo "data/GeoLite2-Country.mmdb not found; run 'make fetch-db' first"; exit 1; }
	docker build -t $(IMAGE) .

run: build
	./bin/iplocationmatch -db $(DB)
