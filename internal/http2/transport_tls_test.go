package http2

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/josexy/xhttp/internal/testcert"
)

func TestDialTLSClosesConnectionWhenALPNIsNotHTTP2(t *testing.T) {
	cert, err := tls.X509KeyPair(testcert.LocalhostCert, testcert.LocalhostKey)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			closed <- err
			return
		}
		_ = tlsConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = bufio.NewReader(tlsConn).ReadByte()
		if err == io.EOF {
			closed <- nil
			return
		}
		closed <- err
	}()

	transport := new(Transport)
	_, err = transport.dialTLS(t.Context(), "tcp", listener.Addr().String(), &tls.Config{
		InsecureSkipVerify: true,
	})
	if err == nil {
		t.Fatal("dialTLS unexpectedly succeeded without HTTP/2 ALPN")
	}
	if err := <-closed; err != nil {
		t.Fatalf("server did not observe the failed client connection closing: %v", err)
	}
}
