// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build nethttpomithttp2

package http

import "errors"

// HTTP2RequestForRetry returns nil and err when HTTP/2 support is omitted.
func HTTP2RequestForRetry(req *Request, err error) (*Request, error) {
	if req == nil {
		return nil, errors.New("http: nil Request")
	}
	if err == nil {
		return nil, errors.New("http2: nil RoundTrip error")
	}
	return nil, err
}
