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
