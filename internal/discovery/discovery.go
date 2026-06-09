// Package discovery advertises this agent on the local network via mDNS
// (zeroconf) and browses for peers. Discovery only yields hints — the agent
// confirms a peer and learns its full state by polling its /api/state.
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

const Service = "_potluck._tcp"
const domain = "local."

// PeerHint is a peer found via mDNS, before its state has been fetched.
type PeerHint struct {
	ID   string
	Name string
	Pool string
	Addr string // host:port
}

// Advertise registers this node in mDNS until ctx is cancelled.
func Advertise(ctx context.Context, id, name, pool string, port int, version string) error {
	txt := []string{
		"id=" + id,
		"name=" + name,
		"pool=" + pool,
		"version=" + version,
	}
	server, err := zeroconf.Register(instanceName(name, id), Service, domain, port, txt, nil)
	if err != nil {
		return fmt.Errorf("mdns register: %w", err)
	}
	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()
	return nil
}

// instanceName makes the mDNS instance unique even when two machines share a
// hostname, by appending a short ID suffix.
func instanceName(name, id string) string {
	suffix := id
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return name + "-" + suffix
}

// Browse continuously scans for peers and sends hints on the channel.
// Each scan runs for scanWindow, then restarts, so departed peers stop
// appearing and new peers are found quickly.
func Browse(ctx context.Context, selfID, pool string, hints chan<- PeerHint, log *slog.Logger) {
	const scanWindow = 10 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := browseOnce(ctx, selfID, pool, hints, scanWindow); err != nil && ctx.Err() == nil {
			log.Warn("mdns browse failed, retrying", "err", err)
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

func browseOnce(ctx context.Context, selfID, pool string, hints chan<- PeerHint, window time.Duration) error {
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return fmt.Errorf("mdns resolver: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	scanCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	if err := resolver.Browse(scanCtx, Service, domain, entries); err != nil {
		return fmt.Errorf("mdns browse: %w", err)
	}
	for entry := range entries {
		hint, ok := parseEntry(entry)
		if !ok || hint.ID == selfID || hint.Pool != pool {
			continue
		}
		select {
		case hints <- hint:
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

func parseEntry(e *zeroconf.ServiceEntry) (PeerHint, bool) {
	h := PeerHint{}
	for _, kv := range e.Text {
		k, v, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		switch k {
		case "id":
			h.ID = v
		case "name":
			h.Name = v
		case "pool":
			h.Pool = v
		}
	}
	if h.ID == "" {
		return h, false
	}
	// Prefer IPv4; fall back to IPv6.
	if len(e.AddrIPv4) > 0 {
		h.Addr = fmt.Sprintf("%s:%d", e.AddrIPv4[0], e.Port)
	} else if len(e.AddrIPv6) > 0 {
		h.Addr = fmt.Sprintf("[%s]:%d", e.AddrIPv6[0], e.Port)
	} else {
		return h, false
	}
	return h, true
}
