package openaicompat

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// engineKeepAlive is the shortest idle keep-alive among the engines this
// client talks to: llama-server (cpp-httplib), vLLM and SGLang (uvicorn) all
// close an idle connection after 5 s.
const engineKeepAlive = 5 * time.Second

func chatOnce(t *testing.T, c Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := c.Chat(ctx, engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		return err
	}
	for range ch {
	}
	return nil
}

const sseAnswer = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// countingEngine is an OpenAI server that counts the chat requests it served
// and the connections it accepted, and closes a connection that sat idle for
// idle — what every engine does after its keep-alive timeout.
func countingEngine(t *testing.T, idle time.Duration) (srv *httptest.Server, hits, conns *atomic.Int32) {
	hits, conns = new(atomic.Int32), new(atomic.Int32)
	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseAnswer)
	}))
	srv.Config.IdleTimeout = idle
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, hits, conns
}

// The client must retire a pooled connection before the engine does, or a
// request can take a connection from the pool in the instant the engine closes
// it — "server closed idle connection", a 502 for an engine that was fine.
func TestTheClientRetiresAnIdleConnectionBeforeTheEngines(t *testing.T) {
	if IdleConnTimeout >= engineKeepAlive {
		t.Fatalf("IdleConnTimeout %s is not below the engines' %s keep-alive", IdleConnTimeout, engineKeepAlive)
	}
	c := NewClient("llamacpp", "http://x", nil)
	if tr, ok := c.HTTP.Transport.(*http.Transport); !ok || tr.IdleConnTimeout != IdleConnTimeout {
		t.Fatalf("the engine client's transport does not carry IdleConnTimeout")
	}

	// An engine that would keep the connection longer than the client: after
	// the client's idle timeout the next request is on a NEW connection — the
	// client dropped the old one first — and is served exactly once.
	srv, hits, conns := countingEngine(t, IdleConnTimeout+2*time.Second)
	c = NewClient("llamacpp", srv.URL, nil)
	if err := chatOnce(t, c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(IdleConnTimeout + 500*time.Millisecond)
	if err := chatOnce(t, c); err != nil {
		t.Fatalf("the request after an idle pause failed: %v", err)
	}
	if h, n := hits.Load(), conns.Load(); h != 2 || n != 2 {
		t.Fatalf("engine served %d requests on %d connections, want 2 on 2 (the client retires the idle one itself)", h, n)
	}
}

// An engine that closed the idle connection itself: the next request succeeds
// and reaches the engine exactly once.
func TestARequestAfterTheEngineClosedTheIdleConnectionIsServedOnce(t *testing.T) {
	srv, hits, _ := countingEngine(t, 200*time.Millisecond)
	c := NewClient("llamacpp", srv.URL, nil)
	if err := chatOnce(t, c); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond) // the engine has closed it; the client has not
	if err := chatOnce(t, c); err != nil {
		t.Fatalf("the request after the engine closed the idle connection failed: %v", err)
	}
	if h := hits.Load(); h != 2 {
		t.Fatalf("engine-side hits = %d, want 2 (one per request)", h)
	}
}

// An engine that READ a request and then dropped the connection without an
// answer may have acted on it: the request is never sent a second time — the
// caller gets the error. The connection is a reused one, the only kind Go
// ever retries on.
func TestARequestTheEngineReceivedAndDroppedIsNotSentTwice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var hits atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for n := 1; ; n++ {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					_, _ = io.Copy(io.Discard, req.Body)
					hits.Add(1)
					if n == 2 {
						return // received in full, then dropped with no answer
					}
					_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: "+strconv.Itoa(len(sseAnswer))+"\r\n\r\n"+sseAnswer)
				}
			}(conn)
		}
	}()
	c := NewClient("llamacpp", "http://"+ln.Addr().String(), nil)
	if err := chatOnce(t, c); err != nil {
		t.Fatal(err)
	}
	err = chatOnce(t, c)
	if !errors.Is(err, engines.ErrUnreachable) {
		t.Fatalf("want the dropped request to fail as unreachable, got %v", err)
	}
	time.Sleep(200 * time.Millisecond) // a retry, if one were made, would have landed
	if h := hits.Load(); h != 2 {
		t.Fatalf("engine-side hits = %d, want 2: the dropped request was sent again", h)
	}
}
