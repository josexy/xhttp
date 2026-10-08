// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/textproto"

	"github.com/josexy/xhttp/httptrace"
	"github.com/josexy/xhttp/internal"
	"github.com/josexy/xhttp/internal/ascii"
	"golang.org/x/net/http/httpguts"
)

type http1ExactRequestPlan struct {
	block            HeaderBlock
	contentLength    int64
	hasContentLength bool
	chunked          bool
	trailerNames     map[string]bool
}

func prepareHTTP1ExactRequest(req *Request, config requestHeaderBlocksWriteConfig) (http1ExactRequestPlan, error) {
	if config.initial == nil || config.proto != 1 {
		return http1ExactRequestPlan{}, errors.New("http: exact request header block protocol does not match HTTP/1")
	}
	block := cloneHeaderBlock(*config.initial)
	if err := validateHTTP1HeaderBlock(block, HeaderBlockInitial); err != nil {
		return http1ExactRequestPlan{}, err
	}
	if block.StatusCode != 0 {
		return http1ExactRequestPlan{}, errors.New("http: exact HTTP/1 request block must not contain StatusCode")
	}

	plan := http1ExactRequestPlan{block: block}
	var hostCount, contentLengthCount, transferEncodingCount int
	for _, field := range block.Fields {
		lower, _ := ascii.ToLower(field.Name)
		switch lower {
		case "host":
			hostCount++
			if !httpguts.ValidHostHeader(field.Value) {
				return plan, errors.New("http: invalid Host field in exact request header block")
			}
		case "content-length":
			contentLengthCount++
			length, err := parseContentLength([]string{field.Value})
			if err != nil {
				return plan, fmt.Errorf("http: invalid Content-Length in exact request header block: %w", err)
			}
			plan.contentLength = length
			plan.hasContentLength = true
		case "transfer-encoding":
			transferEncodingCount++
			if !ascii.EqualFold(textproto.TrimString(field.Value), "chunked") {
				return plan, fmt.Errorf("http: unsupported Transfer-Encoding %q in exact request header block", field.Value)
			}
			plan.chunked = true
		case "trailer":
			if plan.trailerNames == nil {
				plan.trailerNames = make(map[string]bool)
			}
			var trailerErr error
			foreachHeaderElement(field.Value, func(name string) {
				canonical := CanonicalHeaderKey(name)
				if !httpguts.ValidTrailerHeader(canonical) {
					trailerErr = badStringError("bad trailer key", name)
					return
				}
				lower, ok := ascii.ToLower(name)
				if !ok || plan.trailerNames[lower] {
					trailerErr = fmt.Errorf("http: duplicate or invalid trailer declaration %q", name)
					return
				}
				plan.trailerNames[lower] = true
			})
			if trailerErr != nil {
				return plan, trailerErr
			}
		}
	}
	if hostCount != 1 {
		return plan, fmt.Errorf("http: exact HTTP/1 request header block contains %d Host fields; want exactly 1", hostCount)
	}
	if contentLengthCount > 1 {
		return plan, errors.New("http: exact HTTP/1 request header block contains multiple Content-Length fields")
	}
	if transferEncodingCount > 1 {
		return plan, errors.New("http: exact HTTP/1 request header block contains multiple Transfer-Encoding fields")
	}
	if plan.hasContentLength && plan.chunked {
		return plan, errors.New("http: exact HTTP/1 request header block contains both Content-Length and Transfer-Encoding")
	}
	if len(plan.trailerNames) > 0 && !plan.chunked {
		return plan, errors.New("http: exact HTTP/1 request trailers require Transfer-Encoding: chunked")
	}

	hasBody := req.Body != nil && req.Body != NoBody
	outgoingLength := req.outgoingLength()
	switch {
	case plan.hasContentLength:
		if !hasBody && plan.contentLength != 0 {
			return plan, fmt.Errorf("http: exact Content-Length is %d with nil Body", plan.contentLength)
		}
		if hasBody && outgoingLength >= 0 && outgoingLength != plan.contentLength {
			return plan, fmt.Errorf("http: Request.ContentLength=%d does not match exact Content-Length=%d", outgoingLength, plan.contentLength)
		}
	case plan.chunked:
		if req.ContentLength > 0 {
			return plan, errors.New("http: positive Request.ContentLength conflicts with exact chunked framing")
		}
	case hasBody:
		return plan, errors.New("http: exact HTTP/1 request with a Body requires Content-Length or Transfer-Encoding: chunked")
	}
	if config.trailers != nil && !plan.chunked {
		return plan, errors.New("http: exact request trailer block provider requires Transfer-Encoding: chunked")
	}

	if config.trailers == nil && len(req.Trailer) > 0 {
		if invalid := validateHeaders(req.Trailer); invalid != "" {
			return plan, fmt.Errorf("http: invalid exact request trailer %s", invalid)
		}
		if !plan.chunked {
			return plan, errors.New("http: request trailers require exact chunked framing")
		}
		if err := validateExactTrailerNameSet(headerFieldsFromHeader(req.Trailer, nil), plan.trailerNames); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func validateExactTrailerNameSet(fields []HeaderField, declared map[string]bool) error {
	actual := make(map[string]bool)
	for _, field := range fields {
		if !httpguts.ValidTrailerHeader(CanonicalHeaderKey(field.Name)) {
			return fmt.Errorf("http: field %q is not valid in trailers", field.Name)
		}
		lower, ok := ascii.ToLower(field.Name)
		if !ok {
			return fmt.Errorf("http: invalid trailer field %q", field.Name)
		}
		actual[lower] = true
	}
	if len(actual) == 0 {
		return nil
	}
	if len(actual) != len(declared) {
		return errors.New("http: exact trailer field names do not match the Trailer declaration")
	}
	for name := range actual {
		if !declared[name] {
			return errors.New("http: exact trailer field names do not match the Trailer declaration")
		}
	}
	return nil
}

func requestExpectsContinue(req *Request) bool {
	config := requestHeaderBlocksToWrite(req.Context())
	if config.initial == nil || config.proto != 1 {
		return req.expectsContinue()
	}
	for _, field := range config.initial.Fields {
		if ascii.EqualFold(field.Name, "Expect") && hasToken(field.Value, "100-continue") {
			return true
		}
	}
	return false
}

func requestWantsClose(req *Request) bool {
	config := requestHeaderBlocksToWrite(req.Context())
	if config.initial != nil && config.proto == 1 {
		for _, field := range config.initial.Fields {
			if ascii.EqualFold(field.Name, "Connection") && hasToken(field.Value, "close") {
				return true
			}
		}
		return false
	}
	return req.wantsClose()
}

func (r *Request) writeExactHTTP1(w io.Writer, ruri string, config requestHeaderBlocksWriteConfig, waitForContinue func() bool, trace *httptrace.ClientTrace) error {
	plan, err := prepareHTTP1ExactRequest(r, config)
	if err != nil {
		return err
	}
	var bw *bufio.Writer
	if _, ok := w.(io.ByteWriter); !ok {
		bw = bufio.NewWriter(w)
		w = bw
	}
	if _, err := fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", valueOrDefault(r.Method, "GET"), ruri); err != nil {
		return err
	}
	if err := writeHTTP1Fields(w, plan.block.Fields, trace); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\r\n"); err != nil {
		return err
	}
	if trace != nil && trace.WroteHeaders != nil {
		trace.WroteHeaders()
	}
	if waitForContinue != nil {
		if buffered, ok := w.(*bufio.Writer); ok {
			if err := buffered.Flush(); err != nil {
				return err
			}
		}
		if trace != nil && trace.Wait100Continue != nil {
			trace.Wait100Continue()
		}
		if !waitForContinue() {
			return nil
		}
	}
	if buffered, ok := w.(*bufio.Writer); ok && r.Body != nil && r.Body != NoBody &&
		!isKnownInMemoryReader(r.Body) {
		if err := buffered.Flush(); err != nil {
			return err
		}
	}

	if r.Body != nil && r.Body != NoBody {
		switch {
		case plan.hasContentLength:
			if err := copyExactContentLength(w, r.Body, plan.contentLength); err != nil {
				return err
			}
		case plan.chunked:
			chunkWriter := internal.NewChunkedWriter(http1RequestChunkWriter(w))
			if _, err := io.Copy(chunkWriter, r.Body); err != nil {
				return requestBodyReadError{err}
			}
			if err := chunkWriter.Close(); err != nil {
				return err
			}
		}
	} else if plan.chunked {
		chunkWriter := internal.NewChunkedWriter(http1RequestChunkWriter(w))
		if err := chunkWriter.Close(); err != nil {
			return err
		}
	}

	if plan.chunked {
		trailerFields, err := exactRequestTrailerFields(r, config, plan)
		if err != nil {
			return err
		}
		if err := writeHTTP1Fields(w, trailerFields, nil); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return err
		}
	}
	if bw != nil {
		return bw.Flush()
	}
	return nil
}

func http1RequestChunkWriter(w io.Writer) io.Writer {
	if buffered, ok := w.(*bufio.Writer); ok {
		return &internal.FlushAfterChunkWriter{Writer: buffered}
	}
	return w
}

func copyExactContentLength(w io.Writer, body io.Reader, length int64) error {
	written, err := io.CopyN(w, body, length)
	if err != nil {
		if err == io.EOF {
			return fmt.Errorf("http: ContentLength=%d with Body length %d", length, written)
		}
		return requestBodyReadError{err}
	}
	var extra [1]byte
	for range 100 {
		n, readErr := body.Read(extra[:])
		if n != 0 {
			return fmt.Errorf("http: ContentLength=%d with Body length greater than %d", length, length)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return requestBodyReadError{readErr}
		}
	}
	return requestBodyReadError{io.ErrNoProgress}
}

func exactRequestTrailerFields(req *Request, config requestHeaderBlocksWriteConfig, plan http1ExactRequestPlan) ([]HeaderField, error) {
	if config.trailers != nil {
		block, err := config.trailers()
		if err != nil {
			return nil, err
		}
		if headerBlockProtocol(block) != 1 {
			return nil, errors.New("http: exact request trailer block protocol does not match HTTP/1")
		}
		if err := validateHTTP1HeaderBlock(block, HeaderBlockTrailer); err != nil {
			return nil, err
		}
		if block.StatusCode != 0 {
			return nil, errors.New("http: exact HTTP/1 trailer block must not contain StatusCode")
		}
		if err := validateExactTrailerNameSet(block.Fields, plan.trailerNames); err != nil {
			return nil, err
		}
		return append([]HeaderField(nil), block.Fields...), nil
	}
	fields := headerFieldsFromHeader(req.Trailer, nil)
	if err := validateExactTrailerNameSet(fields, plan.trailerNames); err != nil {
		return nil, err
	}
	if order := requestHeaderOrder(req.Context()).Trailers; len(order) > 0 {
		return orderHTTP1HeaderFields(fields, order), nil
	}
	return fields, nil
}
