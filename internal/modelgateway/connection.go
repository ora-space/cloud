package modelgateway

import (
	"net"
	"sync"
	"time"
)

// Hard expiry bounds even active streams; reconnecting requires fresh DNS/address admission.
// It deliberately does not retry an interrupted model generation.
const upstreamConnectionLifetime = 5 * time.Minute

type leasedConnection struct {
	net.Conn
	mu    sync.Mutex
	timer *time.Timer
}

func leaseConnection(conn net.Conn, lifetime time.Duration) net.Conn {
	c := &leasedConnection{Conn: conn}
	c.mu.Lock()
	c.timer = time.AfterFunc(lifetime, func() { _ = c.Close() })
	c.mu.Unlock()
	return c
}

func (c *leasedConnection) Close() error {
	c.mu.Lock()
	c.timer.Stop()
	c.mu.Unlock()
	return c.Conn.Close()
}
