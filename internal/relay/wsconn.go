// Package relay implements the muxdeck relay: the tunnel client that ships
// in the daemon, and the self-hostable rendezvous server. The daemon dials
// out over a single WebSocket; a yamux session multiplexes ordinary HTTP
// connections back down it, so the relay can reverse-proxy the daemon's
// existing surface — attach WebSockets included — with no feature forks.
package relay

import (
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn adapts a *websocket.Conn to net.Conn so yamux can run over it.
// Binary messages only; a partially read message carries over between Reads.
type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
}

func newWSConn(ws *websocket.Conn) *wsConn { return &wsConn{ws: ws} }

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			mt, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage {
				continue
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error         { return c.ws.Close() }
func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }
func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
