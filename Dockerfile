# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.27.4 AS build
# Fail rather than silently download a different toolchain than go.mod asks for.
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY gen ./gen
RUN go build -trimpath -ldflags="-s -w" -o /out/iplocationmatch ./cmd/iplocationmatch

# ---- runtime ----
FROM gcr.io/distroless/static:nonroot
# The database is NOT fetched here. Run `make fetch-db` first so that
# data/GeoLite2-Country.mmdb exists; if it does not, this COPY fails the build.
# Keep the resulting image in a private registry: GeoLite2 is licensed by MaxMind.
COPY data/GeoLite2-Country.mmdb /data/GeoLite2-Country.mmdb
COPY --from=build /out/iplocationmatch /iplocationmatch

ENV IPLOCATIONMATCH_DB=/data/GeoLite2-Country.mmdb
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/iplocationmatch"]
