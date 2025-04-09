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
	"bytes"
)

const (
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10
)

// Conn is a minimal WebSocket connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	// The negotiated subprotocol
	Subprotocol string
}

// ReadMessage reads a message from the connection.
// For simplicity in this implementation, it treats all messages as either text or binary.
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		return 0, nil, err
	}
	
	// Basic frame type detection - in a real implementation this would parse the WebSocket frame format
	if len(line) > 0 && line[0] == 0x02 {
		// Simple marker for binary data in our simplified implementation
		return BinaryMessage, bytes.TrimSuffix(line[1:], []byte{'\n'}), nil
	}
	
	// Check for close frame (simplified)
	if len(line) > 0 && line[0] == 0x08 {
		return CloseMessage, nil, nil
	}
	
	// Detect ping/pong (simplified)
	if len(line) > 0 && line[0] == 0x09 {
		return PingMessage, bytes.TrimSuffix(line[1:], []byte{'\n'}), nil
	}
	if len(line) > 0 && line[0] == 0x0A {
		return PongMessage, bytes.TrimSuffix(line[1:], []byte{'\n'}), nil
	}
	
	// Default to text message
	return TextMessage, bytes.TrimSuffix(line, []byte{'\n'}), nil
}

// WriteMessage writes a message to the connection.
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	var msgData []byte
	
	switch messageType {
	case BinaryMessage:
		// Add a simple marker for binary data in our simplified implementation
		msgData = append([]byte{0x02}, data...)
	case CloseMessage:
		// Simple close frame
		msgData = append([]byte{0x08}, data...)
	case PingMessage:
		msgData = append([]byte{0x09}, data...)
	case PongMessage:
		msgData = append([]byte{0x0A}, data...)
	default: // TextMessage or unknown
		msgData = data
	}
	
	// Ensure message ends with newline for our simplified protocol
	if !bytes.HasSuffix(msgData, []byte{'\n'}) {
		msgData = append(msgData, '\n')
	}
	
	_, err := c.conn.Write(msgData)
	return err
}

// WriteControl writes a control message with the given deadline.
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if messageType != CloseMessage && messageType != PingMessage && messageType != PongMessage {
		return errors.New("invalid control message type")
	}
	
	err := c.conn.SetWriteDeadline(deadline)
	if err != nil {
		return err
	}
	
	return c.WriteMessage(messageType, data)
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
	// Subprotocols specifies the server's supported protocols in order of preference.
	// If this field is not nil and the client requested subprotocols, then the
	// first match will be chosen.
	Subprotocols []string
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
	
	// Handle subprotocols if specified
	if len(u.Subprotocols) > 0 {
		clientProtocols := r.Header.Get("Sec-WebSocket-Protocol")
		if clientProtocols != "" {
			clientProtos := strings.Split(clientProtocols, ", ")
			for _, serverProto := range u.Subprotocols {
				for _, clientProto := range clientProtos {
					if clientProto == serverProto {
						responseHeader.Set("Sec-WebSocket-Protocol", clientProto)
						break
					}
				}
				// If we found a match, break
				if responseHeader.Get("Sec-WebSocket-Protocol") != "" {
					break
				}
			}
		}
	}
	
	acceptKey := computeAcceptKey(key)
	headers := w.Header()
	headers.Set("Upgrade", "websocket")
	headers.Set("Connection", "Upgrade")
	headers.Set("Sec-WebSocket-Accept", acceptKey)
	
	// Copy any additional headers provided by the application
	for k, vs := range responseHeader {
		if k == "Sec-WebSocket-Protocol" {
			headers.Set(k, vs[0])
		} else {
			for _, v := range vs {
				headers.Add(k, v)
			}
		}
	}
	
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
	// Subprotocols specifies the client's requested subprotocols.
	Subprotocols []string
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
		
	// Add subprotocols if specified
	if len(d.Subprotocols) > 0 {
		req += fmt.Sprintf("Sec-WebSocket-Protocol: %s\r\n", strings.Join(d.Subprotocols, ", "))
	}
	
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
