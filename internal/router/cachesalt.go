package router

import (
	"context"
)

// The request's cache salt (PLAN T26.8, feature "cache_salt").
//
// The leader derives one salt per caller — its team, or its key, by the
// policy's routing.cacheIsolation — and attaches it here before the request
// reaches the router. Chat puts it on the request (engines.ChatRequest
// .CacheSalt), where a driver whose engine keys its prefix cache by a salt
// sends it, and prefixKeysOf mixes it into the prefix pins, so the router
// never pins one team's request to a worker for a prefix only another team's
// salt has warmed: routing and the engine's cache agree.

type cacheSaltCtx struct{}

// WithCacheSalt attaches the request's salt. "" attaches nothing: every
// caller shares, which is routing.cacheIsolation "none".
func WithCacheSalt(ctx context.Context, salt string) context.Context {
	if salt == "" {
		return ctx
	}
	return context.WithValue(ctx, cacheSaltCtx{}, salt)
}

func cacheSaltFrom(ctx context.Context) string {
	s, _ := ctx.Value(cacheSaltCtx{}).(string)
	return s
}
