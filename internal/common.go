// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package internal

import "errors"

var (
	ErrAbortHandler    = errors.New("github.com/josexy/xhttp: abort Handler")
	ErrBodyNotAllowed  = errors.New("http: request method or response status code does not allow body")
	ErrRequestCanceled = errors.New("github.com/josexy/xhttp: request canceled")
	ErrSkipAltProtocol = errors.New("github.com/josexy/xhttp: skip alternate protocol")
)
