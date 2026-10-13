# xhttp

A fork of Go's `net/http` with HTTP/1 and HTTP/2 wire header metadata,
header ordering, exact header blocks, HTTP/2 fingerprints, retry hooks,
and response completion support.

Requires Go 1.27.2 or later. Import `github.com/josexy/xhttp` instead of
`net/http`; the two packages' HTTP types are not interchangeable.

## Upstream baseline

The HTTP implementation and its internal HTTP/2 package are synchronized with
[Go 1.27.2](https://github.com/golang/go/tree/go1.27.2/src/net/http), including
the fixes since Go 1.27.0 and their regression tests. Fork-specific extensions
are retained. HPACK and HTTP utility packages still come from `golang.org/x/net`.

This update includes:

- HTTP/2 flow control accounting, deferred HPACK encoder settings updates,
  and unread request body credit recovery.
- HTTP/2 response header sanitization: connection-specific fields are removed;
  invalid or conflicting Content-Length fields are discarded and identical
  duplicates are collapsed. Captured header blocks still describe received traffic.
- Separate limits for declared trailers, and a default ServeContent limit of
  200 byte ranges (`GODEBUG=httpservecontentmaxranges=0` disables it).
- HTTP/1 early response body close/error handling and safe trailer discarding
  while a transport drains a closed body for connection reuse.
- No HTTP connection reuse after CONNECT, even on rejection. Successful
  CONNECT responses omit HTTP body framing fields. Exact response blocks reject
  those fields instead of silently changing the supplied block.
- `httputil.ReverseProxy` rejects CONNECT (405 by default, or its ErrorHandler).
  Applications implementing CONNECT tunnels directly are unaffected by that
  ReverseProxy restriction.
- Continuation read errors are propagated by the fork's ordered header parser.

GODEBUG non-default usage counters are maintained locally in this fork;
standard-library `runtime/metrics` counters do not report xhttp usage.

## Validation

```sh
go test ./...
go test -race ./...
go vet ./...
go mod verify
go mod tidy -diff
git diff --check
```

Review `go fix --diff ./...` before applying automatic modernizations. Keep
unrelated modernization changes separate from upstream patch synchronization.
