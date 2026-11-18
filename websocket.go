package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type wireEvent struct {
	SchemaVersion int    `json:"schemaVersion"`
	InstanceID    string `json:"instanceId"`
	Seq           uint64 `json:"seq"`
	Type          string `json:"type"`
	Timestamp     string `json:"timestamp"`
	JobID         string `json:"jobId,omitempty"`
	VideoID       string `json:"videoId,omitempty"`
	Data          any    `json:"data"`
}
type wsClient struct {
	msgs   chan wireEvent
	cancel context.CancelFunc
	conn   *websocket.Conn
}
type Hub struct {
	mu       sync.Mutex
	clients  map[*wsClient]bool
	instance string
	closed   bool
}

func newHub() *Hub { return &Hub{clients: map[*wsClient]bool{}, instance: newID()} }
func (h *Hub) event(kind, job, video string, data any) wireEvent {
	return wireEvent{SchemaVersion: 1, InstanceID: h.instance, Type: kind, Timestamp: time.Now().Format(time.RFC3339Nano), JobID: job, VideoID: video, Data: data}
}
func (h *Hub) broadcast(kind, job, video string, data any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ev := h.event(kind, job, video, data)
	for c := range h.clients {
		select {
		case c.msgs <- ev:
		default:
			c.cancel()
			delete(h.clients, c)
		}
	}
}
func (h *Hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		c.cancel()
		c.conn.CloseNow()
		delete(h.clients, c)
	}
}
func (m *JobManager) serveWS(w http.ResponseWriter, r *http.Request) {
	h := m.hub
	h.mu.Lock()
	if h.closed || len(h.clients) >= 16 {
		h.mu.Unlock()
		writeError(w, &AppError{503, "CONNECTION_LIMIT", "WebSocket 連線已滿或服務關閉"})
		return
	}
	h.mu.Unlock()
	conn, e := websocket.Accept(w, r, nil)
	if e != nil {
		return
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	conn.SetReadLimit(1024)
	ctx = conn.CloseRead(ctx)
	c := &wsClient{msgs: make(chan wireEvent, 128), cancel: cancel, conn: conn}
	// State lock precedes hub lock for both registration and publishing.
	m.mu.Lock()
	h.mu.Lock()
	if h.closed || len(h.clients) >= 16 {
		h.mu.Unlock()
		m.mu.Unlock()
		return
	}
	c.msgs <- h.event("snapshot", "", "", m.snapshotLocked())
	h.clients[c] = true
	h.mu.Unlock()
	m.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.clients, c); h.mu.Unlock() }()
	tick := time.NewTicker(15 * second)
	defer tick.Stop()
	var seq uint64
	send := func(ev wireEvent) error {
		seq++
		ev.Seq = seq
		b, e := json.Marshal(ev)
		if e != nil {
			return e
		}
		wc, stop := context.WithTimeout(ctx, 5*second)
		defer stop()
		return conn.Write(wc, websocket.MessageText, b)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.msgs:
			if send(ev) != nil {
				return
			}
		case <-tick.C:
			m.mu.Lock()
			rev := m.revision
			m.mu.Unlock()
			if send(h.event("heartbeat", "", "", map[string]any{"revision": rev})) != nil {
				return
			}
			pc, stop := context.WithTimeout(ctx, 5*second)
			e := conn.Ping(pc)
			stop()
			if e != nil {
				return
			}
		}
	}
}
