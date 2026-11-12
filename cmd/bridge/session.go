package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/longern/webtransport-bridge/internal/bridge"
	"github.com/quic-go/webtransport-go"
)

type sessionHandler struct {
	server      *webtransport.Server
	bridge      *bridge.Bridge
	idleTimeout time.Duration
	sessions    *concurrencyLimit
	exchanges   *concurrencyLimit
}

func (h *sessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/webtransport" || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodConnect {
		w.Header().Set("Allow", http.MethodConnect)
		http.Error(w, "expected WebTransport CONNECT", http.StatusMethodNotAllowed)
		return
	}
	if !h.server.CheckOrigin(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	source, err := sourceNetwork(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid peer address", http.StatusBadRequest)
		return
	}
	releaseSession, status := h.sessions.acquire(source)
	if status != 0 {
		http.Error(w, "session limit", status)
		return
	}
	defer releaseSession()
	session, err := h.server.Upgrade(w, r)
	if err != nil {
		return
	}
	defer session.CloseWithError(0, "")
	idle := newSessionIdle(session.Context(), h.idleTimeout)
	defer idle.stop()
	for idle.ctx.Err() == nil {
		stream, err := session.AcceptStream(idle.ctx)
		if err != nil {
			return
		}
		releaseExchange, status := h.exchanges.acquire(source)
		if status != 0 {
			stream.CancelRead(1)
			stream.CancelWrite(1)
			continue
		}
		if !idle.begin() {
			releaseExchange()
			stream.CancelRead(1)
			stream.CancelWrite(1)
			return
		}
		go func() {
			defer idle.finish()
			defer releaseExchange()
			defer stream.CancelRead(0)
			defer stream.Close()
			if err := h.bridge.Handle(stream.Context(), stream); err != nil {
				stream.CancelWrite(1)
				log.Printf("exchange failed: %v", err)
			}
		}()
	}
}

// A session is idle before its first exchange and after its last exchange
// ends. Transport pings and rejected streams never update its deadline.
type sessionIdle struct {
	ctx      context.Context
	cancel   context.CancelFunc
	timeout  time.Duration
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	active   int
}

func newSessionIdle(ctx context.Context, timeout time.Duration) *sessionIdle {
	ctx, cancel := context.WithCancel(ctx)
	idle := &sessionIdle{ctx: ctx, cancel: cancel, timeout: timeout}
	idle.mu.Lock()
	idle.deadline = time.Now().Add(timeout)
	idle.timer = time.AfterFunc(timeout, idle.expire)
	idle.mu.Unlock()
	return idle
}

func (i *sessionIdle) expire() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.active != 0 || i.ctx.Err() != nil {
		return
	}
	// A callback from a prior idle interval may race with an exchange.
	if remaining := time.Until(i.deadline); remaining > 0 {
		i.timer.Reset(remaining)
		return
	}
	i.cancel()
}

func (i *sessionIdle) begin() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.ctx.Err() != nil || (i.active == 0 && !time.Now().Before(i.deadline)) {
		return false
	}
	if i.active == 0 {
		i.timer.Stop()
	}
	i.active++
	return true
}

func (i *sessionIdle) finish() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.active--
	if i.active == 0 && i.ctx.Err() == nil {
		i.deadline = time.Now().Add(i.timeout)
		i.timer.Reset(i.timeout)
	}
}

func (i *sessionIdle) stop() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cancel()
	i.timer.Stop()
}
