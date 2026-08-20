// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/josexy/net/http/httpguts"
	"github.com/josexy/net/http2/hpack"
	"github.com/josexy/xhttp/internal/httpcommon"
)

// HeaderField is a decoded HTTP/2 header field.
//
// Name is the lowercase field name used on the wire. Sensitive reports
// whether the field used HPACK's never-indexed representation.
type HeaderField struct {
	Name      string
	Value     string
	Sensitive bool
}

// HeaderBlockKind identifies the role of an HTTP/2 HEADERS block.
type HeaderBlockKind uint8

const (
	// HeaderBlockInitial is an initial request header block or the final
	// (non-informational) response header block.
	HeaderBlockInitial HeaderBlockKind = iota + 1

	// HeaderBlockInformational is a 1xx response header block.
	HeaderBlockInformational

	// HeaderBlockTrailer is a trailer header block.
	HeaderBlockTrailer
)

// HeaderBlock contains the fields decoded from one HTTP/2 HEADERS block.
// Fields retain their wire order, including pseudo-header fields and repeated
// field names. Truncated reports that the configured header-list limit was
// reached and Fields is incomplete.
type HeaderBlock struct {
	Kind      HeaderBlockKind
	Fields    []HeaderField
	Truncated bool
}

// HeaderBlockFunc returns a header block when it is ready.
//
// It is used by WithRequestHeaderBlocks for request trailers. The transport
// calls the function after it reads the request body to EOF, which permits a
// streaming proxy to return the trailer block received from its downstream
// client without buffering the body.
type HeaderBlockFunc func() (HeaderBlock, error)

// InformationalResponseHandler handles one HTTP/2 1xx response header block.
// The handler is called synchronously from the transport read loop as soon as
// the block arrives. It must not retain or mutate shared state without its own
// synchronization.
type InformationalResponseHandler func(HeaderBlock) error

// HeaderOrder specifies the order in which HTTP/2 fields are written.
//
// Headers applies to the initial request or response header block. On a
// server it also applies to explicitly written 1xx responses. Trailers
// applies to the trailer block. Each slice contains field names, not values.
// Names not present in the block are ignored. Unlisted regular fields follow
// listed fields in lowercase lexical order.
type HeaderOrder struct {
	Headers  []string
	Trailers []string
}

type requestHeaderBlocksContextKey struct{}
type requestHeaderOrderContextKey struct{}
type requestHeaderBlocksWriteContextKey struct{}
type informationalResponseHandlerContextKey struct{}

type requestHeaderBlocksWriteConfig struct {
	initial  *HeaderBlock
	trailers HeaderBlockFunc
}

type headerBlockStore struct {
	mu     sync.RWMutex
	blocks []HeaderBlock
}

// HeaderBlockStore is the live store backing received HTTP/2 header blocks.
// Snapshot returns immutable copies while trailers may still be arriving.
type HeaderBlockStore = headerBlockStore

func (s *headerBlockStore) add(kind HeaderBlockKind, fields []hpack.HeaderField, truncated bool) {
	if s == nil {
		return
	}
	b := newHeaderBlock(kind, fields, truncated)
	s.mu.Lock()
	s.blocks = append(s.blocks, b)
	s.mu.Unlock()
}

func newHeaderBlock(kind HeaderBlockKind, fields []hpack.HeaderField, truncated bool) HeaderBlock {
	b := HeaderBlock{
		Kind:      kind,
		Fields:    make([]HeaderField, len(fields)),
		Truncated: truncated,
	}
	for i, f := range fields {
		b.Fields[i] = HeaderField{
			Name:      f.Name,
			Value:     f.Value,
			Sensitive: f.Sensitive,
		}
	}
	return b
}

func cloneHeaderBlock(b HeaderBlock) HeaderBlock {
	b.Fields = append([]HeaderField(nil), b.Fields...)
	return b
}

func (s *headerBlockStore) snapshot() []HeaderBlock {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.blocks) == 0 {
		return nil
	}
	blocks := make([]HeaderBlock, len(s.blocks))
	for i, b := range s.blocks {
		blocks[i] = cloneHeaderBlock(b)
	}
	return blocks
}

// Snapshot returns an immutable copy of all header blocks received so far.
func (s *headerBlockStore) Snapshot() []HeaderBlock { return s.snapshot() }

// RequestHeaderBlocks returns a snapshot of the HTTP/2 header blocks received
// for r. The initial block is available when the server handler starts. A
// trailer block is added after it arrives, normally while r.Body is read.
//
// It returns nil for requests not created by the legacy implementation in
// this package, including the initial HTTP/1 request of an h2c upgrade.
func RequestHeaderBlocks(ctx context.Context) []HeaderBlock {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(requestHeaderBlocksContextKey{}).(*headerBlockStore)
	return s.snapshot()
}

// WithRequestHeaderOrder returns a shallow copy of req whose HTTP/2 initial
// headers and trailers are written in order. Header values continue to come
// from req.Header and req.Trailer.
//
// The returned request is supported by the legacy implementation in this
// package (Go 1.26 and earlier, or builds using the http2legacy tag).
func WithRequestHeaderOrder(ctx context.Context, order HeaderOrder) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("http2: nil request context")
	}
	order, err := normalizeHeaderOrder(order, requestPseudoHeaders)
	if err != nil {
		return nil, err
	}
	blocks := requestHeaderBlocksToWrite(ctx)
	if len(order.Headers) > 0 && blocks.initial != nil {
		return nil, errors.New("http2: request header order conflicts with an exact initial header block")
	}
	if len(order.Trailers) > 0 && blocks.trailers != nil {
		return nil, errors.New("http2: request trailer order conflicts with an exact trailer block provider")
	}
	if fingerprint, ok := fingerprintToWriteFromContext(ctx); ok {
		if err := validateFingerprintHeaderConfig(fingerprint, order, blocks); err != nil {
			return nil, err
		}
	}
	if len(order.Headers) == 0 && len(order.Trailers) == 0 {
		return ctx, nil
	}
	return context.WithValue(ctx, requestHeaderOrderContextKey{}, order), nil
}

// WithRequestHeaderBlocks returns a shallow copy of req whose HTTP/2 initial
// header block is written exactly as initial.Fields. Repeated field names,
// field values, and Sensitive flags are retained. Request routing and the body
// still come from req; in particular, the transport connection is selected
// using req.URL rather than the :authority field in initial.
//
// If trailers is non-nil, the transport calls it after req.Body reaches EOF
// and writes the returned HeaderBlockTrailer exactly. This late callback lets
// a proxy stream a request body while preserving the actual trailer block.
// A returned trailer block with no fields sends no trailer block.
//
// initial must be a complete HeaderBlockInitial containing a valid HTTP/2
// request pseudo-header set. The caller is responsible for applying any proxy
// header filtering or pseudo-header rewriting before calling this function.
// This API is mutually exclusive with HeaderOrder for the corresponding
// initial or trailer block.
//
// It returns an error wrapping errors.ErrUnsupported when the legacy
// implementation is not enabled.
func WithRequestHeaderBlocks(ctx context.Context, initial HeaderBlock, trailers HeaderBlockFunc) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("http2: nil request context")
	}
	if err := validateRequestHeaderBlock(initial); err != nil {
		return nil, err
	}
	order := requestHeaderOrder(ctx)
	if len(order.Headers) > 0 {
		return nil, errors.New("http2: exact initial header block conflicts with request header order")
	}
	if trailers != nil && len(order.Trailers) > 0 {
		return nil, errors.New("http2: exact trailer block provider conflicts with request trailer order")
	}
	initial = cloneHeaderBlock(initial)
	config := requestHeaderBlocksWriteConfig{initial: &initial, trailers: trailers}
	if fingerprint, ok := fingerprintToWriteFromContext(ctx); ok {
		if err := validateFingerprintHeaderConfig(fingerprint, order, config); err != nil {
			return nil, err
		}
	}
	return context.WithValue(ctx, requestHeaderBlocksWriteContextKey{}, config), nil
}

// WithInformationalResponseHandler returns a shallow copy of req for which
// handler is called with every HTTP/2 1xx response block as soon as it is
// received. The block retains wire field order, repeated fields, values, and
// Sensitive flags. Returning an error aborts the round trip.
//
// The handler runs synchronously on the transport read loop and should return
// promptly. A proxy can forward the block to its downstream response writer
// with WriteResponseHeaderBlock.
//
// It returns an error wrapping errors.ErrUnsupported when the legacy
// implementation is not enabled.
func WithInformationalResponseHandler(ctx context.Context, handler InformationalResponseHandler) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("http2: nil request context")
	}
	if handler == nil {
		return nil, errors.New("http2: nil informational response handler")
	}
	return context.WithValue(ctx, informationalResponseHandlerContextKey{}, handler), nil
}

var requestPseudoHeaders = map[string]bool{
	":authority": true,
	":method":    true,
	":path":      true,
	":protocol":  true,
	":scheme":    true,
}

var responsePseudoHeaders = map[string]bool{
	":status": true,
}

func normalizeHeaderOrder(order HeaderOrder, allowedPseudos map[string]bool) (HeaderOrder, error) {
	headers, err := normalizeHeaderOrderNames(order.Headers, allowedPseudos, true)
	if err != nil {
		return HeaderOrder{}, err
	}
	trailers, err := normalizeHeaderOrderNames(order.Trailers, nil, false)
	if err != nil {
		return HeaderOrder{}, err
	}
	return HeaderOrder{Headers: headers, Trailers: trailers}, nil
}

func normalizeHeaderOrderNames(names []string, allowedPseudos map[string]bool, allowPseudos bool) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	regularSeen := false
	for _, name := range names {
		lower, ascii := httpcommon.LowerHeader(name)
		if !ascii || lower == "" {
			return nil, fmt.Errorf("http2: invalid header order field %q", name)
		}
		pseudo := lower[0] == ':'
		if pseudo {
			if !allowPseudos || !allowedPseudos[lower] {
				return nil, fmt.Errorf("http2: invalid pseudo-header %q in header order", name)
			}
			if regularSeen {
				return nil, fmt.Errorf("http2: pseudo-header %q appears after a regular field in header order", name)
			}
		} else {
			regularSeen = true
			if !httpguts.ValidHeaderFieldName(lower) {
				return nil, fmt.Errorf("http2: invalid header order field %q", name)
			}
		}
		if seen[lower] {
			return nil, fmt.Errorf("http2: duplicate field %q in header order", name)
		}
		seen[lower] = true
		normalized = append(normalized, lower)
	}
	return normalized, nil
}

type headerBlockRole uint8

const (
	headerBlockRequestInitial headerBlockRole = iota + 1
	headerBlockResponseInformational
	headerBlockResponseInitial
	headerBlockTrailerFields
)

func validateRequestHeaderBlock(block HeaderBlock) error {
	pseudos, err := validateHeaderBlock(block, headerBlockRequestInitial)
	if err != nil {
		return err
	}
	method := pseudos[":method"]
	if method == "" {
		return errors.New("http2: exact request header block is missing :method")
	}
	protocol := pseudos[":protocol"]
	if method == "CONNECT" && protocol == "" {
		if pseudos[":authority"] == "" || pseudos[":scheme"] != "" || pseudos[":path"] != "" {
			return errors.New("http2: malformed CONNECT pseudo-header fields in exact request header block")
		}
		return nil
	}
	if protocol != "" && method != "CONNECT" {
		return errors.New("http2: :protocol requires CONNECT in exact request header block")
	}
	if method == "CONNECT" && pseudos[":authority"] == "" {
		return errors.New("http2: extended CONNECT is missing :authority in exact request header block")
	}
	scheme := pseudos[":scheme"]
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("http2: invalid :scheme %q in exact request header block", scheme)
	}
	if pseudos[":path"] == "" {
		return errors.New("http2: exact request header block is missing :path")
	}
	return nil
}

func validateResponseHeaderBlock(block HeaderBlock) error {
	role := headerBlockResponseInitial
	if block.Kind == HeaderBlockInformational {
		role = headerBlockResponseInformational
	}
	pseudos, err := validateHeaderBlock(block, role)
	if err != nil {
		return err
	}
	status := pseudos[":status"]
	if len(status) != 3 {
		return fmt.Errorf("http2: invalid :status %q in exact response header block", status)
	}
	code, err := strconv.Atoi(status)
	if err != nil || code < 100 || code > 999 {
		return fmt.Errorf("http2: invalid :status %q in exact response header block", status)
	}
	if block.Kind == HeaderBlockInformational && code >= 200 {
		return fmt.Errorf("http2: status %d is not informational", code)
	}
	if block.Kind == HeaderBlockInitial && code < 200 {
		return fmt.Errorf("http2: status %d is not a final response", code)
	}
	return nil
}

func validateTrailerHeaderBlock(block HeaderBlock) error {
	_, err := validateHeaderBlock(block, headerBlockTrailerFields)
	return err
}

func validateHeaderBlock(block HeaderBlock, role headerBlockRole) (map[string]string, error) {
	wantKind := HeaderBlockInitial
	allowedPseudos := requestPseudoHeaders
	requiredPseudo := ":method"
	switch role {
	case headerBlockResponseInformational:
		wantKind = HeaderBlockInformational
		allowedPseudos = responsePseudoHeaders
		requiredPseudo = ":status"
	case headerBlockResponseInitial:
		allowedPseudos = responsePseudoHeaders
		requiredPseudo = ":status"
	case headerBlockTrailerFields:
		wantKind = HeaderBlockTrailer
		allowedPseudos = nil
		requiredPseudo = ""
	}
	if block.Kind != wantKind {
		return nil, fmt.Errorf("http2: exact header block kind %d; want %d", block.Kind, wantKind)
	}
	if block.Truncated {
		return nil, errors.New("http2: cannot write a truncated header block")
	}
	if len(block.Fields) == 0 && role != headerBlockTrailerFields {
		return nil, errors.New("http2: exact initial header block has no fields")
	}

	pseudos := make(map[string]string, len(allowedPseudos))
	sawRegular := false
	for _, field := range block.Fields {
		if !httpguts.ValidHeaderFieldValue(field.Value) {
			return nil, fmt.Errorf("http2: invalid value for exact header field %q", field.Name)
		}
		if len(field.Name) > 0 && field.Name[0] == ':' {
			if sawRegular {
				return nil, fmt.Errorf("http2: pseudo-header %q appears after a regular field", field.Name)
			}
			if !allowedPseudos[field.Name] {
				return nil, fmt.Errorf("http2: invalid pseudo-header %q in exact header block", field.Name)
			}
			if _, duplicate := pseudos[field.Name]; duplicate {
				return nil, fmt.Errorf("http2: duplicate pseudo-header %q in exact header block", field.Name)
			}
			pseudos[field.Name] = field.Value
			continue
		}
		sawRegular = true
		if !validWireHeaderFieldName(field.Name) {
			return nil, fmt.Errorf("http2: invalid wire header field name %q", field.Name)
		}
		if role == headerBlockTrailerFields && !httpguts.ValidTrailerHeader(field.Name) {
			return nil, fmt.Errorf("http2: field %q is not valid in trailers", field.Name)
		}
		switch field.Name {
		case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			return nil, fmt.Errorf("http2: connection-specific field %q is not valid in HTTP/2", field.Name)
		case "te":
			if field.Value != "" && field.Value != "trailers" {
				return nil, errors.New(`http2: header field "te" may only be "trailers"`)
			}
		}
	}
	if requiredPseudo != "" && pseudos[requiredPseudo] == "" {
		return nil, fmt.Errorf("http2: exact header block is missing %s", requiredPseudo)
	}
	return pseudos, nil
}

func requestHeaderOrder(ctx context.Context) HeaderOrder {
	order, _ := ctx.Value(requestHeaderOrderContextKey{}).(HeaderOrder)
	return order
}

func requestHeaderBlocksToWrite(ctx context.Context) requestHeaderBlocksWriteConfig {
	config, _ := ctx.Value(requestHeaderBlocksWriteContextKey{}).(requestHeaderBlocksWriteConfig)
	return config
}

func informationalResponseHandler(ctx context.Context) InformationalResponseHandler {
	handler, _ := ctx.Value(informationalResponseHandlerContextKey{}).(InformationalResponseHandler)
	return handler
}

func hpackHeaderFields(fields []HeaderField) []hpack.HeaderField {
	converted := make([]hpack.HeaderField, len(fields))
	for i, field := range fields {
		converted[i] = hpack.HeaderField{
			Name:      field.Name,
			Value:     field.Value,
			Sensitive: field.Sensitive,
		}
	}
	return converted
}

func contextWithRequestHeaderBlocks(ctx context.Context, s *headerBlockStore) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, requestHeaderBlocksContextKey{}, s)
}

// ClearRequestHeaderBlocks removes exact HTTP/2 block replay metadata from ctx.
func ClearRequestHeaderBlocks(ctx context.Context) context.Context {
	return context.WithValue(ctx, requestHeaderBlocksWriteContextKey{}, requestHeaderBlocksWriteConfig{})
}
