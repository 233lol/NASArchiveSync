package storage

import (
	"io"
	"net"
	"testing"
	"time"
)

type recordingConn struct {
	net.Conn
	readDeadlines  int
	writeDeadlines int
}

func (c *recordingConn) SetReadDeadline(time.Time) error  { c.readDeadlines++; return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { c.writeDeadlines++; return nil }
func (c *recordingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordingConn) Write(p []byte) (int, error)      { return len(p), nil }

func TestIdleTimeoutConnAppliesDeadlines(t *testing.T) {
	raw := &recordingConn{}
	conn := NewIdleTimeoutConn(raw, time.Second)

	if _, err := conn.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("Read: %v", err)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if raw.readDeadlines != 1 {
		t.Errorf("read deadlines = %d, want 1", raw.readDeadlines)
	}
	if raw.writeDeadlines != 1 {
		t.Errorf("write deadlines = %d, want 1", raw.writeDeadlines)
	}

	conn.SetIdleTimeout(0)
	_, _ = conn.Read(make([]byte, 1))
	_, _ = conn.Write([]byte("x"))
	if raw.readDeadlines != 1 {
		t.Errorf("disabled timeout set a read deadline: %d", raw.readDeadlines)
	}
	if raw.writeDeadlines != 1 {
		t.Errorf("disabled timeout set a write deadline: %d", raw.writeDeadlines)
	}
}
