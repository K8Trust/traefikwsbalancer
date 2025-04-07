// ws/ws.go
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
)

// Conn is a minimal WebSocket connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
}

// ReadMessage reads a line (ending in '\n') and returns it as a text message.
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return 0, nil, err
	}
	return TextMessage, []byte(strings.TrimSuffix(line, "\n")), nil
}

// WriteMessage writes data followed by a newline.
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	_, err := c.conn.Write(append(data, '\n'))
	return err
}

// SetReadDeadline sets the read deadline on the underlying connection.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the write deadline on the underlying connection.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// Close closes the underlying connection.
func (c *Conn) Close() error {
	return c.conn.Close()
}

// IsWebSocketUpgrade checks if a request is a WebSocket upgrade.
func IsWebSocketUpgrade(r *http.Request) bool {
	upgrade := r.Header.Get("Upgrade")
	connection := r.Header.Get("Connection")
	return strings.ToLower(upgrade) == "websocket" && strings.Contains(strings.ToLower(connection), "upgrade")
}

// Upgrader performs a minimal WebSocket handshake.
type Upgrader struct {
	HandshakeTimeout time.Duration
	ReadBufferSize   int
	WriteBufferSize  int
	// CheckOrigin is called to validate the request origin.
	CheckOrigin func(r *http.Request) bool
}

// Upgrade upgrades an HTTP connection to a WebSocket.
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
	if !IsWebSocketUpgrade(r) {
		return nil, errors.New("not a websocket handshake")
	}
	if u.CheckOrigin != nil && !u.CheckOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, errors.New("origin not allowed")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, errors.New("missing Sec-WebSocket-Key")
	}
	acceptKey := computeAcceptKey(key)
	headers := w.Header()
	headers.Set("Upgrade", "websocket")
	headers.Set("Connection", "Upgrade")
	headers.Set("Sec-WebSocket-Accept", acceptKey)
	w.WriteHeader(http.StatusSwitchingProtocols)

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response does not support hijacking")
	}
	netConn, _, err := hj.Hijack() // Discard the second return value.
	if err != nil {
		return nil, err
	}
	return &Conn{conn: netConn, br: bufio.NewReader(netConn)}, nil
}

func computeAcceptKey(key string) string {
	magic := "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New()
	h.Write([]byte(key + magic))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Dialer dials a WebSocket server.
type Dialer struct {
	HandshakeTimeout time.Duration
	ReadBufferSize   int
	WriteBufferSize  int
}

var DefaultDialer = &Dialer{
	HandshakeTimeout: 5 * time.Second,
	ReadBufferSize:   1024,
	WriteBufferSize:  1024,
}

// Dial dials the given URL with the provided headers and performs a minimal handshake.
func (d *Dialer) Dial(rawurl string, requestHeader http.Header) (*Conn, *http.Response, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	conn, err := net.DialTimeout("tcp", host, d.HandshakeTimeout)
	if err != nil {
		return nil, nil, err
	}
	key := generateKey()
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n", u.RequestURI()) +
		fmt.Sprintf("Host: %s\r\n", u.Host) +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", key) +
		"Sec-WebSocket-Version: 13\r\n"
	for k, vals := range requestHeader {
		for _, v := range vals {
			req += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	req += "\r\n"
	conn.SetDeadline(time.Now().Add(d.HandshakeTimeout))
	_, err = conn.Write([]byte(req))
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "GET"})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(resp.Body)
		conn.Close()
		return nil, resp, errors.New("handshake failed: " + string(body))
	}
	conn.SetDeadline(time.Time{})
	return &Conn{conn: conn, br: bufio.NewReader(conn)}, resp, nil
}

func generateKey() string {
	// For simplicity, use a fixed key.
	return "dGhlIHNhbXBsZSBub25jZQ=="
}