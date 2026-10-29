package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type bodyKey struct{}

// BodyReadError answers a failed body read the way the surface promises: a
// body over its route's cap is 413 `request_too_large` naming the cap, and
// anything else is 400 `invalid_request`. OpenAI-shaped, like every other
// error here (PLAN T15.3).
func BodyReadError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			fmt.Sprintf("request body exceeds this route's limit of %d bytes", tooBig.Limit))
		return
	}
	writeJSONError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
}

// bufferedBody is a request body read once, shared by every middleware and
// the handler behind it through the request context.
type bufferedBody struct{ bytes []byte }

// requestBody returns the request's body bytes, reading them at most ONCE per
// request. The first caller reads and stashes them on the context; every
// later caller — the next middleware, the handler — gets the same bytes and
// the same request. r.Body is left readable (a reader over the stash) for
// anything that still reads it directly.
//
// Before this, ModelAllowMiddleware, RateLimitMiddleware and ChatCompletions
// each io.ReadAll'd the body and handed the next a fresh reader over their own
// copy, so one POST was three copies resident for its whole life (PLAN T15.2).
//
// The returned request must be the one passed on: the stash rides on its
// context.
func requestBody(r *http.Request) ([]byte, *http.Request, error) {
	if b, ok := r.Context().Value(bodyKey{}).(*bufferedBody); ok {
		return b.bytes, r, nil
	}
	data, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return nil, r, err
	}
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, &bufferedBody{bytes: data}))
	r.Body = io.NopCloser(bytes.NewReader(data))
	return data, r, nil
}
