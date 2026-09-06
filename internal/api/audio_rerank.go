package api

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// RerankAudioConfig is the small slice of operator config the
// audio/rerank handlers need at runtime. Wired by the controlplane at
// startup. The endpoints are passthroughs — Opod owns the auth,
// usage logging, and rate-limit headers; the underlying engine speaks
// the actual rerank / whisper / piper protocols.
type RerankAudioConfig struct {
	// LlamaCppEndpoint is where the engine listens. Used for
	// /v1/rerank passthrough; llama-server has supported rerank
	// natively since b3580 (mid-2024) and exposes it at the same
	// path Opod proxies.
	LlamaCppEndpoint string
	// WhisperEndpoint, when set, is a `whisper-server`-shape HTTP
	// endpoint Opod proxies for /v1/audio/transcriptions. Empty →
	// the handler returns 501 with a setup hint.
	WhisperEndpoint string
	// PiperEndpoint is the equivalent for /v1/audio/speech.
	PiperEndpoint string
}

var globalRerankAudioConfig RerankAudioConfig

// SetRerankAudioConfig wires the endpoints. Idempotent; the
// controlplane calls this at startup.
func SetRerankAudioConfig(c RerankAudioConfig) { globalRerankAudioConfig = c }

// Rerank handles POST /v1/rerank — the Cohere-shape body + response,
// forwarded to llama-server. We sit in the path so auth +
// rate-limits + usage logging apply consistently across rerank and
// chat.
func (h *Handler) Rerank(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	// Pre-call guardrails apply here too — the rerank body (query +
	// documents) is plain text, and a rewrite replaces what we forward.
	body, ok := applyPreCallGuardrails(r.Context(), w, h.Store, body)
	if !ok {
		// Guardrail blocked the request; response already written.
		return
	}
	model := peekRequestedModel(body)
	endpoint := globalRerankAudioConfig.LlamaCppEndpoint
	if endpoint == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "rerank_not_configured",
			"set router.engine.llamacpp_endpoint (or OPOD_LLAMACPP_ENDPOINT) — rerank requires llama-server")
		return
	}
	endpoint = strings.TrimRight(endpoint, "/")

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		recordUsage(r.Context(), h.Store, "rerank", model, nil, time.Since(start), "error")
		writeJSONError(w, http.StatusBadGateway, "engine_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	outcome := "ok"
	if resp.StatusCode >= 400 {
		outcome = "error"
	}
	recordUsage(r.Context(), h.Store, "rerank", model, nil, time.Since(start), outcome)
}

// AudioTranscriptions handles POST /v1/audio/transcriptions. Today it
// proxies to a configured whisper-server endpoint; when unset we
// return 501 with the exact env var the operator needs to set.
// Multipart form is passed through; Opod owns the auth wrap.
func (h *Handler) AudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	endpoint := globalRerankAudioConfig.WhisperEndpoint
	if endpoint == "" {
		writeJSONError(w, http.StatusNotImplemented, "asr_not_configured",
			"/v1/audio/transcriptions is wired but no engine is configured. "+
				"Run a whisper-server (https://github.com/ggerganov/whisper.cpp) and set "+
				"OPOD_WHISPER_ENDPOINT=http://localhost:8083 (or router.engine.whisper_endpoint in config.yaml).")
		return
	}
	h.proxyAudio(w, r, endpoint, "/v1/audio/transcriptions", "audio-asr")
}

// AudioSpeech handles POST /v1/audio/speech (TTS). Same 501 story as
// transcriptions until a piper endpoint is configured.
func (h *Handler) AudioSpeech(w http.ResponseWriter, r *http.Request) {
	endpoint := globalRerankAudioConfig.PiperEndpoint
	if endpoint == "" {
		writeJSONError(w, http.StatusNotImplemented, "tts_not_configured",
			"/v1/audio/speech is wired but no engine is configured. "+
				"Run piper-server (https://github.com/rhasspy/piper) and set "+
				"OPOD_PIPER_ENDPOINT=http://localhost:8084 (or router.engine.piper_endpoint in config.yaml).")
		return
	}
	h.proxyAudio(w, r, endpoint, "/v1/audio/speech", "audio-tts")
}

// proxyAudio is the shared forwarder for the audio endpoints. Both
// whisper-server and piper-server respect the OpenAI-compatible
// shapes (multipart form for transcriptions, JSON body returning
// audio bytes for speech).
//
// Pre-call guardrails intentionally don't apply here: the bodies are
// multipart/binary audio (potentially megabytes), not the JSON text
// the guardrail hook's contract inspects or rewrites.
func (h *Handler) proxyAudio(w http.ResponseWriter, r *http.Request, endpoint, path, protocol string) {
	defer r.Body.Close()
	start := time.Now()
	endpoint = strings.TrimRight(endpoint, "/")

	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint+path, r.Body)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "proxy_error", err.Error())
		return
	}
	// Mirror the inbound Content-Type so multipart boundary etc.
	// survive the hop.
	for _, k := range []string{"Content-Type", "Accept", "Accept-Encoding"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		recordUsage(r.Context(), h.Store, protocol, "", nil, time.Since(start), "error")
		writeJSONError(w, http.StatusBadGateway, "engine_unreachable", err.Error())
		return
	}
	defer resp.Body.Close()
	copyUpstreamHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)

	outcome := "ok"
	if resp.StatusCode >= 400 {
		outcome = "error"
	}
	// Peek the model id from the multipart form / JSON body would
	// require reading the body twice; rerank uses peekRequestedModel
	// because the body is small JSON, but audio bodies may be
	// megabytes. Trade-off: usage rows for audio carry an empty
	// model id today — operators can grep by protocol.
	recordUsage(r.Context(), h.Store, protocol, "", nil, time.Since(start), outcome)
}
