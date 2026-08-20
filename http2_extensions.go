// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"weak"

	"github.com/josexy/xhttp/internal/http2"
)

// SettingID is an HTTP/2 setting identifier.
type SettingID uint16

const (
	SettingHeaderTableSize       SettingID = SettingID(http2.SettingHeaderTableSize)
	SettingEnablePush            SettingID = SettingID(http2.SettingEnablePush)
	SettingMaxConcurrentStreams  SettingID = SettingID(http2.SettingMaxConcurrentStreams)
	SettingInitialWindowSize     SettingID = SettingID(http2.SettingInitialWindowSize)
	SettingMaxFrameSize          SettingID = SettingID(http2.SettingMaxFrameSize)
	SettingMaxHeaderListSize     SettingID = SettingID(http2.SettingMaxHeaderListSize)
	SettingEnableConnectProtocol SettingID = SettingID(http2.SettingEnableConnectProtocol)
	SettingNoRFC7540Priorities   SettingID = SettingID(http2.SettingNoRFC7540Priorities)
)

// String returns the setting name, or a numeric representation for an unknown
// setting.
func (s SettingID) String() string { return http2.SettingID(s).String() }

// Setting is an HTTP/2 setting parameter.
type Setting struct {
	ID  SettingID
	Val uint32
}

// String returns a human-readable representation of s.
func (s Setting) String() string { return s.http2().String() }

// Valid reports whether s has a valid value.
func (s Setting) Valid() error { return s.http2().Valid() }

func (s Setting) http2() http2.Setting {
	return http2.Setting{ID: http2.SettingID(s.ID), Val: s.Val}
}

func (order HeaderOrder) http2() http2.HeaderOrder {
	return http2.HeaderOrder{
		Headers:  append([]string(nil), order.Headers...),
		Trailers: append([]string(nil), order.Trailers...),
	}
}

func http2WithRequestHeaderOrderBridge(req *Request, order HeaderOrder) (*Request, error) {
	ctx, err := http2.WithRequestHeaderOrder(req.Context(), order.http2())
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

type http2ResponseExtensionWriter interface {
	setHTTP2HeaderOrder(http2.HeaderOrder) error
	writeHTTP2HeaderBlock(http2.HeaderBlock) error
	setHTTP2TrailerBlock(http2.HeaderBlock) error
}

func findHTTP2ResponseExtensionWriter(w ResponseWriter) (http2ResponseExtensionWriter, error) {
	for range 100 {
		if writer, ok := w.(http2ResponseExtensionWriter); ok {
			return writer, nil
		}
		uw, ok := w.(responseWriterUnwrapper)
		if !ok {
			return nil, errors.ErrUnsupported
		}
		w = uw.Unwrap()
		if w == nil {
			return nil, errors.ErrUnsupported
		}
	}
	return nil, fmt.Errorf("too many response writer wrappers: %w", errors.ErrUnsupported)
}

func http2SetResponseHeaderOrderBridge(w ResponseWriter, order HeaderOrder) error {
	writer, err := findHTTP2ResponseExtensionWriter(w)
	if err != nil {
		return fmt.Errorf("http2: response header order: %w", err)
	}
	return writer.setHTTP2HeaderOrder(order.http2())
}

func http2RequestHeaderBlocksBridge(r *Request) []HeaderBlock {
	if r == nil {
		return nil
	}
	return publicHeaderBlocks(http2.RequestHeaderBlocks(r.Context()))
}

var http2ResponseHeaderBlockStores = struct {
	sync.RWMutex
	m map[weak.Pointer[Response]]*http2.HeaderBlockStore
}{m: make(map[weak.Pointer[Response]]*http2.HeaderBlockStore)}

func registerHTTP2ResponseHeaderBlocks(resp *Response, store *http2.HeaderBlockStore) {
	if resp == nil || store == nil {
		return
	}
	key := weak.Make(resp)
	http2ResponseHeaderBlockStores.Lock()
	http2ResponseHeaderBlockStores.m[key] = store
	http2ResponseHeaderBlockStores.Unlock()
	runtime.AddCleanup(resp, func(key weak.Pointer[Response]) {
		http2ResponseHeaderBlockStores.Lock()
		delete(http2ResponseHeaderBlockStores.m, key)
		http2ResponseHeaderBlockStores.Unlock()
	}, key)
}

func http2ResponseHeaderBlocksBridge(resp *Response) []HeaderBlock {
	if resp == nil {
		return nil
	}
	key := weak.Make(resp)
	http2ResponseHeaderBlockStores.RLock()
	store := http2ResponseHeaderBlockStores.m[key]
	http2ResponseHeaderBlockStores.RUnlock()
	runtime.KeepAlive(resp)
	if store == nil {
		return nil
	}
	return publicHeaderBlocks(store.Snapshot())
}

func http2WithRequestHeaderBlocksBridge(req *Request, initial HeaderBlock, trailers HeaderBlockFunc) (*Request, error) {
	var http2Trailers http2.HeaderBlockFunc
	if trailers != nil {
		http2Trailers = func() (http2.HeaderBlock, error) {
			block, err := trailers()
			return block.http2(), err
		}
	}
	ctx, err := http2.WithRequestHeaderBlocks(req.Context(), initial.http2(), http2Trailers)
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

func http2WithInformationalResponseHandlerBridge(req *Request, handler InformationalResponseHandler) (*Request, error) {
	ctx, err := http2.WithInformationalResponseHandler(req.Context(), func(block http2.HeaderBlock) error {
		return handler(publicHeaderBlock(block))
	})
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

func http2WriteResponseHeaderBlockBridge(w ResponseWriter, block HeaderBlock) error {
	writer, err := findHTTP2ResponseExtensionWriter(w)
	if err != nil {
		return fmt.Errorf("http2: write response header block: %w", err)
	}
	return writer.writeHTTP2HeaderBlock(block.http2())
}

func http2SetResponseTrailerBlockBridge(w ResponseWriter, block HeaderBlock) error {
	writer, err := findHTTP2ResponseExtensionWriter(w)
	if err != nil {
		return fmt.Errorf("http2: set response trailer block: %w", err)
	}
	return writer.setHTTP2TrailerBlock(block.http2())
}

func http2ClearRequestHeaderBlocksBridge(req *Request) *Request {
	return req.WithContext(http2.ClearRequestHeaderBlocks(req.Context()))
}

func (block HeaderBlock) http2() http2.HeaderBlock {
	converted := http2.HeaderBlock{
		Kind:      http2.HeaderBlockKind(block.Kind),
		Truncated: block.Truncated,
	}
	if block.Fields != nil {
		converted.Fields = make([]http2.HeaderField, len(block.Fields))
	}
	for i, field := range block.Fields {
		converted.Fields[i] = http2.HeaderField(field)
	}
	return converted
}

func publicHeaderBlock(block http2.HeaderBlock) HeaderBlock {
	converted := HeaderBlock{
		Kind:      HeaderBlockKind(block.Kind),
		Truncated: block.Truncated,
	}
	if block.Fields != nil {
		converted.Fields = make([]HeaderField, len(block.Fields))
	}
	for i, field := range block.Fields {
		converted.Fields[i] = HeaderField(field)
	}
	return converted
}

func publicHeaderBlocks(blocks []http2.HeaderBlock) []HeaderBlock {
	if blocks == nil {
		return nil
	}
	converted := make([]HeaderBlock, len(blocks))
	for i, block := range blocks {
		converted[i] = publicHeaderBlock(block)
	}
	return converted
}

// Fingerprint is a four-part HTTP/2 frame fingerprint consisting of SETTINGS,
// connection WINDOW_UPDATE, PRIORITY, and request pseudo-header order.
type Fingerprint struct {
	Settings          []Setting
	WindowUpdate      uint32
	Priorities        []FingerprintPriority
	HeaderPriority    *FingerprintHeaderPriority
	PseudoHeaderOrder []string
}

// FingerprintPriority is one priority entry in an HTTP/2 fingerprint.
// Entries in Fingerprint.Priorities were sent as standalone PRIORITY frames.
// Weight is the RFC 7540 weight in the range 1..256.
type FingerprintPriority struct {
	StreamID  uint32
	StreamDep uint32
	Exclusive bool
	Weight    uint16
}

// FingerprintHeaderPriority is the RFC 7540 priority information carried by
// this request's initial HEADERS frame. Its stream ID is allocated by the
// outgoing connection and is not part of this value. Header priority is
// excluded from the canonical fingerprint String and Hash. Weight is in the
// semantic RFC 7540 range 1..256.
type FingerprintHeaderPriority struct {
	StreamDep uint32
	Exclusive bool
	Weight    uint16
}

// Validate reports whether f is a complete, safely replayable HTTP/2
// fingerprint.
func (f Fingerprint) Validate() error { return f.http2().Validate() }

// String returns the canonical four-part HTTP/2 fingerprint string.
// It deliberately excludes HeaderPriority.
func (f Fingerprint) String() string { return f.http2().String() }

// Hash returns the lowercase hexadecimal MD5 of String and deliberately
// excludes HeaderPriority.
func (f Fingerprint) Hash() string { return f.http2().Hash() }

// ParseFingerprint parses and validates a canonical four-part HTTP/2
// fingerprint string. The returned Fingerprint has a nil HeaderPriority,
// because that request-scoped metadata is not encoded by String.
func ParseFingerprint(value string) (Fingerprint, error) {
	fingerprint, err := http2.ParseFingerprint(value)
	if err != nil {
		return Fingerprint{}, err
	}
	return publicFingerprint(fingerprint), nil
}

// RequestFingerprint returns the HTTP/2 fingerprint associated with r.
func RequestFingerprint(r *Request) (Fingerprint, bool) {
	if r == nil {
		return Fingerprint{}, false
	}
	fingerprint, ok := http2.RequestFingerprint(r.Context())
	if !ok {
		return Fingerprint{}, false
	}
	return publicFingerprint(fingerprint), true
}

// WithRequestFingerprint returns a shallow copy of req carrying f for HTTP/2
// connection selection, initial frame replay, and pseudo-header order.
func WithRequestFingerprint(req *Request, f Fingerprint) (*Request, error) {
	if req == nil {
		return nil, errors.New("http2: nil request")
	}
	ctx, err := http2.WithRequestFingerprint(req.Context(), req.Method, http2.Header(req.Header), f.http2())
	if err != nil {
		return nil, err
	}
	return req.WithContext(ctx), nil
}

func (f Fingerprint) http2() http2.Fingerprint {
	converted := http2.Fingerprint{
		WindowUpdate:      f.WindowUpdate,
		PseudoHeaderOrder: append([]string(nil), f.PseudoHeaderOrder...),
	}
	if f.Settings != nil {
		converted.Settings = make([]http2.Setting, len(f.Settings))
	}
	if f.Priorities != nil {
		converted.Priorities = make([]http2.FingerprintPriority, len(f.Priorities))
	}
	for i, setting := range f.Settings {
		converted.Settings[i] = setting.http2()
	}
	for i, priority := range f.Priorities {
		converted.Priorities[i] = http2.FingerprintPriority(priority)
	}
	if f.HeaderPriority != nil {
		priority := http2.FingerprintHeaderPriority(*f.HeaderPriority)
		converted.HeaderPriority = &priority
	}
	return converted
}

func publicFingerprint(f http2.Fingerprint) Fingerprint {
	converted := Fingerprint{
		WindowUpdate:      f.WindowUpdate,
		PseudoHeaderOrder: append([]string(nil), f.PseudoHeaderOrder...),
	}
	if f.Settings != nil {
		converted.Settings = make([]Setting, len(f.Settings))
	}
	if f.Priorities != nil {
		converted.Priorities = make([]FingerprintPriority, len(f.Priorities))
	}
	for i, setting := range f.Settings {
		converted.Settings[i] = Setting{ID: SettingID(setting.ID), Val: setting.Val}
	}
	for i, priority := range f.Priorities {
		converted.Priorities[i] = FingerprintPriority(priority)
	}
	if f.HeaderPriority != nil {
		priority := FingerprintHeaderPriority(*f.HeaderPriority)
		converted.HeaderPriority = &priority
	}
	return converted
}
