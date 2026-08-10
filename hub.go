package main

import (
	"sync"
	"time"
)

type Hub struct {
	mu      sync.RWMutex
	clients map[*wsConn]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*wsConn]struct{})}
}

func (h *Hub) Add(c *wsConn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Remove(c *wsConn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) CloseAll() {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.mu.Lock()
		if !c.closed {
			closeFrame := []byte{0x88, 0x02, 0x03, 0xE8} // 1000 Normal Closure
			c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			c.conn.Write(closeFrame)
			c.closed = true
			c.conn.Close()
		}
		c.mu.Unlock()
	}
}

func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		pkt := make([]byte, len(data))
		copy(pkt, data)
		c.enqueue(pkt)
	}
}

func (h *Hub) BroadcastControl(msg string) {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.enqueueControl(msg)
	}
}
