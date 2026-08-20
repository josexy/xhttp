// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http_test

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/josexy/xhttp"
	"golang.org/x/net/http2"
)

func TestHTTP2RequestForRetry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	releaseServer := make(chan struct{})
	defer close(releaseServer)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		serverErr <- refuseFirstHTTP2Request(conn)
		<-releaseServer
	}()

	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: protocols}
	clientConn, err := transport.NewClientConn(t.Context(), "http", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_, roundTripErr := clientConn.RoundTrip(req)
	if roundTripErr == nil {
		t.Fatal("RoundTrip succeeded; want REFUSED_STREAM")
	}
	retryReq, err := http.HTTP2RequestForRetry(req, roundTripErr)
	if err != nil {
		t.Fatal(err)
	}
	if retryReq == req {
		t.Fatal("HTTP2RequestForRetry returned the original request")
	}
	body, err := io.ReadAll(retryReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = retryReq.Body.Close()
	if got, want := string(body), "payload"; got != want {
		t.Fatalf("retry body = %q; want %q", got, want)
	}

	nonReplayable := new(http.Request)
	*nonReplayable = *req
	nonReplayable.Body = io.NopCloser(strings.NewReader("payload"))
	nonReplayable.GetBody = nil
	if retryReq, err := http.HTTP2RequestForRetry(nonReplayable, roundTripErr); retryReq != nil || err == nil {
		t.Fatalf("non-replayable retry = %#v, %v; want nil request and error", retryReq, err)
	}
	permanentErr := errors.New("permanent error")
	if retryReq, err := http.HTTP2RequestForRetry(req, permanentErr); retryReq != nil || !errors.Is(err, permanentErr) {
		t.Fatalf("permanent error retry = %#v, %v; want original error", retryReq, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func refuseFirstHTTP2Request(conn net.Conn) error {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil {
		return err
	}
	if string(preface) != http2.ClientPreface {
		return fmt.Errorf("client preface = %q; want HTTP/2 preface", preface)
	}
	framer := http2.NewFramer(conn, conn)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return err
		}
		if _, ok := frame.(*http2.SettingsFrame); ok {
			break
		}
	}
	if err := framer.WriteSettings(); err != nil {
		return err
	}
	if err := framer.WriteSettingsAck(); err != nil {
		return err
	}
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return err
		}
		if headers, ok := frame.(*http2.HeadersFrame); ok {
			return framer.WriteRSTStream(headers.StreamID, http2.ErrCodeRefusedStream)
		}
	}
}
