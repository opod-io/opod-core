package adminapi

// The usage push (feature usage_push): a leader sends one OTLP log record per
// request it served to the URL in OPOD_USAGE_OTLP_ENDPOINT, and drops it from
// its bounded queue only when the receiver answers 2xx. These are the names on
// that record, spelled once for the leader that writes them and for any
// receiver that reads them. OpenTelemetry's GenAI and user conventions are used
// where they have a name; the rest are opod's own.
//
// The dedupe key is (UsageAttrLeaderBoot, UsageAttrSeq): the leader's process
// start in unix seconds and a sequence that starts at 1 on every boot. A
// receiver that stores a record once per key may take a resend safely.
const (
	// UsageEventName is the record's event name.
	UsageEventName = "opod.usage"

	// Resource attributes (one set per push).
	UsageAttrServiceName = "service.name" // "opod-leader"
	UsageAttrLeaderBoot  = "opod.leader.boot"

	// Record attributes.
	UsageAttrSeq          = "opod.usage.seq"
	UsageAttrRequestID    = "opod.request.id"
	UsageAttrModel        = "gen_ai.request.model"
	UsageAttrInputTokens  = "gen_ai.usage.input_tokens"
	UsageAttrOutputTokens = "gen_ai.usage.output_tokens"
	UsageAttrAPIKeyID     = "opod.api_key.id"
	UsageAttrUserID       = "user.id"
	UsageAttrProtocol     = "opod.protocol"
	UsageAttrOutcome      = "opod.outcome"
	UsageAttrNodeID       = "opod.node.id"
	UsageAttrLatencyMS    = "opod.latency_ms"
	UsageAttrTTFTMS       = "opod.ttft_ms"
	UsageAttrGateway      = "opod.gateway" // the front door that served it; absent = the leader
	// T32.47: prompt tokens the engine served from its prefix cache (0 = none
	// reported; vLLM, SGLang, llama.cpp and TensorRT-LLM report it, Ollama
	// does not), the admission
	// class that ranked the request (absent = no classes in the policy), and
	// what the request did: chat | embeddings | rerank (how it was spoken —
	// OpenAI, Anthropic Messages, Vertex — is UsageAttrProtocol).
	UsageAttrCachedTokens = "gen_ai.usage.cache_read.input_tokens"
	UsageAttrClass        = "opod.request.class"
	UsageAttrOperation    = "gen_ai.operation.name"
)
