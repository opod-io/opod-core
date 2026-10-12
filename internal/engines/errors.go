package engines

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Error classes. Drivers wrap their failures in one of these so callers
// (API layer, router, CLI) branch with errors.Is instead of matching on
// message text. The conformance suite (internal/engines/enginetest) checks
// every driver produces them.
var (
	// ErrUnloadNotSupported is returned by Engine.Unload when the engine
	// has no protocol-level unload operation (vLLM, MLX-LM, llama-server).
	// Callers should surface it as a soft warning, not a hard failure —
	// the user can always restart the engine if they need the memory back.
	ErrUnloadNotSupported = errors.New("engine does not support unload")

	// ErrUnreachable classifies transport failures: connection refused,
	// DNS, timeouts — the engine process is not answering at its endpoint.
	ErrUnreachable = errors.New("engine unreachable")

	// ErrUpstream classifies a non-2xx response from an engine that IS
	// answering: bad model name, auth, server-side failure.
	ErrUpstream = errors.New("engine returned an error")
)

// UnreachableError is the concrete error behind ErrUnreachable. The
// underlying transport error stays reachable through Unwrap so existing
// substring classifiers ("connection refused", "no such host") keep working.
type UnreachableError struct {
	Engine   string
	Endpoint string
	Err      error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("%s unreachable at %s: %v", e.Engine, e.Endpoint, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrUnreachable) true for every UnreachableError.
func (e *UnreachableError) Is(target error) bool { return target == ErrUnreachable }

// Unreachable wraps a transport failure against engine at endpoint.
func Unreachable(engine, endpoint string, err error) error {
	return &UnreachableError{Engine: engine, Endpoint: endpoint, Err: err}
}

// UpstreamError is the concrete error behind ErrUpstream: the engine
// answered op with a non-2xx status. Body is the (trimmed) response body,
// which for OpenAI-style servers carries the actual reason.
type UpstreamError struct {
	Engine string
	Op     string
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	msg := fmt.Sprintf("%s %s: %d %s", e.Engine, e.Op, e.Status, http.StatusText(e.Status))
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Is makes errors.Is(err, ErrUpstream) true for every UpstreamError.
func (e *UpstreamError) Is(target error) bool { return target == ErrUpstream }

// Upstream wraps a non-2xx response to op (e.g. "chat", "list models").
func Upstream(engine, op string, status int, body []byte) error {
	return &UpstreamError{Engine: engine, Op: op, Status: status, Body: strings.TrimSpace(string(body))}
}

// ErrStreamBroken classifies a stream that ended in a transport failure after
// the engine had answered 200: the connection dropped or the body was cut
// short (`unexpected EOF`) before the stream's own end. Not the engine's own
// error — a chunk that does not decode, or an error the engine sent in the
// stream, is something else.
var ErrStreamBroken = errors.New("engine stream broke")

// StreamBrokenError is the concrete error behind ErrStreamBroken. Its text is
// "stream: <transport error>", what a broken stream has always read.
type StreamBrokenError struct {
	Engine string
	Err    error
}

func (e *StreamBrokenError) Error() string { return "stream: " + e.Err.Error() }

func (e *StreamBrokenError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrStreamBroken) true for every StreamBrokenError.
func (e *StreamBrokenError) Is(target error) bool { return target == ErrStreamBroken }

// StreamBroken wraps the transport failure that cut engine's stream short.
func StreamBroken(engine string, err error) error {
	return &StreamBrokenError{Engine: engine, Err: err}
}

// ErrStreamErrored classifies a stream in which the server, having answered
// 200, sent an error event of its own — an OpenAI-shaped `data: {"error":
// {...}}` chunk — instead of the rest of the answer. It is the server's
// answer, not a transport break: it is never replayed on another worker, and
// the caller sees it as an error, never as a clean end.
var ErrStreamErrored = errors.New("engine reported an error in its stream")

// StreamBrokenType is the `type` a worker puts on the in-band error chunk it
// relays when its OWN engine's stream broke in transport (ErrStreamBroken):
// `data: {"error":{"message":"stream: unexpected EOF","type":"stream_broken"}}`.
// The leader's stream reader reads that chunk back as a StreamBrokenError, so
// a worker whose engine broke before the first event is retried on a sibling
// exactly like a worker whose connection broke. Any other type, or none (a
// worker older than this field), is a StreamErroredError: shown, never
// replayed. An older leader ignores the field.
const StreamBrokenType = "stream_broken"

// StreamErroredError is the concrete error behind ErrStreamErrored: the error
// event's message and type as the server sent them.
type StreamErroredError struct {
	Engine  string
	Type    string // the event's error.type, when it had one
	Message string
}

func (e *StreamErroredError) Error() string {
	if e.Type != "" {
		return "stream error (" + e.Type + "): " + e.Message
	}
	return "stream error: " + e.Message
}

// Is makes errors.Is(err, ErrStreamErrored) true for every StreamErroredError.
func (e *StreamErroredError) Is(target error) bool { return target == ErrStreamErrored }

// ErrBrokeBeforeFirstEvent classifies a stream that broke (ErrStreamBroken)
// before it produced its FIRST event. Nothing of the answer exists anywhere
// yet — no token was sent to the caller, no byte written — so the router may
// ask another worker or gang for the same request, exactly as it does for an
// engine it could not reach (ErrUnreachable). A stream that broke after an
// event is never classified so: the caller already has part of an answer.
var ErrBrokeBeforeFirstEvent = errors.New("engine stream broke before its first event")

// BrokeBeforeFirstEventError is the concrete error behind
// ErrBrokeBeforeFirstEvent; Err is the StreamBrokenError it carries.
type BrokeBeforeFirstEventError struct {
	Err error
}

func (e *BrokeBeforeFirstEventError) Error() string {
	return "the engine answered and its stream broke before the first event: " + e.Err.Error()
}

func (e *BrokeBeforeFirstEventError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrBrokeBeforeFirstEvent) true for every one of them.
func (e *BrokeBeforeFirstEventError) Is(target error) bool { return target == ErrBrokeBeforeFirstEvent }

// ErrUnsupportedRequest classifies a request this ENGINE cannot serve as
// asked — not a transport failure and not the engine's own error, but a
// field the driver knows it cannot honour. The caller turns it into a 400
// naming the field, so a capability gap is a refusal the caller can act on
// rather than an answer that quietly ignored what they sent.
var ErrUnsupportedRequest = errors.New("engine cannot serve this request as asked")

// UnsupportedRequestError is the concrete error behind ErrUnsupportedRequest.
type UnsupportedRequestError struct {
	Engine string // the driver that cannot do it
	Field  string // the request field, as the caller spelled it
	Why    string // one clause, and what to send instead
}

func (e *UnsupportedRequestError) Error() string {
	msg := fmt.Sprintf("%s cannot serve `%s`", e.Engine, e.Field)
	if e.Why != "" {
		msg += ": " + e.Why
	}
	return msg
}

// Is makes errors.Is(err, ErrUnsupportedRequest) true for every one of them.
func (e *UnsupportedRequestError) Is(target error) bool { return target == ErrUnsupportedRequest }

// Unsupported reports that engine cannot honour field, and why.
func Unsupported(engine, field, why string) error {
	return &UnsupportedRequestError{Engine: engine, Field: field, Why: why}
}
