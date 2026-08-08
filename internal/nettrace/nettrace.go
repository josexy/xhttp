// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package nettrace contains the trace types referenced by relocated tests.
// Production tracing uses github.com/josexy/xhttp/httptrace, which forwards to the standard
// library implementation and its private context key.
package nettrace

// TraceKey is a context value key whose value is a *Trace.
type TraceKey struct{}

// LookupIPAltResolverKey is used by tests to describe an alternate resolver.
// The standard net package cannot observe this relocated key.
type LookupIPAltResolverKey struct{}

// Trace contains hooks for tracing activity in a network dial.
type Trace struct {
	DNSStart     func(name string)
	DNSDone      func(netIPs []any, coalesced bool, err error)
	ConnectStart func(network, addr string)
	ConnectDone  func(network, addr string, err error)
}
