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
		alreadyClosed := c.closed
		c.closed = true
		c.mu.Unlock()
		if alreadyClosed {
			continue
		}

		// The physical write goes through connMu, the same lock writeLoop
		// uses for its own conn.Write calls, so this can't interleave with
		// a write still in flight on another goroutine (see websocket.go).
		closeFrame := []byte{0x88, 0x02, 0x03, 0xE8} // 1000 Normal Closure
		c.connMu.Lock()
		c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		c.conn.Write(closeFrame)
		c.connMu.Unlock()

		c.conn.Close()
	}
}

// Broadcast sends the same audio packet to every connected client.
// The underlying byte slice is shared (not copied) across all clients:
// writeFrame only ever reads from it to build a new frame buffer, it
// never mutates it in place, so sharing the reference is safe and avoids
// one extra heap allocation + copy per client per packet (previously
// clientCount allocations every ~20ms at streaming cadence).
func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.enqueue(data)
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
