# opod worker — SGLang on the OFFICIAL lmsysorg image.
#   nvidia: BASE=lmsysorg/sglang:v0.5.2-cu126     amd: BASE=lmsysorg/sglang:v0.5.2-rocm630
#
# The same shape as images/worker-vllm: the base brings the engine and its CUDA
# stack, we add the opod binary, the catalog and the shared entrypoint. Nothing
# SGLang-specific lives in the image — the engine's flags and its metric names
# are the driver's business (internal/engines/sglang).
ARG BASE=lmsysorg/sglang:v0.5.2-cu126
# Stage `opod` = the binary. CI compiles it here; the LOCAL lane (images/build.sh)
# overrides this stage with a prebuilt cross-compiled binary via
# `--build-context opod=dist/linux-amd64` so nothing compiles under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS opod
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

FROM ${BASE}
RUN (apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash && rm -rf /var/lib/apt/lists/*) || true
COPY --from=opod /opod /usr/local/bin/opod
COPY catalog /usr/local/share/opod/catalog
COPY images/entrypoint.sh /usr/local/bin/entrypoint.sh
ENV OPOD_ROLE=worker OPOD_ENGINE=sglang OPOD_DATA_DIR=/var/lib/opod OPOD_MODELS_DIR=/data/models OPOD_NO_UPDATE_CHECK=1
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
