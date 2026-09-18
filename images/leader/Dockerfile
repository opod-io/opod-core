# opod leader — router-only gateway for ONE endpoint (no GPU, no engine).
# Stage `opod` = the binary. CI compiles it here; the LOCAL lane (images/build.sh)
# overrides this stage with a prebuilt cross-compiled binary via
# `--build-context opod=dist/linux-amd64` so nothing compiles under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3c3e25a4da13fd0478eed2df1eb35a0e667094a7124d3993a6a1d30f71c17e79 AS opod
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Cross-compile from the BUILD platform: a two-arch index must never compile Go
# under emulation (the arm64 half of the leader image is what a kind cluster on
# Apple silicon pulls).
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /opod ./cmd/opod

FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash && rm -rf /var/lib/apt/lists/*
COPY --from=opod /opod /usr/local/bin/opod
COPY images/entrypoint.sh /usr/local/bin/entrypoint.sh
ENV OPOD_ROLE=leader OPOD_LISTEN=:8080 OPOD_DATA_DIR=/var/lib/opod OPOD_PULL_DEFAULT_MODEL=false OPOD_NO_UPDATE_CHECK=1
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
