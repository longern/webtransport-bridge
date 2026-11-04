package bridge

import (
	"context"
	"io"
	"sync"
	"time"
)

// exchangeWatch cancels upstream I/O and unblocks stream I/O when an exchange
// stalls, its absolute header/total deadline expires, or its client disconnects.
// Activity is counted only when bytes cross the client stream, not QUIC pings.
type exchangeWatch struct {
	stream                    Stream
	cancel                    context.CancelFunc
	idle                      time.Duration
	mu                        sync.Mutex
	last, headerEnd, totalEnd time.Time
	changed                   chan struct{}
	done                      chan struct{}
	exited                    chan struct{}
}

func watchExchange(parent context.Context, s Stream, idle, header, total time.Duration) (*exchangeWatch, context.Context) {
	ctx, cancel := context.WithCancel(parent)
	now := time.Now()
	w := &exchangeWatch{stream: s, cancel: cancel, idle: idle, last: now, changed: make(chan struct{}, 1), done: make(chan struct{}), exited: make(chan struct{})}
	if header > 0 {
		w.headerEnd = now.Add(header)
	}
	if total > 0 {
		w.totalEnd = now.Add(total)
	}
	go w.run(ctx)
	return w, ctx
}
func (w *exchangeWatch) deadline() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	var end time.Time
	if w.idle > 0 {
		end = w.last.Add(w.idle)
	}
	for _, d := range []time.Time{w.headerEnd, w.totalEnd} {
		if !d.IsZero() && (end.IsZero() || d.Before(end)) {
			end = d
		}
	}
	return end
}
func (w *exchangeWatch) run(ctx context.Context) {
	defer close(w.exited)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		deadline := w.deadline()
		var tick <-chan time.Time
		if !deadline.IsZero() {
			timer.Reset(time.Until(deadline))
			tick = timer.C
		}
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			_ = w.stream.SetDeadline(time.Now())
			return
		case <-w.changed:
			continue
		case <-tick:
			// Recheck: a successful read/write may have moved the idle deadline.
			if deadline = w.deadline(); !deadline.IsZero() && !time.Now().Before(deadline) {
				w.cancel()
				_ = w.stream.SetDeadline(time.Now())
				return
			}
		}
	}
}
func (w *exchangeWatch) progress() { w.mu.Lock(); w.last = time.Now(); w.mu.Unlock() }
func (w *exchangeWatch) headersDone() {
	w.mu.Lock()
	w.headerEnd = time.Time{}
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
}
func (w *exchangeWatch) stop() { close(w.done); w.cancel(); <-w.exited }
func (w *exchangeWatch) Read(p []byte) (int, error) {
	n, e := w.stream.Read(p)
	if n > 0 {
		w.progress()
	}
	return n, e
}
func (w *exchangeWatch) Write(p []byte) (int, error) {
	n, e := w.stream.Write(p)
	if n > 0 {
		w.progress()
	}
	return n, e
}

var _ io.ReadWriter = (*exchangeWatch)(nil)
