// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/textproto"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/josexy/xhttp/httptrace"
)

func TestHTTP1RequestHeaderOrderIncludesAutomaticFields(t *testing.T) {
	req, err := NewRequest("POST", "http://example.test/path", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Z", "z")
	req.Header.Set("X-A", "a")
	req.Header.Set("User-Agent", "ordered-agent")
	req, err = WithRequestHeaderOrder(req, HeaderOrder{Headers: []string{"x-z", "content-length", "host", "user-agent"}})
	if err != nil {
		t.Fatal(err)
	}
	var wire strings.Builder
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}
	got := headerLines(wire.String())
	want := []string{
		"X-Z: z",
		"Content-Length: 3",
		"Host: example.test",
		"User-Agent: ordered-agent",
		"X-A: a",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("header lines:\n got %q\nwant %q\nwire:\n%s", got, want, wire.String())
	}
}

func TestHTTP1RequestHeaderBlocksCaptureOrderAndTrailer(t *testing.T) {
	const wire = "POST / HTTP/1.1\r\n" +
		"hOsT: example.test\r\n" +
		"x-One: first\r\n" +
		"X-Two: second\r\n" +
		"\tcontinued\r\n" +
		"X-ONE: third\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"Trailer: X-End\r\n\r\n" +
		"1\r\nx\r\n0\r\n" +
		"x-EnD: done\r\n\r\n"
	req, err := ReadRequest(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	initial := RequestHeaderBlocks(req)
	if len(initial) != 1 {
		t.Fatalf("initial blocks = %#v", initial)
	}
	wantInitial := HeaderBlock{
		Kind:       HeaderBlockInitial,
		ProtoMajor: 1,
		Fields: []HeaderField{
			{Name: "hOsT", Value: "example.test"},
			{Name: "x-One", Value: "first"},
			{Name: "X-Two", Value: "second continued"},
			{Name: "X-ONE", Value: "third"},
			{Name: "Transfer-Encoding", Value: "chunked"},
			{Name: "Trailer", Value: "X-End"},
		},
	}
	if !reflect.DeepEqual(initial[0], wantInitial) {
		t.Fatalf("initial block:\n got %#v\nwant %#v", initial[0], wantInitial)
	}
	initial[0].Fields[0].Name = "mutated"
	if got := RequestHeaderBlocks(req)[0].Fields[0].Name; got != "hOsT" {
		t.Fatalf("snapshot mutation changed store: %q", got)
	}
	if _, err := io.ReadAll(req.Body); err != nil {
		t.Fatal(err)
	}
	blocks := RequestHeaderBlocks(req)
	if len(blocks) != 2 {
		t.Fatalf("blocks after EOF = %#v", blocks)
	}
	wantTrailer := HeaderBlock{Kind: HeaderBlockTrailer, ProtoMajor: 1, Fields: []HeaderField{{Name: "x-EnD", Value: "done"}}}
	if !reflect.DeepEqual(blocks[1], wantTrailer) {
		t.Fatalf("trailer block:\n got %#v\nwant %#v", blocks[1], wantTrailer)
	}
}

func TestHTTP1InformationalHandlerBeforeTraceAndBlocksAggregate(t *testing.T) {
	ln := newOneShotListener(t, func(conn net.Conn) {
		defer conn.Close()
		readThroughBlankLine(bufio.NewReader(conn))
		io.WriteString(conn, "HTTP/1.1 103 Early Hints\r\nMiXeD: one\r\n\r\n")
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nFiNaL: two\r\n\r\n")
	})

	var mu sync.Mutex
	var events []string
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		mu.Lock()
		events = append(events, "trace")
		mu.Unlock()
		return nil
	}}
	req, err := NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = WithInformationalResponseHandler(req, func(block HeaderBlock) error {
		mu.Lock()
		events = append(events, "handler")
		mu.Unlock()
		if block.StatusCode != 103 || block.ProtoMajor != 1 || block.Fields[0].Name != "MiXeD" {
			t.Errorf("informational block = %#v", block)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	mu.Lock()
	gotEvents := append([]string(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(gotEvents, []string{"handler", "trace"}) {
		t.Fatalf("events = %q", gotEvents)
	}
	blocks := ResponseHeaderBlocks(resp)
	if len(blocks) != 2 || blocks[0].Kind != HeaderBlockInformational || blocks[0].StatusCode != 103 || blocks[1].StatusCode != 200 {
		t.Fatalf("response blocks = %#v", blocks)
	}
}

func TestHTTP1ExactRequestWritesOnlyBlockFields(t *testing.T) {
	wireCh := make(chan string, 1)
	ln := newOneShotListener(t, func(conn net.Conn) {
		defer conn.Close()
		br := bufio.NewReader(conn)
		var wire strings.Builder
		line, _ := br.ReadString('\n')
		wire.WriteString(line)
		contentLength := 0
		for {
			line, _ = br.ReadString('\n')
			wire.WriteString(line)
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				contentLength = 3
			}
			if line == "\r\n" {
				break
			}
		}
		body := make([]byte, contentLength)
		io.ReadFull(br, body)
		wire.Write(body)
		wireCh <- wire.String()
		io.WriteString(conn, "HTTP/1.1 204 No Content\r\n\r\n")
	})

	req, err := NewRequest("POST", "http://"+ln.Addr().String()+"/exact", strings.NewReader("abc"))
	if err != nil {
		t.Fatal(err)
	}
	block := HeaderBlock{Kind: HeaderBlockInitial, ProtoMajor: 1, Fields: []HeaderField{
		{Name: "x-B", Value: "one"},
		{Name: "hOsT", Value: ln.Addr().String()},
		{Name: "x-A", Value: "two"},
		{Name: "X-B", Value: "three"},
		{Name: "Content-Length", Value: "3"},
	}}
	req, err = WithRequestHeaderBlocks(req, block, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-wireCh
	want := "POST /exact HTTP/1.1\r\n" +
		"x-B: one\r\n" +
		"hOsT: " + ln.Addr().String() + "\r\n" +
		"x-A: two\r\n" +
		"X-B: three\r\n" +
		"Content-Length: 3\r\n\r\nabc"
	if got != want {
		t.Fatalf("wire:\n got %q\nwant %q", got, want)
	}
}

func TestHTTP1ExactResponseInformationalAndDynamicTrailer(t *testing.T) {
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		if err := WriteResponseHeaderBlock(w, HeaderBlock{
			Kind: HeaderBlockInformational, ProtoMajor: 1, StatusCode: 103,
			Fields: []HeaderField{{Name: "EaRlY", Value: "yes"}},
		}); err != nil {
			t.Errorf("informational block: %v", err)
			return
		}
		if err := WriteResponseHeaderBlock(w, HeaderBlock{
			Kind: HeaderBlockInitial, ProtoMajor: 1, StatusCode: 200,
			Fields: []HeaderField{
				{Name: "x-B", Value: "one"},
				{Name: "Transfer-Encoding", Value: "chunked"},
				{Name: "Trailer", Value: "X-End"},
				{Name: "X-A", Value: "two"},
			},
		}); err != nil {
			t.Errorf("final block: %v", err)
			return
		}
		if _, err := w.Write([]byte("ok")); err != nil {
			t.Errorf("write body: %v", err)
			return
		}
		if err := SetResponseTrailerBlock(w, HeaderBlock{
			Kind: HeaderBlockTrailer, ProtoMajor: 1,
			Fields: []HeaderField{{Name: "x-EnD", Value: "done"}},
		}); err != nil {
			t.Errorf("trailer block: %v", err)
		}
	}))
	defer closeServer()

	req, _ := NewRequest("GET", url, nil)
	var info []HeaderBlock
	req, _ = WithInformationalResponseHandler(req, func(block HeaderBlock) error {
		info = append(info, cloneHeaderBlock(block))
		return nil
	})
	resp, err := DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(body) != "ok" || len(info) != 1 || info[0].Fields[0].Name != "EaRlY" {
		t.Fatalf("body=%q info=%#v", body, info)
	}
	if resp.Header.Get("Date") != "" || resp.Header.Get("Content-Type") != "" {
		t.Fatalf("exact response gained automatic fields: %#v", resp.Header)
	}
	blocks := ResponseHeaderBlocks(resp)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if got := blocks[1].Fields; !reflect.DeepEqual(got, []HeaderField{
		{Name: "x-B", Value: "one"},
		{Name: "Transfer-Encoding", Value: "chunked"},
		{Name: "Trailer", Value: "X-End"},
		{Name: "X-A", Value: "two"},
	}) {
		t.Fatalf("final fields = %#v", got)
	}
	if got := blocks[2].Fields; !reflect.DeepEqual(got, []HeaderField{{Name: "x-EnD", Value: "done"}}) {
		t.Fatalf("trailer fields = %#v", got)
	}
}

func TestHTTP1ResponseHeaderOrderIncludesAutomaticFields(t *testing.T) {
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		w.Header().Set("X-A", "a")
		w.Header().Set("X-Z", "z")
		if err := SetResponseHeaderOrder(w, HeaderOrder{Headers: []string{"x-z", "date", "content-length", "x-a"}}); err != nil {
			t.Errorf("SetResponseHeaderOrder: %v", err)
		}
		io.WriteString(w, "ok")
	}))
	defer closeServer()
	resp, err := Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	fields := ResponseHeaderBlocks(resp)[0].Fields
	if len(fields) < 4 {
		t.Fatalf("fields = %#v", fields)
	}
	wantNames := []string{"X-Z", "Date", "Content-Length", "X-A"}
	for i, want := range wantNames {
		if fields[i].Name != want {
			t.Fatalf("field names = %#v; index %d want %q", fields, i, want)
		}
	}
}

func TestHTTP1ExactValidationBeforeWriting(t *testing.T) {
	req, _ := NewRequest("GET", "http://example.test/", nil)
	if !omitBundledHTTP2 {
		var err error
		req, err = WithRequestHeaderBlocks(req, HeaderBlock{
			Kind: HeaderBlockInitial, ProtoMajor: 2,
			Fields: []HeaderField{
				{Name: ":method", Value: "GET"},
				{Name: ":scheme", Value: "http"},
				{Name: ":authority", Value: "example.test"},
				{Name: ":path", Value: "/"},
			},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var wire strings.Builder
		if err := req.Write(&wire); err == nil || wire.Len() != 0 {
			t.Fatalf("Write error=%v wire=%q; want protocol error before bytes", err, wire.String())
		}
	}

	_, err := WithRequestHeaderBlocks(req, HeaderBlock{
		Kind: HeaderBlockInitial, ProtoMajor: 1,
		Fields: []HeaderField{{Name: "Host", Value: "example.test"}, {Name: "X", Value: "y", Sensitive: true}},
	}, nil)
	if err == nil {
		t.Fatal("Sensitive HTTP/1 field unexpectedly accepted")
	}
}

func TestHTTP1RequestTrailerOrder(t *testing.T) {
	req, _ := NewRequest("POST", "http://example.test/", io.NopCloser(strings.NewReader("x")))
	req.ContentLength = -1
	req.TransferEncoding = []string{"chunked"}
	req.Trailer = Header{"X-A": {"a"}, "X-Z": {"z"}}
	req, err := WithRequestHeaderOrder(req, HeaderOrder{Trailers: []string{"x-z", "x-a"}})
	if err != nil {
		t.Fatal(err)
	}
	var wire strings.Builder
	if err := req.Write(&wire); err != nil {
		t.Fatal(err)
	}
	if got := wire.String(); !strings.Contains(got, "0\r\nX-Z: z\r\nX-A: a\r\n\r\n") {
		t.Fatalf("ordered trailer not found in wire:\n%s", got)
	}
}

func TestHTTP1ExactRequestStreamingTrailer(t *testing.T) {
	wireCh := make(chan string, 1)
	ln := newOneShotListener(t, func(conn net.Conn) {
		defer conn.Close()
		br := bufio.NewReader(conn)
		var wire strings.Builder
		for {
			line, _ := br.ReadString('\n')
			wire.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		for {
			line, _ := br.ReadString('\n')
			wire.WriteString(line)
			if line == "0\r\n" {
				for {
					line, _ = br.ReadString('\n')
					wire.WriteString(line)
					if line == "\r\n" {
						break
					}
				}
				break
			}
		}
		wireCh <- wire.String()
		io.WriteString(conn, "HTTP/1.1 204 No Content\r\n\r\n")
	})
	req, _ := NewRequest("POST", "http://"+ln.Addr().String()+"/", io.NopCloser(strings.NewReader("x")))
	req.ContentLength = -1
	req, err := WithRequestHeaderBlocks(req, HeaderBlock{
		Kind: HeaderBlockInitial, ProtoMajor: 1,
		Fields: []HeaderField{
			{Name: "Host", Value: ln.Addr().String()},
			{Name: "Transfer-Encoding", Value: "chunked"},
			{Name: "Trailer", Value: "X-A, X-Z"},
		},
	}, func() (HeaderBlock, error) {
		return HeaderBlock{Kind: HeaderBlockTrailer, ProtoMajor: 1, Fields: []HeaderField{
			{Name: "x-Z", Value: "z"},
			{Name: "X-A", Value: "a"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-wireCh
	if !strings.Contains(got, "1\r\nx\r\n0\r\nx-Z: z\r\nX-A: a\r\n\r\n") {
		t.Fatalf("exact chunked body and trailer not found:\n%s", got)
	}
}

func TestHTTP1RedirectClearsExactRequestBlock(t *testing.T) {
	blocksCh := make(chan []HeaderBlock, 2)
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		blocksCh <- RequestHeaderBlocks(r)
		if r.URL.Path == "/first" {
			w.Header().Set("Location", "/next")
			w.WriteHeader(StatusFound)
			return
		}
		w.WriteHeader(StatusNoContent)
	}))
	defer closeServer()
	req, _ := NewRequest("GET", url+"/first", nil)
	host := strings.TrimPrefix(url, "http://")
	req, err := WithRequestHeaderBlocks(req, HeaderBlock{
		Kind: HeaderBlockInitial, ProtoMajor: 1,
		Fields: []HeaderField{{Name: "Host", Value: host}, {Name: "X-Exact", Value: "first-only"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{CheckRedirect: func(next *Request, via []*Request) error {
		if config := requestHeaderBlocksToWrite(next.Context()); config.initial != nil {
			t.Error("redirect retained exact request block before CheckRedirect")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	first, second := <-blocksCh, <-blocksCh
	if got := first[0].Fields; !reflect.DeepEqual(got, []HeaderField{{Name: "Host", Value: host}, {Name: "X-Exact", Value: "first-only"}}) {
		t.Fatalf("first request fields = %#v", got)
	}
	for _, field := range second[0].Fields {
		if asciiEqualFoldForTest(field.Name, "X-Exact") {
			t.Fatalf("redirect replayed exact field: %#v", second[0])
		}
	}
}

func TestHTTP1ResponseExtensionUnwrapAndLateOrderError(t *testing.T) {
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		wrapped := http1UnwrapWriter{ResponseWriter: w}
		if err := SetResponseHeaderOrder(wrapped, HeaderOrder{Headers: []string{"x-first"}}); err != nil {
			t.Errorf("wrapped SetResponseHeaderOrder: %v", err)
		}
		wrapped.Header().Set("X-First", "yes")
		wrapped.WriteHeader(StatusNoContent)
		if err := SetResponseHeaderOrder(wrapped, HeaderOrder{Headers: []string{"date"}}); err == nil {
			t.Error("late SetResponseHeaderOrder succeeded")
		}
	}))
	defer closeServer()
	resp, err := Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestHTTP1ExactRequestFramingErrors(t *testing.T) {
	tests := []struct {
		name   string
		body   io.Reader
		fields []HeaderField
	}{
		{name: "missing host", fields: []HeaderField{{Name: "X", Value: "y"}}},
		{name: "duplicate host", fields: []HeaderField{{Name: "Host", Value: "a"}, {Name: "host", Value: "b"}}},
		{name: "content length and transfer encoding", fields: []HeaderField{{Name: "Host", Value: "example.test"}, {Name: "Content-Length", Value: "1"}, {Name: "Transfer-Encoding", Value: "chunked"}}},
		{name: "unframed body", body: strings.NewReader("x"), fields: []HeaderField{{Name: "Host", Value: "example.test"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, _ := NewRequest("POST", "http://example.test/", test.body)
			req, err := WithRequestHeaderBlocks(req, HeaderBlock{Kind: HeaderBlockInitial, ProtoMajor: 1, Fields: test.fields}, nil)
			if err != nil {
				return
			}
			var wire strings.Builder
			if err := req.Write(&wire); err == nil || wire.Len() != 0 {
				t.Fatalf("Write error=%v wire=%q; want framing error before bytes", err, wire.String())
			}
		})
	}
}

func TestHTTP1TransportHeaderOrderIncludesInjectedFields(t *testing.T) {
	blocksCh := make(chan []HeaderBlock, 1)
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		blocksCh <- RequestHeaderBlocks(r)
		w.WriteHeader(StatusNoContent)
	}))
	defer closeServer()
	req, _ := NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "transport-order")
	req, err := WithRequestHeaderOrder(req, HeaderOrder{Headers: []string{"accept-encoding", "connection", "host", "user-agent"}})
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{Transport: &Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	fields := (<-blocksCh)[0].Fields
	want := []string{"Accept-Encoding", "Connection", "Host", "User-Agent"}
	if len(fields) < len(want) {
		t.Fatalf("fields = %#v", fields)
	}
	for i, name := range want {
		if fields[i].Name != name {
			t.Fatalf("fields = %#v; index %d want %q", fields, i, name)
		}
	}
}

func TestHTTP1ResponseTrailerOrder(t *testing.T) {
	url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
		w.Header().Set("Trailer", "X-A, X-Z")
		if err := SetResponseHeaderOrder(w, HeaderOrder{Trailers: []string{"x-z", "x-a"}}); err != nil {
			t.Errorf("SetResponseHeaderOrder: %v", err)
		}
		io.WriteString(w, "ok")
		w.Header().Set("X-A", "a")
		w.Header().Set("X-Z", "z")
	}))
	defer closeServer()
	resp, err := Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	blocks := ResponseHeaderBlocks(resp)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if got := blocks[1].Fields; !reflect.DeepEqual(got, []HeaderField{{Name: "X-Z", Value: "z"}, {Name: "X-A", Value: "a"}}) {
		t.Fatalf("trailer fields = %#v", got)
	}
}

func TestHTTP1ExactResponseFramingAndTrailerErrors(t *testing.T) {
	t.Run("content length and transfer encoding", func(t *testing.T) {
		errCh := make(chan error, 1)
		url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
			errCh <- WriteResponseHeaderBlock(w, HeaderBlock{
				Kind: HeaderBlockInitial, ProtoMajor: 1, StatusCode: 200,
				Fields: []HeaderField{{Name: "Content-Length", Value: "1"}, {Name: "Transfer-Encoding", Value: "chunked"}},
			})
			w.WriteHeader(StatusNoContent)
		}))
		defer closeServer()
		resp, err := Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if err := <-errCh; err == nil {
			t.Fatal("conflicting exact response framing succeeded")
		}
	})

	t.Run("undeclared trailer", func(t *testing.T) {
		errCh := make(chan error, 1)
		url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
			if err := WriteResponseHeaderBlock(w, HeaderBlock{
				Kind: HeaderBlockInitial, ProtoMajor: 1, StatusCode: 200,
				Fields: []HeaderField{{Name: "Transfer-Encoding", Value: "chunked"}, {Name: "Trailer", Value: "X-A"}},
			}); err != nil {
				errCh <- err
				return
			}
			errCh <- SetResponseTrailerBlock(w, HeaderBlock{
				Kind: HeaderBlockTrailer, ProtoMajor: 1,
				Fields: []HeaderField{{Name: "X-B", Value: "wrong"}},
			})
		}))
		defer closeServer()
		resp, err := Get(url)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := <-errCh; err == nil {
			t.Fatal("undeclared exact trailer succeeded")
		}
	})

	t.Run("content length overflow", func(t *testing.T) {
		errCh := make(chan error, 1)
		url, closeServer := startHTTP1TestServer(t, HandlerFunc(func(w ResponseWriter, r *Request) {
			if err := WriteResponseHeaderBlock(w, HeaderBlock{
				Kind: HeaderBlockInitial, ProtoMajor: 1, StatusCode: 200,
				Fields: []HeaderField{{Name: "Content-Length", Value: "1"}},
			}); err != nil {
				errCh <- err
				return
			}
			_, err := w.Write([]byte("too long"))
			errCh <- err
		}))
		defer closeServer()
		resp, err := Get(url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if err := <-errCh; err != ErrContentLength {
			t.Fatalf("Write error = %v; want ErrContentLength", err)
		}
	})
}

func headerLines(wire string) []string {
	head, _, _ := strings.Cut(wire, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if len(lines) <= 1 {
		return nil
	}
	return lines[1:]
}

func readThroughBlankLine(br *bufio.Reader) {
	for {
		line, err := br.ReadString('\n')
		if err != nil || line == "\r\n" {
			return
		}
	}
}

func newOneShotListener(t *testing.T, serve func(net.Conn)) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			serve(conn)
		}
	}()
	return ln
}

func startHTTP1TestServer(t *testing.T, handler Handler) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Handler: handler}
	go server.Serve(ln)
	return "http://" + ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}
}

type http1UnwrapWriter struct{ ResponseWriter }

func (w http1UnwrapWriter) Unwrap() ResponseWriter { return w.ResponseWriter }

func asciiEqualFoldForTest(a, b string) bool { return strings.EqualFold(a, b) }
