// Copyright 2026 The xhttp Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHTTP1MaxBytesLimitHeaderModes checks that hitting a MaxBytesHandler limit
// closes the connection after the reply in every HTTP/1 response header mode,
// without the limit mutating the handler's Header map. With concurrent set,
// the body is read on another goroutine while the handler writes its response,
// which must not race under -race.
func TestHTTP1MaxBytesLimitHeaderModes(t *testing.T) {
	for _, mode := range []string{"default", "name order", "exact block"} {
		for _, concurrent := range []bool{false, true} {
			name := mode
			if concurrent {
				name += "/concurrent"
			}
			t.Run(name, func(t *testing.T) {
				testHTTP1MaxBytesLimitHeaderMode(t, mode, concurrent)
			})
		}
	}
}

func testHTTP1MaxBytesLimitHeaderMode(t *testing.T, mode string, concurrent bool) {
	errCh := make(chan error, 1)
	connHeaderCh := make(chan string, 1)
	handler := HandlerFunc(func(w ResponseWriter, r *Request) {
		var wg sync.WaitGroup
		defer wg.Wait()
		if concurrent {
			wg.Go(func() { io.ReadAll(r.Body) })
		} else {
			io.ReadAll(r.Body)
			connHeaderCh <- w.Header().Get("Connection")
		}
		var err error
		switch mode {
		case "default":
			w.Header().Set("X-Test", "1")
			w.WriteHeader(StatusRequestEntityTooLarge)
		case "name order":
			err = SetResponseHeaderOrder(w, HeaderOrder{Headers: []string{"connection", "x-test"}})
			w.Header().Set("X-Test", "1")
			w.WriteHeader(StatusRequestEntityTooLarge)
		case "exact block":
			err = WriteResponseHeaderBlock(w, HeaderBlock{
				Kind:       HeaderBlockInitial,
				ProtoMajor: 1,
				StatusCode: StatusRequestEntityTooLarge,
				Fields:     []HeaderField{{Name: "X-Test", Value: "1"}, {Name: "Content-Length", Value: "0"}},
			})
		}
		errCh <- err
	})
	url, closeServer := startHTTP1TestServer(t, MaxBytesHandler(handler, 4))
	defer closeServer()

	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 5\r\n\r\nhello"); err != nil {
		t.Fatal(err)
	}
	// The server must close the connection after the reply. If it is kept
	// alive, ReadAll fails with the deadline instead.
	wire, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("connection was not closed after exceeding the body limit: %v; got %q", err, wire)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("handler response setup: %v", err)
	}
	if !concurrent {
		if got := <-connHeaderCh; got != "" {
			t.Errorf("handler Header()[Connection] = %q after body limit, want unset", got)
		}
	}

	head, _, _ := strings.Cut(string(wire), "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if want := "HTTP/1.1 413 Request Entity Too Large"; lines[0] != want {
		t.Fatalf("status line = %q, want %q", lines[0], want)
	}
	fields := lines[1:]
	if mode == "exact block" {
		// The exact block is written verbatim; the server closes the
		// connection without adding Connection: close.
		if want := []string{"X-Test: 1", "Content-Length: 0"}; strings.Join(fields, "\n") != strings.Join(want, "\n") {
			t.Fatalf("exact response fields = %q, want %q", fields, want)
		}
		return
	}
	connIdx, testIdx := -1, -1
	for i, line := range fields {
		switch line {
		case "Connection: close":
			connIdx = i
		case "X-Test: 1":
			testIdx = i
		}
	}
	if connIdx < 0 || testIdx < 0 {
		t.Fatalf("response fields = %q, want Connection: close and X-Test: 1", fields)
	}
	if mode == "name order" && (connIdx != 0 || testIdx != 1) {
		t.Fatalf("ordered response fields = %q, want Connection then X-Test first", fields)
	}
	resp, err := ReadResponse(bufio.NewReader(strings.NewReader(string(wire))), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	resp.Body.Close()
}

// TestHTTP1MaxBytesLimitSkipsBodyDrain checks that once a MaxBytesHandler
// limit is hit, the server writes its response without first draining the rest
// of the request body. The client sends only part of the declared body and
// waits for the response, so a drain before the response would block until
// the deadline. (The server may still read some of the body after replying.)
func TestHTTP1MaxBytesLimitSkipsBodyDrain(t *testing.T) {
	for _, mode := range []string{"default", "exact block"} {
		t.Run(mode, func(t *testing.T) {
			handler := HandlerFunc(func(w ResponseWriter, r *Request) {
				if _, err := io.ReadAll(r.Body); err == nil {
					t.Error("body read did not hit the limit")
				}
				if mode == "exact block" {
					if err := WriteResponseHeaderBlock(w, HeaderBlock{
						Kind:       HeaderBlockInitial,
						ProtoMajor: 1,
						StatusCode: StatusRequestEntityTooLarge,
						Fields:     []HeaderField{{Name: "Content-Length", Value: "0"}},
					}); err != nil {
						t.Errorf("WriteResponseHeaderBlock: %v", err)
					}
					return
				}
				w.WriteHeader(StatusRequestEntityTooLarge)
			})
			url, closeServer := startHTTP1TestServer(t, MaxBytesHandler(handler, 4))
			defer closeServer()

			conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			// Declare a body below maxPostHandlerReadBytes, which the server
			// would otherwise try to drain, but send only its first bytes.
			if _, err := io.WriteString(conn, "POST / HTTP/1.1\r\nHost: example.test\r\nContent-Length: 100000\r\n\r\nhello"); err != nil {
				t.Fatal(err)
			}
			resp, err := ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("no response before deadline (server drained the body first?): %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want %d", resp.StatusCode, StatusRequestEntityTooLarge)
			}
			// Exact blocks are written verbatim, without Connection: close.
			if mode == "default" && !resp.Close {
				t.Error("response does not declare Connection: close")
			}
		})
	}
}
