// Copyright 2026 The xhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type headerReadError struct{ err error }

func (r headerReadError) Read([]byte) (int, error) { return 0, r.err }

func TestHeaderBlockContinuationReadError(t *testing.T) {
	wantErr := errors.New("continuation read failed")
	// Fill complete buffers so the failure occurs while assembling a continued
	// line. No partial field may be published after the underlying read fails.
	r := io.MultiReader(strings.NewReader("X-Test: start!\r\n\t"+strings.Repeat("a", 16383)), headerReadError{wantErr})
	h, block, err := readMIMEHeaderBlock(textproto.NewReader(bufio.NewReaderSize(r, 16)), HeaderBlockInitial, 0, 100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("read error = %v, want %v", err, wantErr)
	}
	if len(h) != 0 || len(block.Fields) != 0 {
		t.Fatalf("published partial header: %v, %v", h, block.Fields)
	}
}

func TestHTTP1ExactCONNECTResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		fields  []HeaderField
		wantErr bool
	}{
		{"tunnel", 200, nil, false},
		{"rejected", 403, []HeaderField{{Name: "Content-Length", Value: "0"}}, false},
		{"content length", 200, []HeaderField{{Name: "Content-Length", Value: "0"}}, true},
		{"chunked", 200, []HeaderField{{Name: "Transfer-Encoding", Value: "chunked"}}, true},
		{"keep alive rejection", 403, []HeaderField{{Name: "Content-Length", Value: "0"}, {Name: "Connection", Value: "keep-alive"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errCh := make(chan error, 1)
			url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
				// Middleware must not bypass the received CONNECT method's rules.
				r.Method = "GET"
				err := WriteResponseHeaderBlock(w, HeaderBlock{Kind: HeaderBlockInitial, ProtoMajor: 1, StatusCode: tc.status, Fields: tc.fields})
				errCh <- err
				if err != nil {
					w.WriteHeader(StatusBadRequest)
				}
			}))
			defer closeServer()
			conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(conn, "CONNECT example.test:443 HTTP/1.1\r\nHost: example.test:443\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			wire, err := io.ReadAll(conn)
			if err != nil {
				t.Fatalf("connection was not closed after CONNECT: %v", err)
			}
			if err := <-errCh; (err != nil) != tc.wantErr {
				t.Fatalf("WriteResponseHeaderBlock = %v, want error %v", err, tc.wantErr)
			}
			if !tc.wantErr && tc.status == 200 && (strings.Contains(string(wire), "Content-Length:") || strings.Contains(string(wire), "Transfer-Encoding:")) {
				t.Fatalf("tunnel response has HTTP framing: %q", wire)
			}
		})
	}
}
