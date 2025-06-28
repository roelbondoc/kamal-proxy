package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type UDPServer struct {
	config       *Config
	listener     net.PacketConn
	serviceMap   *UDPServiceMap
	running      bool
	shutdown     chan struct{}
	wg           sync.WaitGroup
	mu           sync.RWMutex
}

type UDPPacket struct {
	Data []byte
	Addr net.Addr
	Port int
}

func NewUDPServer(config *Config, serviceMap *UDPServiceMap) *UDPServer {
	return &UDPServer{
		config:     config,
		serviceMap: serviceMap,
		shutdown:   make(chan struct{}),
	}
}

func (s *UDPServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("UDP server already running")
	}

	addr := fmt.Sprintf("%s:%d", s.config.Bind, s.config.UdpPort)
	listener, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to start UDP listener on %s: %w", addr, err)
	}

	s.listener = listener
	s.running = true

	slog.Info("UDP server started", "addr", addr)

	s.wg.Add(1)
	go s.handlePackets()

	return nil
}

func (s *UDPServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	close(s.shutdown)
	s.running = false

	if s.listener != nil {
		s.listener.Close()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("UDP server stopped gracefully")
		return nil
	case <-ctx.Done():
		slog.Warn("UDP server shutdown timeout")
		return ctx.Err()
	}
}

func (s *UDPServer) handlePackets() {
	defer s.wg.Done()

	buffer := make([]byte, 65536) // Max UDP packet size

	for {
		select {
		case <-s.shutdown:
			return
		default:
			s.listener.SetReadDeadline(time.Now().Add(1 * time.Second))
			n, addr, err := s.listener.ReadFrom(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue
				}
				if s.isShuttingDown() {
					return
				}
				slog.Error("Error reading UDP packet", "error", err)
				continue
			}

			packet := UDPPacket{
				Data: make([]byte, n),
				Addr: addr,
				Port: s.config.UdpPort,
			}
			copy(packet.Data, buffer[:n])

			go s.routePacket(packet)
		}
	}
}

func (s *UDPServer) routePacket(packet UDPPacket) {
	service := s.serviceMap.ServiceForPort(packet.Port)
	if service == nil {
		slog.Debug("No service found for UDP port", "port", packet.Port, "addr", packet.Addr)
		return
	}

	sessionManager := s.serviceMap.GetSessionManager()
	err := service.ServeUDP(s.listener, packet.Addr, packet.Data, sessionManager)
	if err != nil {
		slog.Error("Error serving UDP packet", "error", err, "service", service.Name(), "addr", packet.Addr)
	}
}

func (s *UDPServer) isShuttingDown() bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

func (s *UDPServer) Port() int {
	return s.config.UdpPort
}