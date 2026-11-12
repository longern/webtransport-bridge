package main

import (
	"net/http"
	"net/netip"
	"sync"
)

// Use the QUIC peer address. IPv6 clients can have many addresses within
// the same network, so group them by /64 and normalize mapped IPv4.
func sourceNetwork(remoteAddr string) (netip.Prefix, error) {
	peer, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Prefix{}, err
	}
	ip := peer.Addr().Unmap().WithZone("")
	if ip.Is4() {
		return netip.PrefixFrom(ip, 32), nil
	}
	return netip.PrefixFrom(ip, 64).Masked(), nil
}

type concurrencyLimit struct {
	mu             sync.Mutex
	total          int
	bySource       map[netip.Prefix]int
	max, perSource int
}

func newConcurrencyLimit(max, perSource int) *concurrencyLimit {
	return &concurrencyLimit{max: max, perSource: perSource, bySource: make(map[netip.Prefix]int)}
}

func (l *concurrencyLimit) acquire(source netip.Prefix) (release func(), status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bySource[source] >= l.perSource {
		return nil, http.StatusTooManyRequests
	}
	if l.total >= l.max {
		return nil, http.StatusServiceUnavailable
	}
	l.total++
	l.bySource[source]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		l.bySource[source]--
		if l.bySource[source] == 0 {
			delete(l.bySource, source)
		}
	}, 0
}
