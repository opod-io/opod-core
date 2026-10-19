package leader

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// vertexRequest is a ChatCompletionsRequest / StreamRawPredictRequest /
// RawPredictRequest: field 1 the endpoint, field 2 the HttpBody.
func vertexRequest(t *testing.T, chat string) []byte {
	t.Helper()
	hb, err := proto.Marshal(&httpbody.HttpBody{ContentType: "application/json", Data: []byte(chat)})
	if err != nil {
		t.Fatal(err)
	}
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, "projects/p/locations/l/endpoints/e")
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendBytes(b, hb)
}

// T19.5: the PredictionService is served on the leader's port over HTTP/2, and
// a call runs through the /v1 chain — so the key, the admission gate and the
// refusals are the chat route's, as gRPC statuses.
func TestVertexGRPCOnTheLeaderPort(t *testing.T) {
	srv := drainedLeader(t) // m's only worker is drained: nothing can serve it
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p0"})
	srv.cfg.Auth.RequireKeys = true
	ts := httptest.NewUnstartedServer(srv.routes())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()
	conn, err := grpc.NewClient(strings.TrimPrefix(ts.URL, "https://"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})), //nolint:gosec // a test server's own certificate
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	call := func(ctx context.Context, method string, req []byte) ([][]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		st, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/"+vertexGRPCService+"/"+method)
		if err != nil {
			return nil, err
		}
		if err := st.SendMsg(req); err != nil {
			return nil, err
		}
		_ = st.CloseSend()
		var out [][]byte
		for {
			var b []byte
			if err := st.RecvMsg(&b); err == io.EOF {
				return out, nil
			} else if err != nil {
				return out, err
			}
			out = append(out, b)
		}
	}
	chat := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	codeOf := func(err error) codes.Code { return status.Code(err) }

	if _, err := call(context.Background(), "ChatCompletions", vertexRequest(t, chat)); codeOf(err) != codes.Unauthenticated {
		t.Fatalf("no key, keys required: %v", err)
	}
	keyed := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+srv.cfg.Auth.AdminToken)
	if srv.cfg.Auth.AdminToken == "" {
		keyed = context.Background()
		srv.cfg.Auth.RequireKeys = false
	}
	if _, err := call(keyed, "StreamRawPredict", vertexRequest(t, chat)); codeOf(err) != codes.Unavailable {
		t.Fatalf("nothing serves m: %v, want Unavailable (the admission gate's 503)", err)
	}
	if _, err := call(keyed, "RawPredict", vertexRequest(t, `{"model":"m", "stream": true, "messages":[]}`)); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("a streamed RawPredict: %v", err)
	}
	if _, err := call(keyed, "Predict", vertexRequest(t, chat)); codeOf(err) != codes.Unimplemented {
		t.Fatalf("Predict: %v", err)
	}
	if _, err := call(keyed, "ChatCompletions", protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "e")); codeOf(err) != codes.InvalidArgument {
		t.Fatalf("no http_body: %v", err)
	}
}

// fakeStream records what a call sends back.
type fakeStream struct {
	grpc.ServerStream
	sent [][]byte
}

func (f *fakeStream) SendMsg(m any) error {
	f.sent = append(f.sent, m.([]byte))
	return nil
}

// A stream goes back one SSE event per HttpBody as each arrives; a whole answer
// as one body; an HTTP error as its gRPC status.
func TestGRPCBodyWriterReframes(t *testing.T) {
	st := &fakeStream{}
	w := &grpcBodyWriter{stream: st, header: http.Header{}, status: http.StatusOK}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: {\"a\":1}\n\ndata: {\"b\""))
	_, _ = w.Write([]byte(":2}\n\ndata: [DONE]\n\n"))
	if err := w.finish(); err != nil || len(st.sent) != 3 {
		t.Fatalf("stream: %v, %d messages", err, len(st.sent))
	}
	var hb httpbody.HttpBody
	if err := proto.Unmarshal(st.sent[1], &hb); err != nil || hb.GetContentType() != "text/event-stream" || string(hb.GetData()) != "data: {\"b\":2}\n\n" {
		t.Fatalf("second event: %v %q %q", err, hb.GetContentType(), hb.GetData())
	}

	st = &fakeStream{}
	w = &grpcBodyWriter{stream: st, header: http.Header{}, status: http.StatusOK}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"choices":[]}`))
	if err := w.finish(); err != nil || len(st.sent) != 1 {
		t.Fatalf("whole answer: %v, %d", err, len(st.sent))
	}

	w = &grpcBodyWriter{stream: &fakeStream{}, header: http.Header{}, status: http.StatusOK}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	if err := w.finish(); status.Code(err) != codes.ResourceExhausted || !bytes.Contains([]byte(err.Error()), []byte("slow down")) {
		t.Fatalf("429: %v", err)
	}
}
