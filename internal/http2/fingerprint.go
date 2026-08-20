// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/net/http2/hpack"
)

// Fingerprint is a four-part HTTP/2 frame fingerprint consisting of SETTINGS,
// connection WINDOW_UPDATE, PRIORITY, and request pseudo-header order.
//
// WindowUpdate is zero when no initial connection-level WINDOW_UPDATE was
// present. PseudoHeaderOrder contains full pseudo-header names, such as
// ":method" and ":authority".
type Fingerprint struct {
	Settings          []Setting
	WindowUpdate      uint32
	Priorities        []FingerprintPriority
	HeaderPriority    *FingerprintHeaderPriority
	PseudoHeaderOrder []string
}

// FingerprintPriority is one priority entry in an HTTP/2 fingerprint.
// Entries in Fingerprint.Priorities were sent as standalone PRIORITY frames.
// Weight is the RFC 7540 weight in the range 1..256, rather than the
// zero-indexed byte stored in PriorityParam.Weight.
type FingerprintPriority struct {
	StreamID  uint32
	StreamDep uint32
	Exclusive bool
	Weight    uint16
}

// FingerprintHeaderPriority is the RFC 7540 priority information carried by
// this request's initial HEADERS frame. The stream ID belongs to the HEADERS
// frame itself and is deliberately not stored here; replay uses the stream ID
// allocated by the outgoing connection.
//
// Header priority is request-scoped replay metadata. It is deliberately
// excluded from Fingerprint.String and Fingerprint.Hash, which retain the
// canonical four-part fingerprint representation of standalone PRIORITY
// frames. Weight is the semantic RFC 7540 value in the range 1..256.
type FingerprintHeaderPriority struct {
	StreamDep uint32
	Exclusive bool
	Weight    uint16
}

type requestFingerprintContextKey struct{}
type requestFingerprintWriteContextKey struct{}

// requestFingerprintWriteConfig is immutable after it is stored in a request
// context. The precomputed connection key is shared by pool lookup and stream
// creation so neither path needs to clone or serialize the fingerprint again.
type requestFingerprintWriteConfig struct {
	fingerprint   Fingerprint
	connectionKey string
}

var fingerprintPseudoHeaderToken = map[string]string{
	":method":    "m",
	":authority": "a",
	":scheme":    "s",
	":path":      "p",
}

var fingerprintTokenPseudoHeader = map[string]string{
	"m": ":method",
	"a": ":authority",
	"s": ":scheme",
	"p": ":path",
}

const (
	maxFingerprintSettings   = 100
	maxFingerprintPriorities = 100
)

// Validate reports whether f is a complete, safely replayable HTTP/2
// fingerprint.
func (f Fingerprint) Validate() error {
	if len(f.Settings) > maxFingerprintSettings {
		return errors.New("http2: fingerprint has too many settings")
	}
	seenSettings := make(map[SettingID]bool, len(f.Settings))
	for _, setting := range f.Settings {
		if seenSettings[setting.ID] {
			return fmt.Errorf("http2: fingerprint has duplicate setting %d", setting.ID)
		}
		seenSettings[setting.ID] = true
		if err := setting.Valid(); err != nil {
			return fmt.Errorf("http2: invalid fingerprint setting %d: %w", setting.ID, err)
		}
		if setting.ID == SettingNoRFC7540Priorities && setting.Val > 1 {
			return fmt.Errorf("http2: invalid fingerprint setting %d value %d", setting.ID, setting.Val)
		}
	}

	// A connection starts with 65,535 receive-window bytes. A WINDOW_UPDATE
	// that would take it past the RFC 7540 31-bit maximum is not replayable.
	if f.WindowUpdate > uint32(math.MaxInt32-initialWindowSize) {
		return fmt.Errorf("http2: invalid fingerprint window update %d", f.WindowUpdate)
	}

	if len(f.Priorities) > maxFingerprintPriorities {
		return errors.New("http2: fingerprint has too many priorities")
	}
	for i, priority := range f.Priorities {
		if err := validateFingerprintPriority(i, priority); err != nil {
			return err
		}
	}
	if f.HeaderPriority != nil {
		if !validStreamIDOrZero(f.HeaderPriority.StreamDep) {
			return fmt.Errorf("http2: invalid fingerprint HEADERS priority dependency %d", f.HeaderPriority.StreamDep)
		}
		if f.HeaderPriority.Weight < 1 || f.HeaderPriority.Weight > 256 {
			return fmt.Errorf("http2: invalid fingerprint HEADERS priority weight %d", f.HeaderPriority.Weight)
		}
	}

	seenPseudos := make(map[string]bool, len(f.PseudoHeaderOrder))
	if len(f.PseudoHeaderOrder) == 0 {
		return errors.New("http2: fingerprint has no pseudo-header order")
	}
	for _, name := range f.PseudoHeaderOrder {
		if _, ok := fingerprintPseudoHeaderToken[name]; !ok {
			return fmt.Errorf("http2: unsupported fingerprint pseudo-header %q", name)
		}
		if seenPseudos[name] {
			return fmt.Errorf("http2: duplicate fingerprint pseudo-header %q", name)
		}
		seenPseudos[name] = true
	}
	return nil
}

func validateFingerprintPriority(index int, priority FingerprintPriority) error {
	if !validStreamID(priority.StreamID) {
		return fmt.Errorf("http2: invalid fingerprint priority %d stream ID %d", index, priority.StreamID)
	}
	if !validStreamIDOrZero(priority.StreamDep) {
		return fmt.Errorf("http2: invalid fingerprint priority %d dependency %d", index, priority.StreamDep)
	}
	if priority.StreamID == priority.StreamDep {
		return fmt.Errorf("http2: invalid fingerprint priority %d self-dependency", index)
	}
	if priority.Weight < 1 || priority.Weight > 256 {
		return fmt.Errorf("http2: invalid fingerprint priority %d weight %d", index, priority.Weight)
	}
	return nil
}

// String returns the canonical four-part HTTP/2 fingerprint string.
// HeaderPriority is request-scoped replay metadata and is intentionally not
// included. Callers that construct fingerprints manually should call Validate
// before using String.
func (f Fingerprint) String() string {
	var b strings.Builder
	for i, setting := range f.Settings {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(strconv.FormatUint(uint64(setting.ID), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatUint(uint64(setting.Val), 10))
	}
	b.WriteByte('|')
	if f.WindowUpdate == 0 {
		b.WriteString("00")
	} else {
		b.WriteString(strconv.FormatUint(uint64(f.WindowUpdate), 10))
	}
	b.WriteByte('|')
	if len(f.Priorities) == 0 {
		b.WriteByte('0')
	} else {
		for i, priority := range f.Priorities {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatUint(uint64(priority.StreamID), 10))
			b.WriteByte(':')
			if priority.Exclusive {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(priority.StreamDep), 10))
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(uint64(priority.Weight), 10))
		}
	}
	b.WriteByte('|')
	for i, name := range f.PseudoHeaderOrder {
		if i > 0 {
			b.WriteByte(',')
		}
		if token, ok := fingerprintPseudoHeaderToken[name]; ok {
			b.WriteString(token)
		} else {
			// Preserve enough information to make an invalid manually-built
			// value diagnosable. Validate rejects this representation.
			b.WriteString(name)
		}
	}
	return b.String()
}

// Hash returns the lowercase hexadecimal MD5 of the canonical fingerprint string.
// Like String, it deliberately excludes HeaderPriority.
func (f Fingerprint) Hash() string {
	sum := md5.Sum([]byte(f.String()))
	return hex.EncodeToString(sum[:])
}

// ParseFingerprint parses and validates a canonical four-part HTTP/2
// fingerprint string. The returned Fingerprint has a nil HeaderPriority,
// because that request-scoped metadata is not encoded by String.
func ParseFingerprint(value string) (Fingerprint, error) {
	parts := strings.Split(value, "|")
	if len(parts) != 4 {
		return Fingerprint{}, fmt.Errorf("http2: fingerprint has %d parts; want 4", len(parts))
	}

	var f Fingerprint
	if parts[0] != "" {
		for _, encoded := range strings.Split(parts[0], ";") {
			pair := strings.Split(encoded, ":")
			if len(pair) != 2 {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint setting %q", encoded)
			}
			id, err := strconv.ParseUint(pair[0], 10, 16)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint setting ID %q: %w", pair[0], err)
			}
			val, err := strconv.ParseUint(pair[1], 10, 32)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint setting value %q: %w", pair[1], err)
			}
			f.Settings = append(f.Settings, Setting{ID: SettingID(id), Val: uint32(val)})
		}
	}

	windowUpdate, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint window update %q: %w", parts[1], err)
	}
	f.WindowUpdate = uint32(windowUpdate)

	if parts[2] != "0" {
		if parts[2] == "" {
			return Fingerprint{}, errors.New("http2: empty fingerprint priority section")
		}
		for _, encoded := range strings.Split(parts[2], ",") {
			fields := strings.Split(encoded, ":")
			if len(fields) != 4 {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint priority %q", encoded)
			}
			streamID, err := strconv.ParseUint(fields[0], 10, 32)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint priority stream ID %q: %w", fields[0], err)
			}
			if fields[1] != "0" && fields[1] != "1" {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint priority exclusive bit %q", fields[1])
			}
			streamDep, err := strconv.ParseUint(fields[2], 10, 32)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint priority dependency %q: %w", fields[2], err)
			}
			weight, err := strconv.ParseUint(fields[3], 10, 16)
			if err != nil {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint priority weight %q: %w", fields[3], err)
			}
			f.Priorities = append(f.Priorities, FingerprintPriority{
				StreamID:  uint32(streamID),
				StreamDep: uint32(streamDep),
				Exclusive: fields[1] == "1",
				Weight:    uint16(weight),
			})
		}
	}

	if parts[3] != "" {
		for _, token := range strings.Split(parts[3], ",") {
			name, ok := fingerprintTokenPseudoHeader[token]
			if !ok {
				return Fingerprint{}, fmt.Errorf("http2: invalid fingerprint pseudo-header token %q", token)
			}
			f.PseudoHeaderOrder = append(f.PseudoHeaderOrder, name)
		}
	}
	if err := f.Validate(); err != nil {
		return Fingerprint{}, err
	}
	return f, nil
}

// RequestFingerprint returns the HTTP/2 fingerprint associated with r. On a
// server request, the connection-level portion is frozen when the
// first request HEADERS is accepted, while PseudoHeaderOrder belongs to r.
//
// It returns false for HTTP/1 requests, the initial request of an h2c upgrade,
// and requests not created or annotated by the supported implementation.
func RequestFingerprint(ctx context.Context) (Fingerprint, bool) {
	if ctx == nil {
		return Fingerprint{}, false
	}
	f, ok := fingerprintFromContext(ctx)
	if !ok {
		return Fingerprint{}, false
	}
	return cloneFingerprint(f), true
}

// WithRequestFingerprint returns a shallow copy of req carrying f for
// HTTP/2 connection selection, initial frame replay, and pseudo-header order.
// It is supported only by the legacy implementation and by a directly-used
// Transport with its default connection pool.
func WithRequestFingerprint(ctx context.Context, method string, header Header, f Fingerprint) (context.Context, error) {
	if ctx == nil {
		return nil, errors.New("http2: nil request context")
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	f = cloneFingerprint(f)
	if err := validateFingerprintHeaderConfig(f, requestHeaderOrder(ctx), requestHeaderBlocksToWrite(ctx)); err != nil {
		return nil, err
	}
	if requestHeaderBlocksToWrite(ctx).initial == nil {
		if err := validateFingerprintRequestPseudoSet(method, header, f); err != nil {
			return nil, err
		}
	}
	config := &requestFingerprintWriteConfig{
		fingerprint:   f,
		connectionKey: connectionFingerprintKey(f),
	}
	return context.WithValue(ctx, requestFingerprintWriteContextKey{}, config), nil
}

func cloneFingerprint(f Fingerprint) Fingerprint {
	f.Settings = append([]Setting(nil), f.Settings...)
	f.Priorities = append([]FingerprintPriority(nil), f.Priorities...)
	if f.HeaderPriority != nil {
		priority := *f.HeaderPriority
		f.HeaderPriority = &priority
	}
	f.PseudoHeaderOrder = append([]string(nil), f.PseudoHeaderOrder...)
	return f
}

func fingerprintFromContext(ctx context.Context) (Fingerprint, bool) {
	if f, ok := fingerprintToWriteFromContext(ctx); ok {
		return f, true
	}
	f, ok := ctx.Value(requestFingerprintContextKey{}).(Fingerprint)
	return f, ok
}

func fingerprintToWriteFromContext(ctx context.Context) (Fingerprint, bool) {
	config, ok := fingerprintWriteConfigFromContext(ctx)
	if !ok {
		return Fingerprint{}, false
	}
	return config.fingerprint, true
}

func fingerprintWriteConfigFromContext(ctx context.Context) (*requestFingerprintWriteConfig, bool) {
	config, ok := ctx.Value(requestFingerprintWriteContextKey{}).(*requestFingerprintWriteConfig)
	return config, ok && config != nil
}

func contextWithRequestFingerprint(ctx context.Context, f Fingerprint) context.Context {
	return context.WithValue(ctx, requestFingerprintContextKey{}, cloneFingerprint(f))
}

func connectionFingerprintKey(f Fingerprint) string {
	f.HeaderPriority = nil
	f.PseudoHeaderOrder = nil
	return f.String()
}

func effectiveRequestHeaderOrder(ctx context.Context) HeaderOrder {
	order := requestHeaderOrder(ctx)
	f, ok := fingerprintToWriteFromContext(ctx)
	if !ok || requestHeaderBlocksToWrite(ctx).initial != nil {
		return order
	}
	regular := make([]string, 0, len(order.Headers))
	for _, name := range order.Headers {
		if !strings.HasPrefix(name, ":") {
			regular = append(regular, name)
		}
	}
	order.Headers = append(append([]string(nil), f.PseudoHeaderOrder...), regular...)
	return order
}

func validateFingerprintHeaderConfig(f Fingerprint, order HeaderOrder, blocks requestHeaderBlocksWriteConfig) error {
	if blocks.initial != nil {
		pseudos := make([]string, 0, len(f.PseudoHeaderOrder))
		for _, field := range blocks.initial.Fields {
			if !strings.HasPrefix(field.Name, ":") {
				break
			}
			pseudos = append(pseudos, field.Name)
		}
		if !reflect.DeepEqual(pseudos, f.PseudoHeaderOrder) {
			return fmt.Errorf("http2: exact initial header block pseudo-header order %v conflicts with fingerprint order %v", pseudos, f.PseudoHeaderOrder)
		}
	}

	var orderedPseudos []string
	for _, name := range order.Headers {
		if strings.HasPrefix(name, ":") {
			orderedPseudos = append(orderedPseudos, name)
		}
	}
	if len(orderedPseudos) > 0 && !reflect.DeepEqual(orderedPseudos, f.PseudoHeaderOrder) {
		return fmt.Errorf("http2: request pseudo-header order %v conflicts with fingerprint order %v", orderedPseudos, f.PseudoHeaderOrder)
	}
	return nil
}

func validateFingerprintRequestPseudoSet(method string, header Header, f Fingerprint) error {
	want := map[string]bool{
		":authority": true,
		":method":    true,
	}
	protocol := header.Get(":protocol")
	if method != "CONNECT" || protocol != "" {
		want[":scheme"] = true
		want[":path"] = true
	}
	if protocol != "" {
		want[":protocol"] = true
	}
	if len(f.PseudoHeaderOrder) != len(want) {
		return fmt.Errorf("http2: fingerprint pseudo-header set %v does not match request", f.PseudoHeaderOrder)
	}
	for _, name := range f.PseudoHeaderOrder {
		if !want[name] {
			return fmt.Errorf("http2: fingerprint pseudo-header set %v does not match request", f.PseudoHeaderOrder)
		}
	}
	return nil
}

type fingerprintCollector struct {
	settings     []Setting
	sawSettings  bool
	windowUpdate uint32
	priorities   []FingerprintPriority
	frozen       bool
}

func (c *fingerprintCollector) captureSettings(f *SettingsFrame) {
	if c.frozen || c.sawSettings || f == nil || f.IsAck() {
		return
	}
	c.sawSettings = true
	c.settings = make([]Setting, f.NumSettings())
	for i := range c.settings {
		c.settings[i] = f.Setting(i)
	}
}

func (c *fingerprintCollector) captureWindowUpdate(f *WindowUpdateFrame) {
	if c.frozen || c.windowUpdate != 0 || f == nil || f.StreamID != 0 {
		return
	}
	c.windowUpdate = f.Increment
}

func (c *fingerprintCollector) capturePriority(streamID uint32, p PriorityParam) {
	// Retain one entry past the public validation limit so an overflowed
	// collector produces an invalid fingerprint instead of silently treating a
	// truncated PRIORITY sequence as exact. Further frames are ignored to keep
	// memory bounded until the first request HEADERS frame freezes collection.
	if c.frozen || len(c.priorities) > maxFingerprintPriorities {
		return
	}
	c.priorities = append(c.priorities, FingerprintPriority{
		StreamID:  streamID,
		StreamDep: p.StreamDep,
		Exclusive: p.Exclusive,
		Weight:    uint16(p.Weight) + 1,
	})
}

func (c *fingerprintCollector) freeze() {
	c.frozen = true
}

func (c *fingerprintCollector) fingerprint(fields []hpack.HeaderField, headerPriority *FingerprintHeaderPriority) Fingerprint {
	f := Fingerprint{
		Settings:       append([]Setting(nil), c.settings...),
		WindowUpdate:   c.windowUpdate,
		Priorities:     append([]FingerprintPriority(nil), c.priorities...),
		HeaderPriority: headerPriority,
	}
	for _, field := range fields {
		if !field.IsPseudo() {
			break
		}
		f.PseudoHeaderOrder = append(f.PseudoHeaderOrder, field.Name)
	}
	return f
}

func fingerprintHeaderPriority(p PriorityParam) FingerprintHeaderPriority {
	return FingerprintHeaderPriority{
		StreamDep: p.StreamDep,
		Exclusive: p.Exclusive,
		Weight:    uint16(p.Weight) + 1,
	}
}
