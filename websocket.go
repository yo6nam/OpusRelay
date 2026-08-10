package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const clientQueueDepth = 200
const controlQueueDepth = 20

type wsMessage struct {
	data   []byte
	isText bool
}

type wsConn struct {
	conn         net.Conn
	mu           sync.Mutex
	closed       bool
	sendQueue    chan wsMessage // binary audio frames
	controlQueue chan wsMessage // text control frames — always drained first
	done         chan struct{}
	logger       *log.Logger
	pingSentAt   atomic.Int64 // unix nano; 0 means no ping outstanding
}

func upgradeWS(w http.ResponseWriter, r *http.Request, logger *log.Logger) (*wsConn, error) {
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		return nil, fmt.Errorf("not a websocket request")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijacking not supported")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	buf.WriteString(resp)
	if err := buf.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	c := &wsConn{
		conn:         conn,
		sendQueue:    make(chan wsMessage, clientQueueDepth),
		controlQueue: make(chan wsMessage, controlQueueDepth),
		done:         make(chan struct{}),
		logger:       logger,
	}
	go c.writeLoop()
	go c.readLoop()
	return c, nil
}

func (c *wsConn) writeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {

		select {
		case msg, ok := <-c.controlQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
			continue
		default:
		}

		select {
		case msg, ok := <-c.controlQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
		case msg, ok := <-c.sendQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
		case <-ticker.C:
			c.pingSentAt.Store(time.Now().UnixNano())
			if err := c.writeFrame(nil, false, 0x9); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsConn) writeFrame(data []byte, isText bool, opcode ...byte) error {
	var header [10]byte
	switch {
	case len(opcode) > 0:
		header[0] = 0x80 | opcode[0] // FIN + explicit opcode (e.g. 0x9 = ping)
	case isText:
		header[0] = 0x81
	default:
		header[0] = 0x82
	}
	n := len(data)
	var hLen int
	switch {
	case n <= 125:
		header[1] = byte(n)
		hLen = 2
	case n <= 65535:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(n))
		hLen = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
		hLen = 10
	}
	frame := make([]byte, hLen+n)
	copy(frame, header[:hLen])
	copy(frame[hLen:], data)
	c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := c.conn.Write(frame)
	return err
}

func (c *wsConn) enqueue(data []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	msg := wsMessage{data: data, isText: false}
	select {
	case c.sendQueue <- msg:
		return true
	default:
		select {
		case <-c.sendQueue:
		default:
		}
		c.sendQueue <- msg
		return true
	}
}

func (c *wsConn) enqueueControl(msg string) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	m := wsMessage{data: []byte(msg), isText: true}
	select {
	case c.controlQueue <- m:
		return true
	default:
		select {
		case <-c.controlQueue:
		default:
		}
		c.controlQueue <- m
		return true
	}
}

func (c *wsConn) readLoop() {
	defer func() {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			c.conn.Close()
		}
		c.mu.Unlock()
		close(c.done)
		close(c.sendQueue)
		close(c.controlQueue)
	}()

	const idleTimeout = 90 * time.Second
	buf := bufio.NewReaderSize(c.conn, 4096)
	for {
		c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		b0, err := buf.ReadByte()
		if err != nil {
			return
		}
		b1, err := buf.ReadByte()
		if err != nil {
			return
		}
		opcode := b0 & 0x0F
		masked := (b1 & 0x80) != 0
		payloadLen := int(b1 & 0x7F)

		switch payloadLen {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(buf, ext[:]); err != nil {
				return
			}
			payloadLen = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(buf, ext[:]); err != nil {
				return
			}
			payloadLen = int(binary.BigEndian.Uint64(ext[:]))
		}

		if payloadLen > 65536 {
			return
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(buf, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(buf, payload); err != nil {
				return
			}
			if masked {
				for i := range payload {
					payload[i] ^= mask[i%4]
				}
			}
		}

		switch opcode {
		case 0x8:
			closeFrame := []byte{0x88, 0x00}
			c.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			c.conn.Write(closeFrame)
			return
		case 0x9:
			pong := append([]byte{0x8A, byte(len(payload))}, payload...)
			c.enqueue(pong)
		case 0xA:
			sentAt := c.pingSentAt.Swap(0)
			if sentAt != 0 {
				rtt := time.Duration(time.Now().UnixNano() - sentAt)
				rttMs := float64(rtt) / float64(time.Millisecond)
				if c.logger != nil {
					c.logger.Printf("Latency to %s: %.1fms", c.conn.RemoteAddr(), rttMs)
				}
				c.enqueueControl(fmt.Sprintf(`{"type":"latency","rtt_ms":%.1f}`, rttMs))
			}
		}
	}
}

func (c *wsConn) Wait() {
	<-c.done
}

func (c *wsConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
	}
}
