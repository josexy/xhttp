package http_test

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	http "github.com/josexy/xhttp"
	"github.com/josexy/xhttp/httptest"
)

type finishWrapped struct{ http.ResponseWriter }

func (w finishWrapped) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func TestFinishResponseIncludesTrailersBeforeHandlerReturns(t *testing.T) {
	finished := make(chan error, 1)
	release := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-End")
		_, err := io.WriteString(w, "payload")
		if err != nil {
			finished <- err
			return
		}
		w.Header().Set("X-End", "complete")
		err = http.FinishResponse(finishWrapped{w})
		if err == nil {
			if _, writeErr := w.Write([]byte("late")); writeErr == nil {
				err = errors.New("write after finish succeeded")
			}
		}
		if next := http.FinishResponse(w); err == nil && next != nil {
			err = next
		}
		finished <- err
		<-release
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	defer close(release)
	result := make(chan error, 1)
	go func() {
		resp, err := server.Client().Get(server.URL)
		if err != nil {
			result <- err
			return
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err == nil && (string(data) != "payload" || resp.Trailer.Get("X-End") != "complete") {
			err = errors.New("missing body or final trailer")
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("END_STREAM not sent until handler return")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
func TestFinishResponseReportsCanceledStream(t *testing.T) {
	result := make(chan error, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "partial")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		result <- http.FinishResponse(w)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled stream completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("finish hung after cancellation")
	}
}
func TestFinishResponseHTTP1Unsupported(t *testing.T) {
	if err := http.FinishResponse(httptest.NewRecorder()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatal(err)
	}
}

type finishFailListener struct {
	net.Listener
	armed *atomic.Bool
}

func (l finishFailListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return c, err
	}
	return finishFailConn{Conn: c, armed: l.armed}, nil
}

type finishFailConn struct {
	net.Conn
	armed *atomic.Bool
}

func (c finishFailConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		return 0, errors.New("injected terminal failure")
	}
	return c.Conn.Write(p)
}
func TestFinishResponseTerminalWriteFailure(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "data", true: "exact_trailer"}[exact], func(t *testing.T) {
			var armed atomic.Bool
			consumed := make(chan struct{})
			finished := make(chan error, 1)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "prefix")
				_ = http.NewResponseController(w).Flush()
				<-consumed
				if exact {
					_ = http.SetResponseTrailerBlock(w, http.HeaderBlock{Kind: http.HeaderBlockTrailer, ProtoMajor: 2, Fields: []http.HeaderField{{Name: "x-end", Value: "complete"}}})
				}
				armed.Store(true)
				finished <- http.FinishResponse(w)
			}))
			server.Listener = finishFailListener{Listener: server.Listener, armed: &armed}
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			resp, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data := make([]byte, 6)
			if _, err = io.ReadFull(resp.Body, data); err != nil {
				t.Fatal(err)
			}
			close(consumed)
			select {
			case err = <-finished:
				if err == nil {
					t.Fatal("terminal failed write reported success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal write did not finish")
			}
		})
	}
}
