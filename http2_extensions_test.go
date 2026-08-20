// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http_test

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/josexy/xhttp"
	"github.com/josexy/xhttp/httptest"
)

func TestHTTP1ExactRequestRejectedOnNegotiatedHTTP2(t *testing.T) {
	var called atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	req, err := http.NewRequest("GET", server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.WithRequestHeaderBlocks(req, http.HeaderBlock{
		Kind:       http.HeaderBlockInitial,
		ProtoMajor: 1,
		Fields:     []http.HeaderField{{Name: "Host", Value: req.URL.Host}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Client().Do(req); err == nil {
		t.Fatal("HTTP/1 exact block unexpectedly sent over HTTP/2")
	}
	if called.Load() {
		t.Fatal("server handler ran despite protocol mismatch")
	}
}

func TestHTTP2TrailerBlockWithZeroMetadataStillDispatches(t *testing.T) {
	errCh := make(chan error, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.WriteResponseHeaderBlock(w, http.HeaderBlock{
			Kind: http.HeaderBlockInitial,
			Fields: []http.HeaderField{
				{Name: ":status", Value: "200"},
				{Name: "x-initial", Value: "yes"},
			},
		}); err != nil {
			errCh <- err
			return
		}
		if _, err := w.Write([]byte("ok")); err != nil {
			errCh <- err
			return
		}
		errCh <- http.SetResponseTrailerBlock(w, http.HeaderBlock{
			Kind:   http.HeaderBlockTrailer,
			Fields: []http.HeaderField{{Name: "x-trailer", Value: "done"}},
		})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	blocks := http.ResponseHeaderBlocks(resp)
	if len(blocks) != 2 || blocks[1].Kind != http.HeaderBlockTrailer || blocks[1].ProtoMajor != 0 {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestHTTP2FingerprintExtensions(t *testing.T) {
	fingerprint := http.Fingerprint{
		Settings: []http.Setting{
			{ID: http.SettingHeaderTableSize, Val: 4096},
		},
		Priorities:        []http.FingerprintPriority{{StreamID: 3, StreamDep: 0, Weight: 201}},
		HeaderPriority:    &http.FingerprintHeaderPriority{StreamDep: 0, Exclusive: true, Weight: 101},
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
	const encoded = "1:4096|00|3:0:0:201|m,a,s,p"
	if got := fingerprint.String(); got != encoded {
		t.Fatalf("Fingerprint.String() = %q; want %q", got, encoded)
	}
	headerPriorityOnly := fingerprint
	headerPriorityOnly.Priorities = nil
	if got, want := headerPriorityOnly.String(), "1:4096|00|0|m,a,s,p"; got != want {
		t.Fatalf("HEADERS-priority-only Fingerprint.String() = %q; want %q", got, want)
	}

	parsed, err := http.ParseFingerprint(encoded)
	if err != nil {
		t.Fatalf("ParseFingerprint: %v", err)
	}
	wantParsed := fingerprint
	wantParsed.HeaderPriority = nil
	if !reflect.DeepEqual(parsed, wantParsed) {
		t.Fatalf("ParseFingerprint() = %#v; want serialized fields %#v", parsed, wantParsed)
	}
	if parsed.Hash() != fingerprint.Hash() {
		t.Fatalf("parsed Hash() = %q; want %q", parsed.Hash(), fingerprint.Hash())
	}
	if _, err := http.ParseFingerprint("1:4096|00|h:1:1:0:101|m,a,s,p"); err == nil {
		t.Fatal("ParseFingerprint unexpectedly accepted unpublished HEADERS priority extension")
	}

	req, err := http.NewRequest("GET", "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.WithRequestFingerprint(req, fingerprint)
	if err != nil {
		t.Fatalf("WithRequestFingerprint: %v", err)
	}
	stored, ok := http.RequestFingerprint(req)
	if !ok {
		t.Fatal("RequestFingerprint did not find the attached fingerprint")
	}
	if !reflect.DeepEqual(stored, fingerprint) {
		t.Fatalf("RequestFingerprint() = %#v; want %#v", stored, fingerprint)
	}

	stored.Settings[0].Val++
	stored.Priorities[0].Weight++
	stored.HeaderPriority.Weight++
	storedAgain, _ := http.RequestFingerprint(req)
	if !reflect.DeepEqual(storedAgain, fingerprint) {
		t.Fatal("RequestFingerprint returned mutable shared state")
	}
}

func TestHTTP2FingerprintTransportReplayAndPooling(t *testing.T) {
	observed := make(chan http.Fingerprint, 4)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fingerprint, ok := http.RequestFingerprint(r)
		if !ok {
			http.Error(w, "missing HTTP/2 fingerprint", http.StatusInternalServerError)
			return
		}
		observed <- fingerprint
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DisableCompression = true
	var dials atomic.Int32
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	base := http.Fingerprint{
		Settings: []http.Setting{
			{ID: http.SettingHeaderTableSize, Val: 65536},
			{ID: http.SettingMaxConcurrentStreams, Val: 1000},
			{ID: http.SettingInitialWindowSize, Val: 6291456},
			{ID: http.SettingMaxHeaderListSize, Val: 262144},
		},
		WindowUpdate: 15663105,
		Priorities: []http.FingerprintPriority{
			{StreamID: 3, StreamDep: 0, Weight: 201},
			{StreamID: 5, StreamDep: 3, Exclusive: true, Weight: 101},
		},
		HeaderPriority:    &http.FingerprintHeaderPriority{StreamDep: 0, Weight: 1},
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
	differentPseudoOrder := base
	differentPseudoOrder.PseudoHeaderOrder = []string{":scheme", ":method", ":authority", ":path"}
	differentHeaderPriority := base
	differentHeaderPriority.HeaderPriority = &http.FingerprintHeaderPriority{StreamDep: 0, Exclusive: true, Weight: 33}
	differentConnection := base
	differentConnection.Settings = append([]http.Setting(nil), base.Settings...)
	differentConnection.Settings[0].Val = 32768

	wants := []http.Fingerprint{base, differentPseudoOrder, differentHeaderPriority, differentConnection}
	for i, fingerprint := range wants {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/request-"+strconv.Itoa(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		req, err = http.WithRequestFingerprint(req, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	for i, want := range wants {
		if got := <-observed; !reflect.DeepEqual(got, want) {
			t.Fatalf("request %d fingerprint = %#v; want %#v", i, got, want)
		}
	}
	if got, want := dials.Load(), int32(2); got != want {
		t.Fatalf("dial count = %d; want %d", got, want)
	}
}

func TestHTTP2NewClientConnFingerprint(t *testing.T) {
	type observation struct {
		fingerprint http.Fingerprint
		ok          bool
	}
	observed := make(chan observation, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fingerprint, ok := http.RequestFingerprint(r)
		observed <- observation{fingerprint: fingerprint, ok: ok}
		if !ok {
			http.Error(w, "missing HTTP/2 fingerprint", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()

	fingerprint := http.Fingerprint{
		Settings: []http.Setting{
			{ID: http.SettingHeaderTableSize, Val: 65536},
			{ID: http.SettingMaxConcurrentStreams, Val: 1000},
			{ID: http.SettingInitialWindowSize, Val: 6291456},
			{ID: http.SettingMaxHeaderListSize, Val: 262144},
		},
		WindowUpdate:      15663105,
		Priorities:        []http.FingerprintPriority{{StreamID: 3, StreamDep: 0, Weight: 201}},
		HeaderPriority:    &http.FingerprintHeaderPriority{StreamDep: 0, Exclusive: true, Weight: 101},
		PseudoHeaderOrder: []string{":method", ":authority", ":scheme", ":path"},
	}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.WithRequestFingerprint(req, fingerprint)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := transport.NewClientConn(req.Context(), "https", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	resp, err := conn.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-observed
	if !got.ok {
		t.Fatal("server did not observe an HTTP/2 fingerprint")
	}
	if !reflect.DeepEqual(got.fingerprint, fingerprint) {
		t.Fatalf("server fingerprint = %#v; want %#v", got.fingerprint, fingerprint)
	}

	different := fingerprint
	different.Settings = append([]http.Setting(nil), fingerprint.Settings...)
	different.Settings[0].Val = 32768
	req, err = http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, err = http.WithRequestFingerprint(req, different)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RoundTrip(req); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("RoundTrip with different connection fingerprint error = %v; want errors.ErrUnsupported", err)
	}
}

func TestHTTP2HeaderOrderExtensions(t *testing.T) {
	type capture struct {
		blocks []http.HeaderBlock
		err    error
	}
	captured := make(chan capture, 1)

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := http.SetResponseHeaderOrder(w, http.HeaderOrder{
			Headers: []string{":status", "x-response-second", "x-response-first"},
		})
		captured <- capture{blocks: http.RequestHeaderBlocks(r), err: err}
		if err != nil {
			return
		}
		w.Header().Set("X-Response-First", "1")
		w.Header().Set("X-Response-Second", "2")
		w.WriteHeader(http.StatusNoContent)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Request-First", "1")
	req.Header.Set("X-Request-Second", "2")
	req, err = http.WithRequestHeaderOrder(req, http.HeaderOrder{
		Headers: []string{
			":method", ":authority", ":scheme", ":path",
			"x-request-second", "x-request-first",
		},
	})
	if err != nil {
		t.Fatalf("WithRequestHeaderOrder: %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("response protocol = %q; want HTTP/2", resp.Proto)
	}

	serverCapture := <-captured
	if serverCapture.err != nil {
		t.Fatalf("SetResponseHeaderOrder: %v", serverCapture.err)
	}
	assertHeaderNamePrefix(t, serverCapture.blocks, []string{
		":method", ":authority", ":scheme", ":path",
		"x-request-second", "x-request-first",
	})
	assertHeaderNamePrefix(t, http.ResponseHeaderBlocks(resp), []string{
		":status", "x-response-second", "x-response-first",
	})
}

func TestHTTP2ExactHeaderBlockExtensions(t *testing.T) {
	type result struct {
		blocks []http.HeaderBlock
		err    error
	}
	serverResult := make(chan result, 1)

	informational := http.HeaderBlock{
		Kind: http.HeaderBlockInformational,
		Fields: []http.HeaderField{
			{Name: ":status", Value: "103"},
			{Name: "link", Value: "</style.css>; rel=preload"},
		},
	}
	final := http.HeaderBlock{
		Kind: http.HeaderBlockInitial,
		Fields: []http.HeaderField{
			{Name: ":status", Value: "204"},
			{Name: "x-exact", Value: "first"},
			{Name: "x-exact", Value: "second", Sensitive: true},
		},
	}

	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blocks := http.RequestHeaderBlocks(r)
		if err := http.WriteResponseHeaderBlock(w, informational); err != nil {
			serverResult <- result{blocks: blocks, err: err}
			return
		}
		err := http.WriteResponseHeaderBlock(w, final)
		serverResult <- result{blocks: blocks, err: err}
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	req, err := http.NewRequest("GET", ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	requestBlock := http.HeaderBlock{
		Kind: http.HeaderBlockInitial,
		Fields: []http.HeaderField{
			{Name: ":method", Value: "GET"},
			{Name: ":authority", Value: req.URL.Host},
			{Name: ":scheme", Value: "https"},
			{Name: ":path", Value: "/"},
			{Name: "x-exact", Value: "first"},
			{Name: "x-exact", Value: "second", Sensitive: true},
		},
	}
	req, err = http.WithRequestHeaderBlocks(req, requestBlock, nil)
	if err != nil {
		t.Fatalf("WithRequestHeaderBlocks: %v", err)
	}

	infoBlocks := make(chan http.HeaderBlock, 1)
	req, err = http.WithInformationalResponseHandler(req, func(block http.HeaderBlock) error {
		infoBlocks <- block
		return nil
	})
	if err != nil {
		t.Fatalf("WithInformationalResponseHandler: %v", err)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	gotServer := <-serverResult
	if gotServer.err != nil {
		t.Fatalf("WriteResponseHeaderBlock: %v", gotServer.err)
	}
	if len(gotServer.blocks) == 0 {
		t.Fatal("RequestHeaderBlocks returned no blocks")
	}
	assertHeaderBlock(t, gotServer.blocks[0], requestBlock)

	select {
	case got := <-infoBlocks:
		assertHeaderBlock(t, got, informational)
	default:
		t.Fatal("informational response handler was not called")
	}

	responseBlocks := http.ResponseHeaderBlocks(resp)
	if len(responseBlocks) < 2 {
		t.Fatalf("ResponseHeaderBlocks returned %d blocks; want at least 2", len(responseBlocks))
	}
	assertHeaderBlock(t, responseBlocks[0], informational)
	assertHeaderBlock(t, responseBlocks[1], final)
}

func assertHeaderNamePrefix(t *testing.T, blocks []http.HeaderBlock, want []string) {
	t.Helper()
	if len(blocks) == 0 {
		t.Fatal("no HTTP/2 header blocks captured")
	}
	if blocks[0].Kind != http.HeaderBlockInitial {
		t.Fatalf("first block kind = %v; want HeaderBlockInitial", blocks[0].Kind)
	}
	if len(blocks[0].Fields) < len(want) {
		t.Fatalf("first block has %d fields; want at least %d", len(blocks[0].Fields), len(want))
	}
	got := make([]string, len(want))
	for i := range want {
		got[i] = blocks[0].Fields[i].Name
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("header name prefix = %v; want %v", got, want)
	}
}

func assertHeaderBlock(t *testing.T, got, want http.HeaderBlock) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("header block = %#v; want %#v", got, want)
	}
}
