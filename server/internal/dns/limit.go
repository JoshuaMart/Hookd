package dns

import (
	"net"
	"sync"
)

// limitListener closes connections beyond its capacity instead of queueing
// them, so a flood cannot exhaust the process; clients retry over UDP or elsewhere.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(l net.Listener, n int) *limitListener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: c, sem: l.sem}, nil
		default:
			_ = c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once sync.Once
	sem  chan struct{}
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.sem })
	return err
}
