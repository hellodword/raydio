# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26
ARG RUNTIME_UID=65532
ARG RUNTIME_GID=65532

FROM golang:${GO_VERSION}-bookworm AS build-base
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=1
ENV GOFLAGS="-trimpath -buildvcs=false"
ENV RAYDIO_LDFLAGS="-s -w -buildid= -extldflags=-Wl,--build-id=none"

RUN mkdir -p /out/rootfs/srv/raydio/data

FROM build-base AS build-raydio
RUN go build -mod=readonly -ldflags="${RAYDIO_LDFLAGS}" -o /out/raydio ./cmd/raydio

FROM build-base AS build-suno-worker
RUN go build -mod=readonly -ldflags="${RAYDIO_LDFLAGS}" -o /out/suno-worker ./cmd/suno-worker

FROM build-base AS build-raydio-worker
RUN go build -mod=readonly -ldflags="${RAYDIO_LDFLAGS}" -o /out/raydio-worker ./cmd/raydio-worker

FROM gcr.io/distroless/base-debian12:nonroot AS raydio
WORKDIR /srv/raydio
COPY --from=build-base --chown=65532:65532 /out/rootfs/srv/raydio /srv/raydio
COPY --from=build-raydio --chown=65532:65532 /out/raydio /usr/local/bin/raydio
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/raydio"]

FROM gcr.io/distroless/base-debian12:nonroot AS suno-worker
WORKDIR /srv/raydio
COPY --from=build-base --chown=65532:65532 /out/rootfs/srv/raydio /srv/raydio
COPY --from=build-suno-worker --chown=65532:65532 /out/suno-worker /usr/local/bin/suno-worker
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/suno-worker"]

FROM debian:bookworm-slim AS raydio-worker
ARG RUNTIME_UID=65532
ARG RUNTIME_GID=65532
RUN set -eux; \
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates ffmpeg; \
	rm -rf /var/lib/apt/lists/*; \
	groupadd --gid "${RUNTIME_GID}" nonroot; \
	useradd --uid "${RUNTIME_UID}" --gid "${RUNTIME_GID}" --home-dir /srv/raydio --create-home --shell /usr/sbin/nologin nonroot; \
	mkdir -p /srv/raydio/data; \
	chown -R "${RUNTIME_UID}:${RUNTIME_GID}" /srv/raydio
WORKDIR /srv/raydio
COPY --from=build-raydio-worker --chown=${RUNTIME_UID}:${RUNTIME_GID} /out/raydio-worker /usr/local/bin/raydio-worker
USER ${RUNTIME_UID}:${RUNTIME_GID}
ENTRYPOINT ["/usr/local/bin/raydio-worker"]
