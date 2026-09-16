# opod worker — vLLM on Tenstorrent, built on Tenstorrent's own tt-metal image.
#
# Why this one is shaped differently from the other workers: tt-metal's vLLM is
# not `vllm serve`. It is a wrapper (run_vllm_api_server.py) configured by env
# and a model spec, started through Tenstorrent's own entrypoint, which loads
# the model at startup. So opod does not launch it — OPOD_ENGINE_START does, and
# opod connects to the OpenAI API it exposes. That is precisely the contract
# core's vLLM driver already documents, and why the driver registers the
# aliases tt-openai/tenstorrent/tt: the wire protocol is the same, so there is
# no Tenstorrent-specific code anywhere in core.
#
# Verified on a Blackhole p150b Quietbox 2026-09-11: tt-metal opened all four
# chips through tt-kmd 2.11.0. Serving additionally needs the host's 1 GB
# hugepages mounted at /dev/hugepages-1G inside the container: without them the
# chips open but firmware init asserts (channel < get_num_host_channels()), so
# the pod spec must mount them — see README.md.
# The base is not "the latest tt image": each tt-inference-server model spec
# names the tt-metal and vLLM commits it was validated on for one (model,
# device) pair, and the image must carry exactly those. In release 0.10.1 the
# Blackhole P150 / P150X4 LLM specs (Llama-3.1-8B, Llama-3.1/3.3-70B) name
# 55fd115 + aa4ae1e, so that is the default; the 555f240-22be241 build carries
# LLM specs only for P150X8 and P300. Choose by the spec, per endpoint:
#   images/build.sh --tag <t> vllm-tt  with  VLLM_TT_BASE=<image for that spec>
ARG BASE=ghcr.io/tenstorrent/tt-inference-server/vllm-tt-metal-src-release-ubuntu-22.04-amd64:0.10.0-55fd115-aa4ae1e

FROM --platform=$BUILDPLATFORM golang:1.26 AS opod
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /opod ./cmd/opod

FROM ${BASE}
# The base runs as root and de-escalates in its own entrypoint; keep that intact
# by invoking it from OPOD_ENGINE_START rather than replacing it.
USER root
RUN (apt-get update && apt-get install -y --no-install-recommends ca-certificates curl bash && rm -rf /var/lib/apt/lists/*) || true
COPY --from=opod /opod /usr/local/bin/opod
COPY images/entrypoint.sh /usr/local/bin/opod-entrypoint.sh
ENV OPOD_ROLE=worker \
    OPOD_ENGINE=vllm \
    OPOD_VLLM_ENDPOINT=http://127.0.0.1:8000 \
    OPOD_DATA_DIR=/var/lib/opod \
    OPOD_MODELS_DIR=/data/models \
    OPOD_NO_UPDATE_CHECK=1 \
    OPOD_ENGINE_START="/usr/local/bin/docker-entrypoint.sh /bin/bash -c 'source ${PYTHON_ENV_DIR}/bin/activate && python run_vllm_api_server.py'"
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/opod-entrypoint.sh"]
