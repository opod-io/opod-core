# opod worker — llama.cpp on the OFFICIAL ggml image (never compiled here).
#   nvidia: BASE=ghcr.io/ggml-org/llama.cpp:full-cuda   amd: BASE=ghcr.io/ggml-org/llama.cpp:full-rocm
ARG BASE=ghcr.io/ggml-org/llama.cpp:full-cuda
# Stage `opod` = the binary. CI compiles it here; the LOCAL lane (images/build.sh)
# overrides this stage with a prebuilt cross-compiled binary via
# `--build-context opod=dist/linux-amd64` so nothing compiles under emulation.
FROM golang:1.26 AS opod
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /opod ./cmd/opod

FROM ${BASE}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash && rm -rf /var/lib/apt/lists/*
COPY --from=opod /opod /usr/local/bin/opod
COPY catalog /usr/local/share/opod/catalog
COPY images/entrypoint.sh /usr/local/bin/entrypoint.sh
# ggml "full" images keep binaries in /app
ENV PATH=/app:$PATH OPOD_ROLE=worker OPOD_ENGINE=llamacpp OPOD_DATA_DIR=/var/lib/opod OPOD_MODELS_DIR=/data/models OPOD_NO_UPDATE_CHECK=1
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
