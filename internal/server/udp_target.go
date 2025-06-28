package server

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type UDPTarget struct {
	url         string
	addr        *net.UDPAddr
	conn        net.PacketConn
	healthy     bool
	healthCheck *UDPHealthCheck
	mu          sync.RWMutex
}

type UDPHealthCheck struct {
	target   *UDPTarget
	interval time.Duration
	timeout  time.Duration
	stop     chan struct{}
	stopped  chan struct{}
}

func NewUDPTarget(targetURL string) (*UDPTarget, error) {
	addr, err := net.ResolveUDPAddr("udp", targetURL)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address %s: %w", targetURL, err)
	}

	target := &UDPTarget{
		url:     targetURL,
		addr:    addr,
		healthy: false,
	}

	target.healthCheck = &UDPHealthCheck{
		target:   target,
		interval: 5 * time.Second,
		timeout:  2 * time.Second,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}

	return target, nil
}

func (t *UDPTarget) Start() error {
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return fmt.Errorf("failed to create UDP connection for target %s: %w", t.url, err)
	}

	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()

	go t.healthCheck.start()

	slog.Info("UDP target started", "target", t.url)
	return nil
}

func (t *UDPTarget) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn != nil {
		t.conn.Close()
		t.conn = nil
	}

	if t.healthCheck != nil {
		close(t.healthCheck.stop)
		<-t.healthCheck.stopped
	}

	slog.Info("UDP target stopped", "target", t.url)
}

func (t *UDPTarget) ForwardPacket(clientAddr net.Addr, data []byte, responseConn net.PacketConn) error {
	t.mu.RLock()
	conn := t.conn
	healthy := t.healthy
	t.mu.RUnlock()

	if !healthy {
		return fmt.Errorf("target %s is not healthy", t.url)
	}

	if conn == nil {
		return fmt.Errorf("target %s has no connection", t.url)
	}

	_, err := conn.WriteTo(data, t.addr)
	if err != nil {
		return fmt.Errorf("failed to forward packet to %s: %w", t.url, err)
	}

	go t.handleResponse(clientAddr, responseConn)
	return nil
}

func (t *UDPTarget) handleResponse(clientAddr net.Addr, responseConn net.PacketConn) {
	t.mu.RLock()
	conn := t.conn
	t.mu.RUnlock()

	if conn == nil {
		return
	}

	buffer := make([]byte, 65536)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	
	n, _, err := conn.ReadFrom(buffer)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return
		}
		slog.Error("Error reading response from target", "target", t.url, "error", err)
		return
	}

	_, err = responseConn.WriteTo(buffer[:n], clientAddr)
	if err != nil {
		slog.Error("Error sending response to client", "client", clientAddr, "error", err)
	}
}

func (t *UDPTarget) IsHealthy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.healthy
}

func (t *UDPTarget) URL() string {
	return t.url
}

func (hc *UDPHealthCheck) start() {
	defer close(hc.stopped)

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	hc.check()

	for {
		select {
		case <-hc.stop:
			return
		case <-ticker.C:
			hc.check()
		}
	}
}

func (hc *UDPHealthCheck) check() {
	healthy := hc.performCheck()
	
	hc.target.mu.Lock()
	wasHealthy := hc.target.healthy
	hc.target.healthy = healthy
	hc.target.mu.Unlock()

	if healthy != wasHealthy {
		status := "unhealthy"
		if healthy {
			status = "healthy"
		}
		slog.Info("UDP target health changed", "target", hc.target.url, "status", status)
	}
}

func (hc *UDPHealthCheck) performCheck() bool {
	hc.target.mu.RLock()
	conn := hc.target.conn
	addr := hc.target.addr
	hc.target.mu.RUnlock()

	if conn == nil || addr == nil {
		return false
	}

	testData := []byte("health-check")
	
	conn.SetWriteDeadline(time.Now().Add(hc.timeout))
	_, err := conn.WriteTo(testData, addr)
	if err != nil {
		slog.Debug("Health check write failed", "target", hc.target.url, "error", err)
		return false
	}

	buffer := make([]byte, 1024)
	conn.SetReadDeadline(time.Now().Add(hc.timeout))
	_, _, err = conn.ReadFrom(buffer)
	if err != nil {
		slog.Debug("Health check read failed", "target", hc.target.url, "error", err)
		return false
	}

	return true
}