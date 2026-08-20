// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build nethttpomithttp2

package http

import (
	"context"
	"errors"
	"fmt"
	"net"
)

func init() {
	omitBundledHTTP2 = true
}

const noHTTP2 = "no bundled HTTP/2" // should never see this

func (s *Server) configureHTTP2()                               {}
func (s *Server) setHTTP2Config(conf http2ExternalServerConfig) {}
func (s *Server) serveHTTP2Conn(ctx context.Context, nc net.Conn, h Handler, sawClientPreface bool, upgradeReq *Request, settings []byte) {
}

func (t *Transport) configureHTTP2(protocols Protocols) {}
func (t *Transport) http2AddConn(ctx context.Context, scheme, authority string, nc net.Conn) (RoundTripper, error) {
	return nil, errors.ErrUnsupported
}
func (t *Transport) http2ExternalDial(ctx context.Context, cm connectMethod) (RoundTripper, error) {
	return nil, errors.ErrUnsupported
}
func (t *Transport) http2NewClientConn(ctx context.Context, nc net.Conn, internalStateHook func()) (RoundTripper, error) {
	return nil, errors.ErrUnsupported
}
func (t *Transport) http2NewClientConnFromContext(ctx context.Context) (*ClientConn, error) {
	return nil, errors.ErrUnsupported
}

type http2Server struct{}
type http2ExternalServerConfig interface {
	unimplementable()
}

type http2Transport struct{}
type http2ExternalTransportConfig interface {
	ExternalRoundTrip() bool
	RoundTrip(*Request) (*Response, error)
	Registered(*Transport)
	unimplementable()
}

func (*http2Transport) CloseIdleConnections()            {}
func (*http2Transport) IdleConnStrsForTesting() []string { return nil }

type http2RoundTripper struct{}

func (http2RoundTripper) RoundTrip(*Request) (*Response, error) { panic(noHTTP2) }

func http2WithRequestHeaderOrderBridge(req *Request, order HeaderOrder) (*Request, error) {
	return req, nil
}

func http2SetResponseHeaderOrderBridge(ResponseWriter, HeaderOrder) error {
	return fmt.Errorf("http: response header order: %w", errors.ErrUnsupported)
}

func http2RequestHeaderBlocksBridge(*Request) []HeaderBlock   { return nil }
func http2ResponseHeaderBlocksBridge(*Response) []HeaderBlock { return nil }

func http2WithRequestHeaderBlocksBridge(*Request, HeaderBlock, HeaderBlockFunc) (*Request, error) {
	return nil, fmt.Errorf("http: exact HTTP/2 request header blocks: %w", errors.ErrUnsupported)
}

func http2WithInformationalResponseHandlerBridge(req *Request, _ InformationalResponseHandler) (*Request, error) {
	return req, nil
}

func http2WriteResponseHeaderBlockBridge(ResponseWriter, HeaderBlock) error {
	return fmt.Errorf("http: exact HTTP/2 response header block: %w", errors.ErrUnsupported)
}

func http2SetResponseTrailerBlockBridge(ResponseWriter, HeaderBlock) error {
	return fmt.Errorf("http: exact HTTP/2 response trailer block: %w", errors.ErrUnsupported)
}

func http2ClearRequestHeaderBlocksBridge(req *Request) *Request { return req }
