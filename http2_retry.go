// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http

import (
	"errors"

	"github.com/josexy/xhttp/internal/http2"
)

// HTTP2RequestForRetry returns a shallow clone of req suitable for retrying
// after an HTTP/2 ClientConn RoundTrip error.
//
// The returned request is non-nil only when err proves that the server did not
// process the request, and when the request body can safely be replayed. When
// the body must be rewound, HTTP2RequestForRetry calls req.GetBody and installs
// the returned body in the clone.
//
// If the request cannot safely be retried, HTTP2RequestForRetry returns nil and
// an error. Callers should return that error without retrying the request.
func HTTP2RequestForRetry(req *Request, roundTripErr error) (*Request, error) {
	if req == nil {
		return nil, errors.New("http: nil Request")
	}
	if roundTripErr == nil {
		return nil, errors.New("http2: nil RoundTrip error")
	}
	retryReq, err := http2.RequestForRetry(&http2.ClientRequest{
		Body:    req.Body,
		GetBody: req.GetBody,
	}, roundTripErr)
	if err != nil {
		return nil, err
	}
	clone := new(Request)
	*clone = *req
	clone.Body = retryReq.Body
	return clone, nil
}
