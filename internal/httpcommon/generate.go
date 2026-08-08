// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package httpcommon

// The generated file is tagged go1.26 so bundle records the invocation as a
// comment instead of adding a second, unpinned go:generate directive. This
// module already requires Go 1.26 or newer.
//go:generate go run golang.org/x/tools/cmd/bundle@v0.44.0 -dst=github.com/josexy/xhttp/internal/httpcommon -pkg=httpcommon -prefix= -tags=go1.26 -o=httpcommon.go -import=net/http/httptrace=github.com/josexy/xhttp/httptrace github.com/josexy/net/internal/httpcommon
