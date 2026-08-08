// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"slices"

	"github.com/josexy/net/http/httpguts"
	"github.com/josexy/xhttp/internal/ascii"
)

func (w *response) prepareRequestBodyForExactResponse() {
	if expect, ok := w.req.Body.(*expectContinueReader); ok && !expect.sawEOF.Load() {
		w.closeAfterReply = true
	}
	if w.req.ContentLength == 0 || w.closeAfterReply || w.fullDuplex {
		return
	}
	var discard, tooBig bool
	switch bdy := w.req.Body.(type) {
	case *expectContinueReader:
		// A fully-read expectContinueReader needs no further work.
	case *body:
		bdy.mu.Lock()
		switch {
		case bdy.closed:
			if !bdy.sawEOF {
				w.closeAfterReply = true
			}
		case bdy.unreadDataSizeLocked() >= maxPostHandlerReadBytes:
			tooBig = true
		default:
			discard = true
		}
		bdy.mu.Unlock()
	default:
		discard = true
	}
	if discard {
		_, err := io.CopyN(io.Discard, w.reqBody, maxPostHandlerReadBytes+1)
		switch err {
		case nil:
			tooBig = true
		case ErrBodyReadAfterClose:
		case io.EOF:
			if err := w.reqBody.Close(); err != nil {
				w.closeAfterReply = true
			}
		default:
			w.closeAfterReply = true
		}
	}
	if tooBig {
		w.requestBodyLimitHit = true
		w.closeAfterReply = true
	}
}

type http1ExactResponsePlan struct {
	contentLength    int64
	hasContentLength bool
	chunked          bool
	close            bool
	trailerNames     map[string]bool
}

func prepareHTTP1ExactResponse(w *response, block HeaderBlock) (http1ExactResponsePlan, error) {
	var plan http1ExactResponsePlan
	if w.req.ProtoMajor != 1 {
		return plan, fmt.Errorf("http: exact HTTP/1 response block does not match request protocol HTTP/%d", w.req.ProtoMajor)
	}
	if block.StatusCode < 100 || block.StatusCode > 999 {
		return plan, fmt.Errorf("http: invalid exact HTTP/1 response StatusCode %d", block.StatusCode)
	}
	informational := block.Kind == HeaderBlockInformational
	if informational {
		if block.StatusCode < 100 || block.StatusCode > 199 || block.StatusCode == StatusSwitchingProtocols {
			return plan, errors.New("http: informational response block requires a non-terminal 1xx StatusCode")
		}
		if !w.req.ProtoAtLeast(1, 1) {
			return plan, errors.New("http: informational responses are not supported for HTTP/1.0")
		}
	} else if block.Kind != HeaderBlockInitial {
		return plan, errors.New("http: final response requires HeaderBlockInitial")
	} else if block.StatusCode >= 100 && block.StatusCode <= 199 && block.StatusCode != StatusSwitchingProtocols {
		return plan, errors.New("http: non-terminal 1xx StatusCode requires HeaderBlockInformational")
	}

	var contentLengthCount, transferEncodingCount, trailerFieldCount int
	for _, field := range block.Fields {
		lower, _ := ascii.ToLower(field.Name)
		switch lower {
		case "content-length":
			contentLengthCount++
			length, err := parseContentLength([]string{field.Value})
			if err != nil {
				return plan, fmt.Errorf("http: invalid Content-Length in exact response header block: %w", err)
			}
			plan.contentLength = length
			plan.hasContentLength = true
		case "transfer-encoding":
			transferEncodingCount++
			if !ascii.EqualFold(textproto.TrimString(field.Value), "chunked") {
				return plan, fmt.Errorf("http: unsupported Transfer-Encoding %q in exact response header block", field.Value)
			}
			plan.chunked = true
		case "trailer":
			trailerFieldCount++
			if plan.trailerNames == nil {
				plan.trailerNames = make(map[string]bool)
			}
			var trailerErr error
			foreachHeaderElement(field.Value, func(name string) {
				canonical := CanonicalHeaderKey(name)
				lower, ok := ascii.ToLower(name)
				if !ok || !httpguts.ValidTrailerHeader(canonical) || plan.trailerNames[lower] {
					trailerErr = fmt.Errorf("http: invalid or duplicate trailer declaration %q", name)
					return
				}
				plan.trailerNames[lower] = true
			})
			if trailerErr != nil {
				return plan, trailerErr
			}
		}
	}
	if contentLengthCount > 1 {
		return plan, errors.New("http: exact HTTP/1 response block contains multiple Content-Length fields")
	}
	if transferEncodingCount > 1 {
		return plan, errors.New("http: exact HTTP/1 response block contains multiple Transfer-Encoding fields")
	}
	if plan.hasContentLength && plan.chunked {
		return plan, errors.New("http: exact HTTP/1 response block contains both Content-Length and Transfer-Encoding")
	}
	if informational {
		if contentLengthCount != 0 || transferEncodingCount != 0 || trailerFieldCount != 0 {
			return plan, errors.New("http: informational response block cannot contain framing fields")
		}
		return plan, nil
	}
	if plan.chunked && !w.req.ProtoAtLeast(1, 1) {
		return plan, errors.New("http: exact chunked response is not supported for HTTP/1.0")
	}
	if len(plan.trailerNames) > 0 && !plan.chunked {
		return plan, errors.New("http: exact response trailers require Transfer-Encoding: chunked")
	}
	bodyAllowed := bodyAllowedForStatus(block.StatusCode) && w.req.Method != "HEAD"
	if !bodyAllowed {
		if plan.chunked {
			return plan, fmt.Errorf("http: exact response status %d cannot use chunked framing", block.StatusCode)
		}
		if plan.hasContentLength && w.req.Method != "HEAD" && block.StatusCode != StatusNotModified &&
			(block.StatusCode == StatusNoContent || block.StatusCode >= 100 && block.StatusCode <= 199 || plan.contentLength != 0) {
			return plan, fmt.Errorf("http: exact response status %d cannot use the supplied Content-Length", block.StatusCode)
		}
	}
	if bodyAllowed && !plan.hasContentLength && !plan.chunked {
		plan.close = true
	}
	if plan.close && exactHeaderHasToken(block, "connection", "keep-alive") {
		return plan, errors.New("http: close-delimited exact response conflicts with Connection: keep-alive")
	}
	if !w.req.ProtoAtLeast(1, 1) {
		plan.close = true
		if w.wants10KeepAlive && exactHeaderHasToken(block, "connection", "keep-alive") &&
			(plan.hasContentLength || !bodyAllowed) {
			plan.close = false
		}
	}
	return plan, nil
}

func (w *response) writeHTTP1HeaderBlock(block HeaderBlock) error {
	if w.conn.hijacked() {
		return ErrHijacked
	}
	if w.wroteHeader || w.cw.wroteHeader {
		return errors.New("http: exact response header block written after final response headers")
	}
	if len(w.headerOrder.Headers) > 0 {
		return errors.New("http: exact response header block conflicts with response header order")
	}
	plan, err := prepareHTTP1ExactResponse(w, block)
	if err != nil {
		return err
	}
	if block.Kind == HeaderBlockInformational {
		if block.StatusCode == StatusContinue {
			w.disableWriteContinue()
		}
		writeStatusLine(w.conn.bufw, w.req.ProtoAtLeast(1, 1), block.StatusCode, w.statusBuf[:])
		if err := writeExactHTTP1Fields(w.conn.bufw, block.Fields, nil); err != nil {
			return err
		}
		if _, err := w.conn.bufw.Write(crlf); err != nil {
			return err
		}
		return w.conn.bufw.Flush()
	}
	if w.exactTrailerBlock != nil {
		if !plan.chunked {
			return errors.New("http: exact trailer block requires an exact chunked response")
		}
		if err := validateExactTrailerNameSet(w.exactTrailerBlock.Fields, plan.trailerNames); err != nil {
			return err
		}
	}
	w.disableWriteContinue()
	w.wroteHeader = true
	w.status = block.StatusCode
	w.exactHeaderBlock = &block
	w.exactTrailerNames = plan.trailerNames
	w.contentLength = -1
	if plan.hasContentLength {
		w.contentLength = plan.contentLength
	}
	w.cw.chunking = plan.chunked
	if plan.close || w.wantsClose || !w.conn.server.doKeepAlives() || exactHeaderHasToken(block, "connection", "close") {
		w.closeAfterReply = true
	}
	w.trailers = w.trailers[:0]
	for name := range plan.trailerNames {
		w.trailers = append(w.trailers, CanonicalHeaderKey(name))
	}
	slices.Sort(w.trailers)
	return nil
}

func exactHeaderHasToken(block HeaderBlock, name, token string) bool {
	for _, field := range block.Fields {
		if ascii.EqualFold(field.Name, name) && hasToken(field.Value, token) {
			return true
		}
	}
	return false
}

func (w *response) setHTTP1TrailerBlock(block HeaderBlock) error {
	if w.conn.hijacked() {
		return ErrHijacked
	}
	if w.handlerDone.Load() {
		return errors.New("http: exact response trailer block set after the handler finished")
	}
	if len(w.headerOrder.Trailers) > 0 {
		return errors.New("http: exact response trailer block conflicts with response trailer order")
	}
	if len(block.Fields) == 0 {
		w.exactTrailerBlock = nil
		return nil
	}
	if !w.req.ProtoAtLeast(1, 1) {
		return errors.New("http: exact response trailers are not supported for HTTP/1.0")
	}
	declared := w.exactTrailerNames
	if declared == nil {
		declared = declaredTrailerNames(w.handlerHeader)
		for _, name := range w.trailers {
			if lower, ok := ascii.ToLower(name); ok {
				declared[lower] = true
			}
		}
	}
	if err := validateExactTrailerNameSet(block.Fields, declared); err != nil {
		return err
	}
	if w.cw.wroteHeader && !w.cw.chunking {
		return errors.New("http: exact response trailers require chunked framing")
	}
	w.exactTrailerBlock = &block
	return nil
}

func declaredTrailerNames(header Header) map[string]bool {
	declared := make(map[string]bool)
	for _, value := range header["Trailer"] {
		foreachHeaderElement(value, func(name string) {
			if lower, ok := ascii.ToLower(name); ok {
				declared[lower] = true
			}
		})
	}
	return declared
}

func (w *response) exactFallbackTrailerFields() []HeaderField {
	if len(w.exactTrailerNames) == 0 {
		return nil
	}
	fields := make([]HeaderField, 0)
	for _, name := range w.trailers {
		for _, value := range w.handlerHeader[name] {
			fields = append(fields, HeaderField{Name: name, Value: textproto.TrimString(headerNewlineToSpace.Replace(value))})
		}
	}
	return fields
}
