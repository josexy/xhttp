// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"slices"
	"strings"

	"github.com/josexy/xhttp/httptrace"
	"github.com/josexy/xhttp/internal/ascii"
	"golang.org/x/net/http/httpguts"
)

// HeaderOrder specifies the order in which HTTP/1 or HTTP/2 fields are
// written. Headers applies to initial request and response fields and to
// explicitly written informational responses. Trailers applies to trailers.
// Field-name matching is case-insensitive. Missing names are ignored, repeated
// values remain grouped, and unlisted fields follow listed fields in lowercase
// lexical order. A non-empty slice enables ordering for that block type.
type HeaderOrder struct {
	Headers  []string
	Trailers []string
}

type requestHeaderOrderContextKey struct{}

func normalizePublicHeaderOrder(order HeaderOrder) (HeaderOrder, error) {
	headers, err := normalizePublicHeaderNames(order.Headers)
	if err != nil {
		return HeaderOrder{}, fmt.Errorf("http: invalid header order: %w", err)
	}
	trailers, err := normalizePublicHeaderNames(order.Trailers)
	if err != nil {
		return HeaderOrder{}, fmt.Errorf("http: invalid trailer order: %w", err)
	}
	return HeaderOrder{Headers: headers, Trailers: trailers}, nil
}

func normalizePublicHeaderNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	result := make([]string, len(names))
	seen := make(map[string]bool, len(names))
	for i, name := range names {
		var ok bool
		name, ok = ascii.ToLower(name)
		if !ok {
			return nil, fmt.Errorf("non-ASCII field name %q", name)
		}
		if name == "" {
			return nil, errors.New("empty field name")
		}
		if strings.HasPrefix(name, ":") {
			switch name {
			case ":authority", ":method", ":path", ":protocol", ":scheme", ":status":
			default:
				return nil, fmt.Errorf("unsupported pseudo-header %q", name)
			}
		} else if !httpguts.ValidHeaderFieldName(name) {
			return nil, fmt.Errorf("invalid field name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate field name %q", name)
		}
		seen[name] = true
		result[i] = name
	}
	return result, nil
}

func requestHeaderOrder(ctx context.Context) HeaderOrder {
	order, _ := ctx.Value(requestHeaderOrderContextKey{}).(HeaderOrder)
	return order
}

func headerFieldsFromHeader(h Header, exclude map[string]bool) []HeaderField {
	kvs, sorter := h.sortedKeyValues(exclude)
	fields := make([]HeaderField, 0)
	for _, kv := range kvs {
		if !httpguts.ValidHeaderFieldName(kv.key) {
			continue
		}
		for _, value := range kv.values {
			value = headerNewlineToSpace.Replace(value)
			value = textproto.TrimString(value)
			fields = append(fields, HeaderField{Name: kv.key, Value: value})
		}
	}
	headerSorterPool.Put(sorter)
	return fields
}

func writeHTTP1HeaderFields(w io.Writer, fields []HeaderField, order []string, trace *httptrace.ClientTrace) error {
	type fieldGroup struct {
		lower  string
		fields []HeaderField
	}
	groupsByName := make(map[string]*fieldGroup)
	groups := make([]*fieldGroup, 0)
	for _, field := range fields {
		lower, ok := ascii.ToLower(field.Name)
		if !ok || strings.HasPrefix(lower, ":") || !httpguts.ValidHeaderFieldName(field.Name) {
			continue
		}
		group := groupsByName[lower]
		if group == nil {
			group = &fieldGroup{lower: lower}
			groupsByName[lower] = group
			groups = append(groups, group)
		}
		group.fields = append(group.fields, field)
	}

	positions := make(map[string]int, len(order))
	for i, name := range order {
		if !strings.HasPrefix(name, ":") {
			positions[name] = i
		}
	}
	slices.SortStableFunc(groups, func(a, b *fieldGroup) int {
		ai, aListed := positions[a.lower]
		bi, bListed := positions[b.lower]
		switch {
		case aListed && bListed:
			return ai - bi
		case aListed:
			return -1
		case bListed:
			return 1
		default:
			return strings.Compare(a.lower, b.lower)
		}
	})

	ws, ok := w.(io.StringWriter)
	if !ok {
		ws = stringWriter{w}
	}
	for _, group := range groups {
		for start := 0; start < len(group.fields); {
			name := group.fields[start].Name
			values := make([]string, 0, len(group.fields)-start)
			end := start
			for end < len(group.fields) && group.fields[end].Name == name {
				field := group.fields[end]
				for _, part := range []string{field.Name, ": ", field.Value, "\r\n"} {
					if _, err := ws.WriteString(part); err != nil {
						return err
					}
				}
				values = append(values, field.Value)
				end++
			}
			if trace != nil && trace.WroteHeaderField != nil {
				trace.WroteHeaderField(name, values)
			}
			start = end
		}
	}
	return nil
}

// WithRequestHeaderOrder returns a shallow copy of req whose HTTP/1 or
// HTTP/2 initial headers and trailers are written in order.
func WithRequestHeaderOrder(req *Request, order HeaderOrder) (*Request, error) {
	if req == nil {
		return nil, errors.New("http: nil request")
	}
	order, err := normalizePublicHeaderOrder(order)
	if err != nil {
		return nil, err
	}
	blocks := requestHeaderBlocksToWrite(req.Context())
	if len(order.Headers) > 0 && blocks.initial != nil {
		return nil, errors.New("http: request header order conflicts with an exact initial header block")
	}
	if len(order.Trailers) > 0 && blocks.trailers != nil {
		return nil, errors.New("http: request trailer order conflicts with an exact trailer block provider")
	}
	req, err = http2WithRequestHeaderOrderBridge(req, order)
	if err != nil {
		return nil, err
	}
	if len(order.Headers) == 0 && len(order.Trailers) == 0 {
		return req, nil
	}
	return req.WithContext(context.WithValue(req.Context(), requestHeaderOrderContextKey{}, order)), nil
}

type http1responseHeaderOrderSetter interface {
	setHTTP1HeaderOrder(HeaderOrder) error
}

type responseWriterUnwrapper interface {
	Unwrap() ResponseWriter
}

// SetResponseHeaderOrder configures the order of HTTP/1 or HTTP/2 response
// headers and trailers written by w. It must be called before the response is
// committed. It follows wrappers implementing Unwrap.
func SetResponseHeaderOrder(w ResponseWriter, order HeaderOrder) error {
	if w == nil {
		return errors.New("http: nil response writer")
	}
	order, err := normalizePublicHeaderOrder(order)
	if err != nil {
		return err
	}
	current := w
	for range 100 {
		if setter, ok := current.(http1responseHeaderOrderSetter); ok {
			return setter.setHTTP1HeaderOrder(order)
		}
		uw, ok := current.(responseWriterUnwrapper)
		if !ok {
			break
		}
		current = uw.Unwrap()
		if current == nil {
			return fmt.Errorf("http: response header order: %w", errors.ErrUnsupported)
		}
	}
	if current != w {
		w = current
	}
	return http2SetResponseHeaderOrderBridge(w, order)
}
