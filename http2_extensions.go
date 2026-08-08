// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http

// SettingID is an HTTP/2 setting identifier.
type SettingID uint16

const (
	SettingHeaderTableSize       SettingID = SettingID(http2SettingHeaderTableSize)
	SettingEnablePush            SettingID = SettingID(http2SettingEnablePush)
	SettingMaxConcurrentStreams  SettingID = SettingID(http2SettingMaxConcurrentStreams)
	SettingInitialWindowSize     SettingID = SettingID(http2SettingInitialWindowSize)
	SettingMaxFrameSize          SettingID = SettingID(http2SettingMaxFrameSize)
	SettingMaxHeaderListSize     SettingID = SettingID(http2SettingMaxHeaderListSize)
	SettingEnableConnectProtocol SettingID = SettingID(http2SettingEnableConnectProtocol)
	SettingNoRFC7540Priorities   SettingID = SettingID(http2SettingNoRFC7540Priorities)
)

// String returns the setting name, or a numeric representation for an unknown
// setting.
func (s SettingID) String() string {
	return http2SettingID(s).String()
}

// Setting is an HTTP/2 setting parameter.
type Setting struct {
	ID  SettingID
	Val uint32
}

// String returns a human-readable representation of s.
func (s Setting) String() string {
	return s.http2().String()
}

// Valid reports whether s has a valid value.
func (s Setting) Valid() error {
	return s.http2().Valid()
}

func (s Setting) http2() http2Setting {
	return http2Setting{ID: http2SettingID(s.ID), Val: s.Val}
}

// HeaderField is a decoded HTTP/2 header field. Name is the lowercase field
// name used on the wire. Sensitive reports whether HPACK's never-indexed
// representation was used.
type HeaderField struct {
	Name      string
	Value     string
	Sensitive bool
}

// HeaderBlockKind identifies the role of an HTTP/2 HEADERS block.
type HeaderBlockKind uint8

const (
	HeaderBlockInitial       HeaderBlockKind = HeaderBlockKind(http2HeaderBlockInitial)
	HeaderBlockInformational HeaderBlockKind = HeaderBlockKind(http2HeaderBlockInformational)
	HeaderBlockTrailer       HeaderBlockKind = HeaderBlockKind(http2HeaderBlockTrailer)
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

// HeaderBlockFunc returns a header block when it is ready. It is used by
// WithRequestHeaderBlocks for request trailers.
type HeaderBlockFunc func() (HeaderBlock, error)

// InformationalResponseHandler handles one HTTP/2 1xx response header block.
type InformationalResponseHandler func(HeaderBlock) error

// HeaderOrder specifies the order in which HTTP/2 fields are written.
type HeaderOrder struct {
	Headers  []string
	Trailers []string
}

// RequestHeaderBlocks returns a snapshot of the HTTP/2 header blocks received
// for r. It returns nil for requests not created by the bundled HTTP/2
// implementation.
func RequestHeaderBlocks(r *Request) []HeaderBlock {
	return publicHeaderBlocks(http2RequestHeaderBlocks(r))
}

// ResponseHeaderBlocks returns a snapshot of the HTTP/2 header blocks received
// for resp. It returns nil for responses not created by the bundled HTTP/2
// implementation.
func ResponseHeaderBlocks(resp *Response) []HeaderBlock {
	return publicHeaderBlocks(http2ResponseHeaderBlocks(resp))
}

// WithRequestHeaderOrder returns a shallow copy of req whose HTTP/2 initial
// headers and trailers are written in order.
func WithRequestHeaderOrder(req *Request, order HeaderOrder) (*Request, error) {
	return http2WithRequestHeaderOrder(req, order.http2())
}

// WithRequestHeaderBlocks returns a shallow copy of req whose HTTP/2 initial
// header block and optional trailer block are written exactly as supplied.
func WithRequestHeaderBlocks(req *Request, initial HeaderBlock, trailers HeaderBlockFunc) (*Request, error) {
	var http2Trailers http2HeaderBlockFunc
	if trailers != nil {
		http2Trailers = func() (http2HeaderBlock, error) {
			block, err := trailers()
			return block.http2(), err
		}
	}
	return http2WithRequestHeaderBlocks(req, initial.http2(), http2Trailers)
}

// WithInformationalResponseHandler returns a shallow copy of req for which
// handler is called with every HTTP/2 1xx response block as it is received.
func WithInformationalResponseHandler(req *Request, handler InformationalResponseHandler) (*Request, error) {
	if handler == nil {
		return http2WithInformationalResponseHandler(req, nil)
	}
	return http2WithInformationalResponseHandler(req, func(block http2HeaderBlock) error {
		return handler(publicHeaderBlock(block))
	})
}

// SetResponseHeaderOrder configures the order of response headers and trailers
// written by w.
func SetResponseHeaderOrder(w ResponseWriter, order HeaderOrder) error {
	return http2SetResponseHeaderOrder(w, order.http2())
}

// WriteResponseHeaderBlock writes an informational response block or commits
// a final response block using block.Fields exactly.
func WriteResponseHeaderBlock(w ResponseWriter, block HeaderBlock) error {
	return http2WriteResponseHeaderBlock(w, block.http2())
}

// SetResponseTrailerBlock configures the exact HTTP/2 trailer block sent when
// the handler finishes.
func SetResponseTrailerBlock(w ResponseWriter, block HeaderBlock) error {
	return http2SetResponseTrailerBlock(w, block.http2())
}

func (order HeaderOrder) http2() http2HeaderOrder {
	return http2HeaderOrder{
		Headers:  append([]string(nil), order.Headers...),
		Trailers: append([]string(nil), order.Trailers...),
	}
}

func (block HeaderBlock) http2() http2HeaderBlock {
	converted := http2HeaderBlock{
		Kind:      http2HeaderBlockKind(block.Kind),
		Truncated: block.Truncated,
	}
	if block.Fields != nil {
		converted.Fields = make([]http2HeaderField, len(block.Fields))
	}
	for i, field := range block.Fields {
		converted.Fields[i] = http2HeaderField(field)
	}
	return converted
}

func publicHeaderBlock(block http2HeaderBlock) HeaderBlock {
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

func publicHeaderBlocks(blocks []http2HeaderBlock) []HeaderBlock {
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
	PseudoHeaderOrder []string
}

// FingerprintPriority is one priority entry in an HTTP/2 fingerprint. Weight
// is the RFC 7540 weight in the range 1..256.
type FingerprintPriority struct {
	StreamID  uint32
	StreamDep uint32
	Exclusive bool
	Weight    uint16
}

// Validate reports whether f is a complete, safely replayable HTTP/2
// fingerprint.
func (f Fingerprint) Validate() error {
	return f.http2().Validate()
}

// String returns the canonical HTTP/2 fingerprint string.
func (f Fingerprint) String() string {
	return f.http2().String()
}

// Hash returns the lowercase hexadecimal MD5 of String.
func (f Fingerprint) Hash() string {
	return f.http2().Hash()
}

// ParseFingerprint parses and validates a four-part HTTP/2 fingerprint string.
func ParseFingerprint(value string) (Fingerprint, error) {
	fingerprint, err := http2ParseFingerprint(value)
	if err != nil {
		return Fingerprint{}, err
	}
	return publicFingerprint(fingerprint), nil
}

// RequestFingerprint returns the HTTP/2 fingerprint associated with r.
func RequestFingerprint(r *Request) (Fingerprint, bool) {
	fingerprint, ok := http2RequestFingerprint(r)
	if !ok {
		return Fingerprint{}, false
	}
	return publicFingerprint(fingerprint), true
}

// WithRequestFingerprint returns a shallow copy of req carrying f for HTTP/2
// connection selection, initial frame replay, and pseudo-header order.
func WithRequestFingerprint(req *Request, f Fingerprint) (*Request, error) {
	return http2WithRequestFingerprint(req, f.http2())
}

func (f Fingerprint) http2() http2Fingerprint {
	converted := http2Fingerprint{
		WindowUpdate:      f.WindowUpdate,
		PseudoHeaderOrder: append([]string(nil), f.PseudoHeaderOrder...),
	}
	if f.Settings != nil {
		converted.Settings = make([]http2Setting, len(f.Settings))
	}
	if f.Priorities != nil {
		converted.Priorities = make([]http2FingerprintPriority, len(f.Priorities))
	}
	for i, setting := range f.Settings {
		converted.Settings[i] = setting.http2()
	}
	for i, priority := range f.Priorities {
		converted.Priorities[i] = http2FingerprintPriority(priority)
	}
	return converted
}

func publicFingerprint(f http2Fingerprint) Fingerprint {
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
	return converted
}
