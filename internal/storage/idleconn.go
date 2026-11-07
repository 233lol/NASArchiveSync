package storage

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// IdleTimeoutConn applies a read or write deadline before every operation. A
// transfer that stops making progress for the configured duration fails
// instead of blocking forever. The timeout can be changed after the handshake
// so connection setup keeps its own shorter limit.
type IdleTimeoutConn struct {
	net.Conn
	timeout atomic.Int64
}

// NewIdleTimeoutConn wraps conn. A timeout <= 0 disables the deadlines.
func NewIdleTimeoutConn(conn net.Conn, timeout time.Duration) *IdleTimeoutConn {
	c := &IdleTimeoutConn{Conn: conn}
	c.SetIdleTimeout(timeout)
	return c
}

// SetIdleTimeout changes the inactivity limit for future reads and writes.
func (c *IdleTimeoutConn) SetIdleTimeout(timeout time.Duration) {
	if timeout < 0 {
		timeout = 0
	}
	c.timeout.Store(int64(timeout))
}

func (c *IdleTimeoutConn) Read(p []byte) (int, error) {
	if timeout := time.Duration(c.timeout.Load()); timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(timeout))
	}
	return c.Conn.Read(p)
}

func (c *IdleTimeoutConn) Write(p []byte) (int, error) {
	if timeout := time.Duration(c.timeout.Load()); timeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	return c.Conn.Write(p)
}

// WrapHTTPTransportIdleTimeout makes an HTTP transport apply the idle timeout
// to every dialed connection, covering stalled request bodies and responses.
func WrapHTTPTransportIdleTimeout(tr *http.Transport, timeout time.Duration) {
	if tr == nil || timeout <= 0 {
		return
	}
	dial := tr.DialContext
	if dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return NewIdleTimeoutConn(conn, timeout), nil
	}
}
