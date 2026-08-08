// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package httptrace provides mechanisms to trace the events within HTTP
// client requests.
//
// Its public types alias net/http/httptrace so contexts created by this
// package remain visible to net.Dialer, which uses a private standard-library
// context key for DNS and connection hooks.
package httptrace

import (
	"context"
	stdhttptrace "net/http/httptrace"
)

type (
	// ClientTrace is a set of hooks to run at various stages of an outgoing
	// HTTP request.
	ClientTrace = stdhttptrace.ClientTrace
	// WroteRequestInfo contains information provided to ClientTrace.WroteRequest.
	WroteRequestInfo = stdhttptrace.WroteRequestInfo
	// DNSStartInfo contains information about a DNS request.
	DNSStartInfo = stdhttptrace.DNSStartInfo
	// DNSDoneInfo contains information about the results of a DNS lookup.
	DNSDoneInfo = stdhttptrace.DNSDoneInfo
	// GotConnInfo contains information about a connection obtained by a client.
	GotConnInfo = stdhttptrace.GotConnInfo
)

// ContextClientTrace returns the ClientTrace associated with ctx, if any.
func ContextClientTrace(ctx context.Context) *ClientTrace {
	return stdhttptrace.ContextClientTrace(ctx)
}

// WithClientTrace returns a context carrying trace in addition to any trace
// already registered with ctx.
func WithClientTrace(ctx context.Context, trace *ClientTrace) context.Context {
	return stdhttptrace.WithClientTrace(ctx, trace)
}
