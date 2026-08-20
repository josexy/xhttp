// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http2

// RequestForRetry returns a clone of req suitable for retrying after err.
// It is exported for use by the parent http package.
func RequestForRetry(req *ClientRequest, err error) (*ClientRequest, error) {
	return shouldRetryRequest(req, err)
}
