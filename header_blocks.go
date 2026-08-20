// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"runtime"
	"strings"
	"sync"
	"weak"

	"golang.org/x/net/http/httpguts"
)

// HeaderField is one decoded or exact header field. HTTP/1 receive blocks
// preserve the field-name casing used on the wire and contain unfolded values
// with surrounding optional whitespace removed. HTTP/2 names are lowercase,
// and Sensitive reports HPACK's never-indexed representation. Sensitive is
// always false for HTTP/1 and is rejected in exact HTTP/1 blocks.
type HeaderField struct {
	Name      string
	Value     string
	Sensitive bool
}

// HeaderBlockKind identifies the role of a header block.
type HeaderBlockKind uint8

const (
	HeaderBlockInitial HeaderBlockKind = iota + 1
	HeaderBlockInformational
	HeaderBlockTrailer
)

// HeaderBlock contains the fields from one HTTP header block in wire order.
// ProtoMajor is 1 for captured HTTP/1 blocks. Existing HTTP/2 captures retain
// zero for compatibility. StatusCode is set for captured HTTP/1 final and
// informational response blocks, and selects the status line when an exact
// HTTP/1 response block is written. Truncated reports that Fields is incomplete.
type HeaderBlock struct {
	Kind       HeaderBlockKind
	Fields     []HeaderField
	Truncated  bool
	ProtoMajor int
	StatusCode int
}

// HeaderBlockFunc returns a trailer block when it is ready.
type HeaderBlockFunc func() (HeaderBlock, error)

// InformationalResponseHandler handles one 1xx response block synchronously.
type InformationalResponseHandler func(HeaderBlock) error

type headerBlockStore struct {
	mu     sync.RWMutex
	blocks []HeaderBlock
}

func cloneHeaderBlock(block HeaderBlock) HeaderBlock {
	block.Fields = append([]HeaderField(nil), block.Fields...)
	return block
}

func (s *headerBlockStore) add(block HeaderBlock) {
	if s == nil {
		return
	}
	block = cloneHeaderBlock(block)
	s.mu.Lock()
	s.blocks = append(s.blocks, block)
	s.mu.Unlock()
}

func (s *headerBlockStore) appendFrom(other *headerBlockStore) {
	if s == nil || other == nil || s == other {
		return
	}
	for _, block := range other.snapshot() {
		s.add(block)
	}
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
	for i, block := range s.blocks {
		blocks[i] = cloneHeaderBlock(block)
	}
	return blocks
}

type requestHeaderBlocksWriteContextKey struct{}
type informationalResponseHandlerContextKey struct{}

type requestHeaderBlocksWriteConfig struct {
	proto    int
	initial  *HeaderBlock
	trailers HeaderBlockFunc
}

func requestHeaderBlocksToWrite(ctx context.Context) requestHeaderBlocksWriteConfig {
	config, _ := ctx.Value(requestHeaderBlocksWriteContextKey{}).(requestHeaderBlocksWriteConfig)
	return config
}

func clearRequestHeaderBlocksForRedirect(req *Request) *Request {
	if req == nil {
		return nil
	}
	req = http2ClearRequestHeaderBlocksBridge(req)
	return req.WithContext(context.WithValue(req.Context(), requestHeaderBlocksWriteContextKey{}, requestHeaderBlocksWriteConfig{}))
}

func informationalResponseHandler(ctx context.Context) InformationalResponseHandler {
	handler, _ := ctx.Value(informationalResponseHandlerContextKey{}).(InformationalResponseHandler)
	return handler
}

var requestHeaderBlockStores = struct {
	sync.RWMutex
	m map[weak.Pointer[Request]]*headerBlockStore
}{m: make(map[weak.Pointer[Request]]*headerBlockStore)}

var responseHeaderBlockStores = struct {
	sync.RWMutex
	m map[weak.Pointer[Response]]*headerBlockStore
}{m: make(map[weak.Pointer[Response]]*headerBlockStore)}

func registerRequestHeaderBlocks(req *Request, store *headerBlockStore) {
	if req == nil || store == nil {
		return
	}
	key := weak.Make(req)
	requestHeaderBlockStores.Lock()
	requestHeaderBlockStores.m[key] = store
	requestHeaderBlockStores.Unlock()
	runtime.AddCleanup(req, func(key weak.Pointer[Request]) {
		requestHeaderBlockStores.Lock()
		delete(requestHeaderBlockStores.m, key)
		requestHeaderBlockStores.Unlock()
	}, key)
}

func requestHeaderBlockStore(req *Request) *headerBlockStore {
	if req == nil {
		return nil
	}
	key := weak.Make(req)
	requestHeaderBlockStores.RLock()
	store := requestHeaderBlockStores.m[key]
	requestHeaderBlockStores.RUnlock()
	runtime.KeepAlive(req)
	return store
}

func registerResponseHeaderBlocks(resp *Response, store *headerBlockStore) {
	if resp == nil || store == nil {
		return
	}
	key := weak.Make(resp)
	responseHeaderBlockStores.Lock()
	responseHeaderBlockStores.m[key] = store
	responseHeaderBlockStores.Unlock()
	runtime.AddCleanup(resp, func(key weak.Pointer[Response]) {
		responseHeaderBlockStores.Lock()
		delete(responseHeaderBlockStores.m, key)
		responseHeaderBlockStores.Unlock()
	}, key)
}

func responseHeaderBlockStore(resp *Response) *headerBlockStore {
	if resp == nil {
		return nil
	}
	key := weak.Make(resp)
	responseHeaderBlockStores.RLock()
	store := responseHeaderBlockStores.m[key]
	responseHeaderBlockStores.RUnlock()
	runtime.KeepAlive(resp)
	return store
}

// readMIMEHeaderBlock follows textproto.Reader.ReadMIMEHeader's validation
// and map semantics while retaining each logical field line in receive order.
func readMIMEHeaderBlock(tp *textproto.Reader, kind HeaderBlockKind, statusCode int, maxHeaders int64) (textproto.MIMEHeader, HeaderBlock, error) {
	header := make(textproto.MIMEHeader)
	block := HeaderBlock{Kind: kind, ProtoMajor: 1, StatusCode: statusCode}

	if buf, err := tp.R.Peek(1); err == nil && (buf[0] == ' ' || buf[0] == '\t') {
		line, readErr := tp.ReadLineBytes()
		if readErr != nil {
			return header, block, readErr
		}
		if len(line) > 80 {
			line = line[:80]
		}
		return header, block, textproto.ProtocolError(fmt.Sprintf("malformed MIME header initial line: %q", line))
	}

	for {
		line, err := readContinuedMIMEHeaderLine(tp)
		if len(line) == 0 {
			return header, block, err
		}
		nameBytes, valueBytes, ok := bytes.Cut(line, []byte{':'})
		if !ok || !validMIMEHeaderFieldName(nameBytes) {
			return header, block, textproto.ProtocolError(fmt.Sprintf("malformed MIME header line: %q", line))
		}
		for _, b := range valueBytes {
			if b == '\r' || b == '\n' || b == 0x7f || b < ' ' && b != '\t' {
				return header, block, textproto.ProtocolError(fmt.Sprintf("malformed MIME header line: %q", line))
			}
		}
		name := string(nameBytes)
		value := string(bytes.TrimLeft(valueBytes, " \t"))
		canonicalName := textproto.CanonicalMIMEHeaderKey(name)
		header[canonicalName] = append(header[canonicalName], value)
		block.Fields = append(block.Fields, HeaderField{Name: name, Value: value})
		maxHeaders--
		if maxHeaders < 0 {
			return header, block, errors.New("message too large")
		}
		if err != nil {
			if err == io.EOF {
				return header, block, err
			}
			return header, block, err
		}
	}
}

func readContinuedMIMEHeaderLine(tp *textproto.Reader) ([]byte, error) {
	line, err := tp.ReadLineBytes()
	if err != nil || len(line) == 0 {
		return line, err
	}
	if bytes.IndexByte(line, ':') < 0 {
		return nil, textproto.ProtocolError(fmt.Sprintf("malformed MIME header: missing colon: %q", line))
	}
	logical := append([]byte(nil), bytes.Trim(line, " \t")...)
	for {
		peek, peekErr := tp.R.Peek(1)
		if peekErr != nil || len(peek) == 0 || peek[0] != ' ' && peek[0] != '\t' {
			break
		}
		continuation, readErr := tp.ReadLineBytes()
		logical = append(logical, ' ')
		logical = append(logical, bytes.Trim(continuation, " \t")...)
		if readErr != nil {
			break
		}
	}
	return logical, nil
}

func validMIMEHeaderFieldName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	if bytes.IndexByte(name, ' ') < 0 {
		return httpguts.ValidHeaderFieldName(string(name))
	}
	normalized := bytes.Clone(name)
	for i, b := range normalized {
		if b == ' ' {
			normalized[i] = 'a'
		}
	}
	return httpguts.ValidHeaderFieldName(string(normalized))
}

// RequestHeaderBlocks returns an immutable snapshot of header blocks received
// for r. HTTP/1 request trailers appear after they are read to EOF.
func RequestHeaderBlocks(r *Request) []HeaderBlock {
	if r == nil {
		return nil
	}
	if blocks := requestHeaderBlockStore(r).snapshot(); blocks != nil {
		return blocks
	}
	return http2RequestHeaderBlocksBridge(r)
}

// ResponseHeaderBlocks returns an immutable snapshot of header blocks received
// for resp. HTTP/1 trailers appear after the response body is read to EOF.
func ResponseHeaderBlocks(resp *Response) []HeaderBlock {
	if blocks := responseHeaderBlockStore(resp).snapshot(); blocks != nil {
		return blocks
	}
	return http2ResponseHeaderBlocksBridge(resp)
}

func headerBlockProtocol(block HeaderBlock) int {
	if block.ProtoMajor != 0 {
		return block.ProtoMajor
	}
	for _, field := range block.Fields {
		if strings.HasPrefix(field.Name, ":") {
			return 2
		}
	}
	return 1
}

func validateHTTP1HeaderBlock(block HeaderBlock, kind HeaderBlockKind) error {
	if block.ProtoMajor != 0 && block.ProtoMajor != 1 {
		return fmt.Errorf("http: exact HTTP/1 header block has ProtoMajor %d", block.ProtoMajor)
	}
	if block.Kind != kind {
		return fmt.Errorf("http: exact header block has kind %d; want %d", block.Kind, kind)
	}
	if block.Truncated {
		return errors.New("http: exact HTTP/1 header block is truncated")
	}
	for _, field := range block.Fields {
		if field.Sensitive {
			return fmt.Errorf("http: exact HTTP/1 field %q has Sensitive set", field.Name)
		}
		if strings.HasPrefix(field.Name, ":") || !httpguts.ValidHeaderFieldName(field.Name) {
			return fmt.Errorf("http: invalid exact HTTP/1 field name %q", field.Name)
		}
		if !httpguts.ValidHeaderFieldValue(field.Value) {
			return fmt.Errorf("http: invalid exact HTTP/1 value for field %q", field.Name)
		}
	}
	return nil
}

// WithRequestHeaderBlocks returns a shallow copy of req whose initial header
// block and optional trailer block are written exactly as supplied. For
// HTTP/1, the request line and routing still come from req, while all header
// lines come only from initial. A trailer provider is called after the request
// body reaches EOF. Automatic redirects clear exact blocks before invoking
// Client.CheckRedirect; same-URL transport retries retain them.
func WithRequestHeaderBlocks(req *Request, initial HeaderBlock, trailers HeaderBlockFunc) (*Request, error) {
	if req == nil {
		return nil, errors.New("http: nil request")
	}
	proto := headerBlockProtocol(initial)
	if proto != 1 && proto != 2 {
		return nil, fmt.Errorf("http: unsupported exact header block protocol %d", proto)
	}
	order := requestHeaderOrder(req.Context())
	if len(order.Headers) > 0 {
		return nil, errors.New("http: exact initial header block conflicts with request header order")
	}
	if trailers != nil && len(order.Trailers) > 0 {
		return nil, errors.New("http: exact trailer block provider conflicts with request trailer order")
	}
	var err error
	if proto == 1 {
		err = validateHTTP1HeaderBlock(initial, HeaderBlockInitial)
		if err == nil && initial.StatusCode != 0 {
			err = errors.New("http: exact HTTP/1 request block must not contain StatusCode")
		}
		initial = cloneHeaderBlock(initial)
		config := requestHeaderBlocksWriteConfig{proto: proto, initial: &initial, trailers: trailers}
		if err == nil {
			_, err = prepareHTTP1ExactRequest(req, config)
		}
		if err == nil {
			req = http2ClearRequestHeaderBlocksBridge(req)
		}
	} else {
		if initial.StatusCode != 0 {
			return nil, errors.New("http: exact HTTP/2 request block must not contain StatusCode")
		}
		req, err = http2WithRequestHeaderBlocksBridge(req, initial, trailers)
	}
	if err != nil {
		return nil, err
	}
	if proto != 1 {
		initial = cloneHeaderBlock(initial)
	}
	config := requestHeaderBlocksWriteConfig{proto: proto, initial: &initial, trailers: trailers}
	return req.WithContext(context.WithValue(req.Context(), requestHeaderBlocksWriteContextKey{}, config)), nil
}

// WithInformationalResponseHandler returns a shallow copy of req for which
// handler is called synchronously for every HTTP/1 or HTTP/2 1xx response as
// it arrives. On HTTP/1 it runs before httptrace.ClientTrace.Got1xxResponse.
// Returning an error aborts the round trip.
func WithInformationalResponseHandler(req *Request, handler InformationalResponseHandler) (*Request, error) {
	if req == nil {
		return nil, errors.New("http: nil request")
	}
	if handler == nil {
		return nil, errors.New("http: nil informational response handler")
	}
	var err error
	req, err = http2WithInformationalResponseHandlerBridge(req, handler)
	if err != nil {
		return nil, err
	}
	return req.WithContext(context.WithValue(req.Context(), informationalResponseHandlerContextKey{}, handler)), nil
}

type http1responseHeaderBlockWriter interface {
	writeHTTP1HeaderBlock(HeaderBlock) error
	setHTTP1TrailerBlock(HeaderBlock) error
}

func findHTTP1ResponseHeaderBlockWriter(w ResponseWriter) (http1responseHeaderBlockWriter, bool, error) {
	for range 100 {
		if writer, ok := w.(http1responseHeaderBlockWriter); ok {
			return writer, true, nil
		}
		uw, ok := w.(responseWriterUnwrapper)
		if !ok {
			return nil, false, nil
		}
		w = uw.Unwrap()
		if w == nil {
			return nil, false, errors.ErrUnsupported
		}
	}
	return nil, false, fmt.Errorf("too many response writer wrappers: %w", errors.ErrUnsupported)
}

// WriteResponseHeaderBlock writes an informational block or commits a final
// HTTP/1 or HTTP/2 response block using Fields exactly. HTTP/1 status lines
// use block.StatusCode; Date, Content-Type, and framing fields are not added.
func WriteResponseHeaderBlock(w ResponseWriter, block HeaderBlock) error {
	if w == nil {
		return errors.New("http: nil response writer")
	}
	proto := headerBlockProtocol(block)
	if proto == 1 {
		if err := validateHTTP1HeaderBlock(block, block.Kind); err != nil {
			return err
		}
		if block.Kind != HeaderBlockInitial && block.Kind != HeaderBlockInformational {
			return errors.New("http: response header block must be initial or informational")
		}
		writer, ok, err := findHTTP1ResponseHeaderBlockWriter(w)
		if err != nil {
			return fmt.Errorf("http: write response header block: %w", err)
		}
		if !ok {
			return fmt.Errorf("http: write response header block: %w", errors.ErrUnsupported)
		}
		return writer.writeHTTP1HeaderBlock(cloneHeaderBlock(block))
	}
	if proto != 2 {
		return fmt.Errorf("http: unsupported exact header block protocol %d", proto)
	}
	if block.StatusCode != 0 {
		return errors.New("http: exact HTTP/2 response block must not contain StatusCode")
	}
	return http2WriteResponseHeaderBlockBridge(w, block)
}

// SetResponseTrailerBlock configures the exact trailer block sent when the
// handler finishes. It may be called after the response body has started. A
// non-empty HTTP/1 block requires chunked framing and a matching Trailer
// declaration in the initial response fields.
func SetResponseTrailerBlock(w ResponseWriter, block HeaderBlock) error {
	if w == nil {
		return errors.New("http: nil response writer")
	}
	proto := headerBlockProtocol(block)
	if proto == 1 {
		if err := validateHTTP1HeaderBlock(block, HeaderBlockTrailer); err != nil {
			return err
		}
		if block.StatusCode != 0 {
			return errors.New("http: exact HTTP/1 trailer block must not contain StatusCode")
		}
		writer, ok, err := findHTTP1ResponseHeaderBlockWriter(w)
		if err != nil {
			return fmt.Errorf("http: set response trailer block: %w", err)
		}
		if ok {
			return writer.setHTTP1TrailerBlock(cloneHeaderBlock(block))
		}
		// ProtoMajor zero is ambiguous for an empty HTTP/2 trailer block.
		if block.ProtoMajor == 1 {
			return fmt.Errorf("http: set response trailer block: %w", errors.ErrUnsupported)
		}
	}
	if proto != 1 && proto != 2 {
		return fmt.Errorf("http: unsupported exact header block protocol %d", proto)
	}
	if block.StatusCode != 0 {
		return errors.New("http: exact HTTP/2 trailer block must not contain StatusCode")
	}
	return http2SetResponseTrailerBlockBridge(w, block)
}
