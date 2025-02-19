// ws/ws.go
package ws

import (
    "bufio"
    "crypto/sha1"
    "encoding/base64"
    "encoding/binary"
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
    // Frame types
    TextMessage   = 1
    BinaryMessage = 2
    CloseMessage  = 8
    PingMessage   = 9
    PongMessage   = 10

    // Frame flags
    finBit = 1 << 7
    maskBit = 1 << 7

    maxFrameHeaderSize = 14
    maxControlFramePayloadSize = 125
)

type frameHeader struct {
    fin    bool
    opcode byte
    masked bool
    length uint64
}

// Conn represents a WebSocket connection
type Conn struct {
    conn net.Conn
    br   *bufio.Reader
    bw   *bufio.Writer
}

func (c *Conn) readFrameHeader() (header frameHeader, err error) {
    // Read first two bytes which contain most flags
    b := make([]byte, 2)
    if _, err := io.ReadFull(c.br, b); err != nil {
        return header, err
    }

    header.fin = b[0]&finBit != 0
    header.opcode = b[0] & 0xf
    header.masked = b[1]&maskBit != 0
    length := uint64(b[1] & 0x7f)

    // Handle extended payload lengths
    switch {
    case length < 126:
        header.length = length
    case length == 126:
        b := make([]byte, 2)
        if _, err := io.ReadFull(c.br, b); err != nil {
            return header, err
        }
        header.length = uint64(binary.BigEndian.Uint16(b))
    case length == 127:
        b := make([]byte, 8)
        if _, err := io.ReadFull(c.br, b); err != nil {
            return header, err
        }
        header.length = binary.BigEndian.Uint64(b)
    }

    return header, nil
}

func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
    header, err := c.readFrameHeader()
    if err != nil {
        return 0, nil, err
    }

    // Read payload
    payload := make([]byte, header.length)
    if _, err := io.ReadFull(c.br, payload); err != nil {
        return 0, nil, err
    }

    // Handle masking if present
    if header.masked {
        mask := make([]byte, 4)
        if _, err := io.ReadFull(c.br, mask); err != nil {
            return 0, nil, err
        }
        maskBytes(mask, payload)
    }

    // Handle control frames
    switch header.opcode {
    case CloseMessage:
        return CloseMessage, payload, nil
    case PingMessage:
        err = c.WriteMessage(PongMessage, payload)
        if err != nil {
            return 0, nil, err
        }
        // Continue reading to get the actual message
        return c.ReadMessage()
    case PongMessage:
        // Skip pong messages and continue reading
        return c.ReadMessage()
    }

    return int(header.opcode), payload, nil
}

func (c *Conn) WriteMessage(messageType int, data []byte) error {
    // Construct header
    var header []byte
    length := len(data)

    // First byte: FIN bit and opcode
    b0 := byte(messageType) | finBit

    // Second byte: MASK bit (clear) and payload length
    var b1 byte
    switch {
    case length <= 125:
        b1 = byte(length)
        header = []byte{b0, b1}
    case length <= 65535:
        b1 = 126
        header = make([]byte, 4)
        header[0] = b0
        header[1] = b1
        binary.BigEndian.PutUint16(header[2:], uint16(length))
    default:
        b1 = 127
        header = make([]byte, 10)
        header[0] = b0
        header[1] = b1
        binary.BigEndian.PutUint64(header[2:], uint64(length))
    }

    // Write frame
    if err := c.write(header); err != nil {
        return err
    }
    return c.write(data)
}

func (c *Conn) write(data []byte) error {
    if len(data) > 0 {
        _, err := c.conn.Write(data)
        return err
    }
    return nil
}

func maskBytes(mask []byte, data []byte) {
    for i := range data {
        data[i] ^= mask[i%4]
    }
}

func (c *Conn) Close() error {
    closeMessage := make([]byte, 2)
    binary.BigEndian.PutUint16(closeMessage, 1000) // Normal closure
    c.WriteMessage(CloseMessage, closeMessage)
    return c.conn.Close()
}

func (c *Conn) SetReadDeadline(t time.Time) error {
    return c.conn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
    return c.conn.SetWriteDeadline(t)
}

// IsWebSocketUpgrade checks if the request is a WebSocket upgrade request
func IsWebSocketUpgrade(r *http.Request) bool {
    upgrade := r.Header.Get("Upgrade")
    connection := r.Header.Get("Connection")
    return strings.ToLower(upgrade) == "websocket" && 
           strings.Contains(strings.ToLower(connection), "upgrade")
}

// Upgrader performs the WebSocket handshake
type Upgrader struct {
    HandshakeTimeout time.Duration
    ReadBufferSize   int
    WriteBufferSize  int
    CheckOrigin      func(r *http.Request) bool
}

func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
    if !IsWebSocketUpgrade(r) {
        return nil, errors.New("not a websocket handshake")
    }

    if u.CheckOrigin != nil && !u.CheckOrigin(r) {
        return nil, errors.New("origin not allowed")
    }

    challengeKey := r.Header.Get("Sec-WebSocket-Key")
    if challengeKey == "" {
        return nil, errors.New("missing Sec-WebSocket-Key")
    }

    h := w.Header()
    h.Set("Upgrade", "websocket")
    h.Set("Connection", "Upgrade")
    h.Set("Sec-WebSocket-Accept", computeAcceptKey(challengeKey))

    // Handle WebSocket Protocol negotiation
    if protocol := r.Header.Get("Sec-WebSocket-Protocol"); protocol != "" {
        protocols := strings.Split(protocol, ",")
        // Select first protocol
        h.Set("Sec-WebSocket-Protocol", strings.TrimSpace(protocols[0]))
    }

    w.WriteHeader(http.StatusSwitchingProtocols)

    hj, ok := w.(http.Hijacker)
    if !ok {
        return nil, errors.New("response does not support hijacking")
    }

    conn, brw, err := hj.Hijack()
    if err != nil {
        return nil, err
    }

    return &Conn{
        conn: conn,
        br:   brw.Reader,
        bw:   brw.Writer,
    }, nil
}

func computeAcceptKey(challengeKey string) string {
    h := sha1.New()
    h.Write([]byte(challengeKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
    return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Dialer contains parameters for connecting to WebSocket server
type Dialer struct {
    HandshakeTimeout time.Duration
    ReadBufferSize   int
    WriteBufferSize  int
}

// Dial connects to the WebSocket server at the specified URL
func (d Dialer) Dial(urlStr string, requestHeader http.Header) (*Conn, *http.Response, error) {
    u, err := url.Parse(urlStr)
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

    // Generate headers for handshake
    key := generateWebSocketKey()
    request := fmt.Sprintf("GET %s HTTP/1.1\r\n", u.RequestURI()) +
        fmt.Sprintf("Host: %s\r\n", u.Host) +
        "Upgrade: websocket\r\n" +
        "Connection: Upgrade\r\n" +
        fmt.Sprintf("Sec-WebSocket-Key: %s\r\n", key) +
        "Sec-WebSocket-Version: 13\r\n"

    // Add custom headers
    for k, values := range requestHeader {
        for _, v := range values {
            request += fmt.Sprintf("%s: %s\r\n", k, v)
        }
    }
    request += "\r\n"

    // Send handshake
    conn.SetDeadline(time.Now().Add(d.HandshakeTimeout))
    if _, err = conn.Write([]byte(request)); err != nil {
        conn.Close()
        return nil, nil, err
    }

    // Read response
    br := bufio.NewReader(conn)
    resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
    if err != nil {
        conn.Close()
        return nil, nil, err
    }

    if resp.StatusCode != http.StatusSwitchingProtocols {
        body, _ := io.ReadAll(resp.Body)
        conn.Close()
        return nil, resp, fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, body)
    }

    // Reset deadline
    conn.SetDeadline(time.Time{})

    return &Conn{
        conn: conn,
        br:   br,
        bw:   bufio.NewWriter(conn),
    }, resp, nil
}

func generateWebSocketKey() string {
    key := make([]byte, 16)
    for i := 0; i < len(key); i++ {
        key[i] = byte(i)
    }
    return base64.StdEncoding.EncodeToString(key)
}