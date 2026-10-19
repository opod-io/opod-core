package leader

// Vertex AI's gRPC PredictionService on the leader's own port (ADR-092 §1,
// PLAN T19.5): ChatCompletions and StreamRawPredict (server streams) and
// RawPredict (unary) carry OpenAI JSON inside a google.api.HttpBody, so a call
// needs no translation — it is framing. Each call becomes an internal POST
// /v1/chat/completions that runs through the leader's own /v1 chain, so keys
// (from the call's `authorization` / `x-api-key` metadata), per-key limits,
// the stale-auth gate, /loadz and the usage row (protocol grpc-vertex) are
// the chat route's, unchanged. The answer goes back as HttpBody messages: one
// JSON body, or one SSE event per message for a stream.
//
// The server is grpc-go's, mounted as an http.Handler (HTTP/2: h2 by ALPN on
// the TLS listener, h2c on a plain one), with a codec that passes the frames
// through as bytes and one handler for the service: the two request messages
// are decoded here with protowire, so no generated Vertex code is vendored.
// Every other method is Unimplemented.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/opod-io/opod/internal/api"
)

// vertexGRPCService is the service's full name; its methods are served at
// /<service>/<method>.
const vertexGRPCService = "google.cloud.aiplatform.v1.PredictionService"

// ProtocolGRPCVertex is the usage row's protocol for a call on it.
const ProtocolGRPCVertex = "grpc-vertex"

// rawCodec hands a gRPC frame to the handler as bytes and sends bytes back:
// the handler owns the (two, tiny) message formats.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("grpc: cannot send %T", v)
}
func (rawCodec) Unmarshal(data []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("grpc: cannot receive into %T", v)
	}
	*p = append((*p)[:0], data...)
	return nil
}

// newVertexGRPC returns the gRPC server; chat serves each call's chat request
// (the leader's own router, so the /v1 chain applies).
func newVertexGRPC(chat http.Handler) *grpc.Server {
	//nolint:staticcheck // ForceServerCodec is the documented way to take frames as bytes
	return grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		method, _ := grpc.MethodFromServerStream(stream)
		svc, name, _ := strings.Cut(strings.TrimPrefix(method, "/"), "/")
		if svc != vertexGRPCService {
			return status.Errorf(codes.Unimplemented, "unknown service %s", svc)
		}
		var unary bool
		switch name {
		case "ChatCompletions", "StreamRawPredict":
		case "RawPredict":
			unary = true
		default:
			return status.Errorf(codes.Unimplemented, "unsupported_request: %s.%s is not served here — ChatCompletions, StreamRawPredict and RawPredict carrying a chat request are", svc, name)
		}
		var in []byte
		if err := stream.RecvMsg(&in); err != nil {
			return err
		}
		body, err := vertexHTTPBody(in)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		var asks struct {
			Stream bool `json:"stream"`
		}
		if unary && json.Unmarshal(body, &asks) == nil && asks.Stream {
			return status.Error(codes.InvalidArgument, "unsupported_request: RawPredict answers one body — call StreamRawPredict or ChatCompletions to stream")
		}
		return serveVertexGRPC(stream, chat, body)
	}))
}

// vertexHTTPBody is the chat JSON inside a ChatCompletionsRequest,
// StreamRawPredictRequest or RawPredictRequest: field 1 is the endpoint name
// (Vertex's routing, ignored here), field 2 the google.api.HttpBody.
func vertexHTTPBody(msg []byte) ([]byte, error) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return nil, fmt.Errorf("malformed request: %w", protowire.ParseError(n))
		}
		msg = msg[n:]
		if num == 2 && typ == protowire.BytesType {
			raw, n := protowire.ConsumeBytes(msg)
			if n < 0 {
				return nil, fmt.Errorf("malformed http_body: %w", protowire.ParseError(n))
			}
			var hb httpbody.HttpBody
			if err := proto.Unmarshal(raw, &hb); err != nil {
				return nil, fmt.Errorf("malformed http_body: %w", err)
			}
			if len(hb.GetData()) == 0 {
				return nil, fmt.Errorf("http_body.data is empty: it carries the chat request")
			}
			return hb.GetData(), nil
		}
		n = protowire.ConsumeFieldValue(num, typ, msg)
		if n < 0 {
			return nil, fmt.Errorf("malformed request: %w", protowire.ParseError(n))
		}
		msg = msg[n:]
	}
	return nil, fmt.Errorf("the request carries no http_body: it carries the chat request")
}

// serveVertexGRPC runs the chat request through the leader's /v1 route and
// streams its answer back as HttpBody messages.
func serveVertexGRPC(stream grpc.ServerStream, chat http.Handler, body []byte) error {
	ctx := api.WithProtocol(stream.Context(), ProtocolGRPCVertex)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "grpc"
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		for _, h := range []string{"authorization", "x-api-key", "x-opod-class", "x-request-id"} {
			if v := md.Get(h); len(v) > 0 {
				req.Header.Set(h, v[0])
			}
		}
	}
	w := &grpcBodyWriter{stream: stream, header: http.Header{}, status: http.StatusOK}
	chat.ServeHTTP(w, req)
	return w.finish()
}

// grpcBodyWriter is the chat route's answer, re-framed: a stream's SSE events
// go out one HttpBody each as they arrive; a whole answer goes out once; an
// error becomes the gRPC status for its HTTP status.
type grpcBodyWriter struct {
	stream  grpc.ServerStream
	header  http.Header
	status  int
	wrote   bool
	sse     bool
	buf     bytes.Buffer
	sendErr error
}

func (g *grpcBodyWriter) Header() http.Header { return g.header }

func (g *grpcBodyWriter) WriteHeader(code int) {
	if g.wrote {
		return
	}
	g.wrote, g.status = true, code
	g.sse = code < 300 && strings.HasPrefix(g.header.Get("Content-Type"), "text/event-stream")
}

func (g *grpcBodyWriter) Write(p []byte) (int, error) {
	if !g.wrote {
		g.WriteHeader(http.StatusOK)
	}
	g.buf.Write(p)
	if g.sse {
		g.drain()
	}
	return len(p), nil
}

// Flush satisfies the chat route's streaming writer; events are sent on Write.
func (g *grpcBodyWriter) Flush() {}

func (g *grpcBodyWriter) send(contentType string, data []byte) {
	if g.sendErr != nil {
		return
	}
	b, err := proto.Marshal(&httpbody.HttpBody{ContentType: contentType, Data: data})
	if err == nil {
		err = g.stream.SendMsg(b)
	}
	g.sendErr = err
}

// drain sends every complete SSE event in the buffer.
func (g *grpcBodyWriter) drain() {
	for {
		raw := g.buf.Bytes()
		end := bytes.Index(raw, []byte("\n\n"))
		if end < 0 {
			return
		}
		g.send("text/event-stream", append([]byte(nil), raw[:end+2]...))
		g.buf.Next(end + 2)
	}
}

func (g *grpcBodyWriter) finish() error {
	if g.sendErr != nil {
		return g.sendErr
	}
	if g.status >= 300 {
		return status.Error(grpcCode(g.status), strings.TrimSpace(g.buf.String()))
	}
	if !g.sse {
		g.send(g.header.Get("Content-Type"), g.buf.Bytes())
	}
	return g.sendErr
}

// grpcCode is the gRPC status for the chat route's HTTP status.
func grpcCode(httpStatus int) codes.Code {
	switch httpStatus {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return codes.InvalidArgument
	case http.StatusUnauthorized:
		return codes.Unauthenticated
	case http.StatusForbidden:
		return codes.PermissionDenied
	case http.StatusNotFound:
		return codes.NotFound
	case http.StatusTooManyRequests:
		return codes.ResourceExhausted
	case http.StatusServiceUnavailable:
		return codes.Unavailable
	case http.StatusGatewayTimeout:
		return codes.DeadlineExceeded
	default:
		return codes.Internal
	}
}
