// Package kitports publishes local Embed Kit services on allocated loopback ports.
// Targets come from the VM's current network slot, never from a guest request.
package kitports

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/sandbox-kit-spec/v3/spec"
)

const (
	maxPorts       = 128
	maxConnections = 1024
	udpIdle        = time.Minute
)

type Endpoint struct {
	Name      string `json:"name,omitempty"`
	Container int    `json:"container"`
	Transport string `json:"transport"`
	Family    string `json:"family"`
	Address   string `json:"address"`
}

type binding struct {
	endpoint Endpoint
	target   string
	tcp      net.Listener
	udp      *net.UDPConn
}

type Publisher struct {
	mu          sync.Mutex
	closed      bool
	active      bool
	bindings    []binding
	connections map[net.Conn]struct{}
	dial        func(string, string) (net.Conn, error)
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
	registry    *registration
}

// Bind allocates all listeners transactionally without forwarding traffic.
// Activate is called only after Kit hooks and the workload are ready to publish.
func Bind(ctx context.Context, ip netip.Addr, ports []spec.Port) (_ *Publisher, retErr error) {
	if !ip.Is4() {
		return nil, errors.New("native ports need an IPv4 sandbox slot")
	}
	if len(ports) > maxPorts {
		return nil, fmt.Errorf("native ports exceed %d publications", maxPorts)
	}
	seen := map[string]bool{}
	for _, port := range ports {
		if err := e2b.ValidateNativePort(port); err != nil {
			return nil, err
		}
		key := spec.PortKey(port)
		if seen[key] {
			return nil, fmt.Errorf("duplicate native port %s", key)
		}
		seen[key] = true
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Publisher{dial: func(network, target string) (net.Conn, error) {
		d := net.Dialer{Timeout: 3 * time.Second}

		return d.DialContext(lifetime, network, target)
	}, cancel: cancel, connections: map[net.Conn]struct{}{}}
	defer func() {
		if retErr != nil {
			_ = p.Close()
		}
	}()
	for _, port := range ports {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b := binding{endpoint: Endpoint{Name: port.Name, Container: port.Container, Transport: spec.PortTransport(port), Family: spec.PortFamily(port)}, target: net.JoinHostPort(ip.String(), strconv.Itoa(port.Container))}
		var lc net.ListenConfig
		if b.endpoint.Transport == "tcp" {
			listener, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			b.tcp = listener
			b.endpoint.Address = listener.Addr().String()
		} else {
			packet, err := lc.ListenPacket(ctx, "udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			b.udp = packet.(*net.UDPConn)
			b.endpoint.Address = packet.LocalAddr().String()
		}
		p.bindings = append(p.bindings, b)
	}

	return p, nil
}

func (p *Publisher) Endpoints() []Endpoint {
	result := make([]Endpoint, 0, len(p.bindings))
	for _, b := range p.bindings {
		result = append(result, b.endpoint)
	}

	return result
}

// Activate writes a host-owned listing and starts serving. Its record is locked
// for this exact lifecycle, so a process crash cannot leave a live-looking entry.
func (p *Publisher) Activate(root, team, instance, lifecycle string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return net.ErrClosed
	}
	if p.active {
		return errors.New("native ports already published")
	}
	record, err := register(root, team, instance, lifecycle, p.Endpoints())
	if err != nil {
		return err
	}
	p.registry = record
	p.active = true
	for _, b := range p.bindings {
		p.wg.Add(1)
		if b.tcp != nil {
			go p.serveTCP(b)
		} else {
			go p.serveUDP(b)
		}
	}

	return nil
}

func (p *Publisher) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.connections) >= maxConnections {
		_ = c.Close()

		return false
	}
	p.connections[c] = struct{}{}

	return true
}

func (p *Publisher) forget(c net.Conn) {
	_ = c.Close()
	p.mu.Lock()
	delete(p.connections, c)
	p.mu.Unlock()
}

func (p *Publisher) serveTCP(b binding) {
	defer p.wg.Done()
	for {
		c, err := b.tcp.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			continue
		}
		p.wg.Go(func() {
			defer p.forget(c)
			target, err := p.dial("tcp4", b.target)
			if err != nil {
				return
			}
			if !p.track(target) {
				return
			}
			defer p.forget(target)
			// Preserve half-close: request EOF must not discard a delayed response.
			done := make(chan struct{})
			go func() { _, _ = io.Copy(target, c); _ = target.(*net.TCPConn).CloseWrite(); close(done) }()
			_, _ = io.Copy(c, target)
			_ = c.(*net.TCPConn).CloseWrite()
			<-done
		})
	}
}

type udpSession struct {
	conn net.Conn
	last time.Time
}

func (p *Publisher) serveUDP(b binding) {
	defer p.wg.Done()
	var mu sync.Mutex
	sessions := map[netip.AddrPort]*udpSession{}
	buffer := make([]byte, 65535)
	for {
		n, source, err := b.udp.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		mu.Lock()
		session := sessions[source]
		if session == nil {
			conn, err := p.dial("udp4", b.target)
			if err != nil {
				mu.Unlock()

				continue
			}
			if !p.track(conn) {
				mu.Unlock()

				continue
			}
			session = &udpSession{conn: conn, last: time.Now()}
			sessions[source] = session
			p.wg.Add(1)
			go func(source netip.AddrPort, s *udpSession) {
				defer p.wg.Done()
				defer p.forget(s.conn)
				defer func() {
					mu.Lock()
					if sessions[source] == s {
						delete(sessions, source)
					}
					mu.Unlock()
				}()
				replies := make([]byte, 65535)
				for {
					n, err := s.conn.Read(replies)
					if err != nil {
						// A deadline can race with an outbound datagram refreshing activity.
						var timeout net.Error
						if errors.As(err, &timeout) && timeout.Timeout() {
							mu.Lock()
							expired := time.Since(s.last) >= udpIdle
							mu.Unlock()
							if !expired {
								continue
							}
						}

						return
					}
					mu.Lock()
					s.last = time.Now()
					_ = s.conn.SetReadDeadline(s.last.Add(udpIdle))
					mu.Unlock()
					if _, err := b.udp.WriteToUDPAddrPort(replies[:n], source); err != nil {
						return
					}
				}
			}(source, session)
		}
		session.last = time.Now()
		_ = session.conn.SetReadDeadline(session.last.Add(udpIdle))
		_, err = session.conn.Write(buffer[:n])
		if err != nil {
			_ = session.conn.Close()
		}
		mu.Unlock()
	}
}

// Close joins all forwarding before the VM's slot can be reassigned. Both
// listeners and established sessions close, including blocked TCP copies.
func (p *Publisher) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		for _, b := range p.bindings {
			if b.tcp != nil {
				_ = b.tcp.Close()
			}
			if b.udp != nil {
				_ = b.udp.Close()
			}
		}
		for c := range p.connections {
			_ = c.Close()
		}
		p.mu.Unlock()
		p.wg.Wait()
		if p.registry != nil {
			p.closeErr = p.registry.close()
		}
	})

	return p.closeErr
}
