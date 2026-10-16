/**
 * @Author: LiuShuXin
 * @Description: 仅中断本测试代理，验证真实 Redis 的断连和恢复
 * @File: recovery_test.go
 * Software: Goland
 * @Date: 2026/10/14
 */
package redis

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type recoveryProxy struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	down        bool
	connections map[net.Conn]bool
	workers     sync.WaitGroup
	done        chan struct{}
}

func startRecoveryProxy(t *testing.T, target string) *recoveryProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &recoveryProxy{listener: listener, target: target, connections: map[net.Conn]bool{}, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.down {
				p.mu.Unlock()
				client.Close()
				continue
			}
			p.connections[client] = true
			p.workers.Add(1)
			p.mu.Unlock()
			go p.forward(client)
		}
	}()
	t.Cleanup(func() { listener.Close(); <-p.done; p.setDown(true); p.workers.Wait() })
	return p
}
func (p *recoveryProxy) forward(client net.Conn) {
	defer p.workers.Done()
	server, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		client.Close()
		p.mu.Lock()
		delete(p.connections, client)
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		client.Close()
		server.Close()
		p.mu.Lock()
		delete(p.connections, client)
		p.mu.Unlock()
		return
	}
	p.connections[server] = true
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { io.Copy(server, client); client.Close(); server.Close(); close(done) }()
	io.Copy(client, server)
	client.Close()
	server.Close()
	<-done
	p.mu.Lock()
	delete(p.connections, client)
	delete(p.connections, server)
	p.mu.Unlock()
}
func (p *recoveryProxy) setDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for conn := range p.connections {
			conn.Close()
		}
	}
}

func TestIntegrationDependencyRecovery(t *testing.T) {
	o := integrationOptions(t)
	if o.TLS {
		t.Skip("plain TCP proxy fixture cannot preserve arbitrary TLS hostname verification")
	}
	p := startRecoveryProxy(t, net.JoinHostPort(o.Host, strconv.Itoa(o.Port)))
	o.Host = "127.0.0.1"
	o.Port = p.listener.Addr().(*net.TCPAddr).Port
	o.HealthTimeout = 300 * time.Millisecond
	c := fixtureClient(t, o, zap.NewNop())
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.setDown(true)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("disconnection not detected")
	}
	p.setDown(false)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("client did not recover on next call: %v", err)
	}
}
