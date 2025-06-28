package server

import (
	"fmt"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"
)

type PortAllocator struct {
	config    PortConfig
	allocated map[int]*PortAllocation
	listeners map[int]net.PacketConn
	lock      sync.RWMutex
	cleanup   *time.Ticker
	shutdown  chan struct{}
}

type PortConfig struct {
	RTPPortRange    PortRange
	RTCPPortRange   PortRange
	AllocationTTL   time.Duration
	RTCPPolicy      string // "adjacent", "separate-range", "auto"
	CleanupInterval time.Duration
}

type PortRange struct {
	Start int
	End   int
	Step  int // Default 2 for RTP/RTCP pairs, 1 for single ports
}

type PortAllocation struct {
	ServiceName  string
	RTPPort      int
	RTCPPort     int
	AllocatedAt  time.Time
	ExpiresAt    time.Time
	Target       *UDPTarget
	Protocol     string // "udp", "webrtc"
	InUse        bool
}

func NewPortAllocator(config PortConfig) *PortAllocator {
	// Set defaults if not provided
	if config.AllocationTTL == 0 {
		config.AllocationTTL = time.Duration(DefaultPortAllocationTTL) * time.Second
	}
	if config.CleanupInterval == 0 {
		config.CleanupInterval = time.Duration(DefaultCleanupInterval) * time.Second
	}
	if config.RTCPPolicy == "" {
		config.RTCPPolicy = "adjacent"
	}
	if config.RTPPortRange.Step == 0 {
		config.RTPPortRange.Step = 2 // Default for RTP/RTCP pairs
	}
	if config.RTCPPortRange.Step == 0 {
		config.RTCPPortRange.Step = 1
	}

	pa := &PortAllocator{
		config:    config,
		allocated: make(map[int]*PortAllocation),
		listeners: make(map[int]net.PacketConn),
		shutdown:  make(chan struct{}),
	}

	pa.startCleanup()
	return pa
}

func (pa *PortAllocator) AllocateRTPRTCPPair(serviceName string, target *UDPTarget) (*PortAllocation, error) {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	switch pa.config.RTCPPolicy {
	case "adjacent":
		return pa.allocateAdjacentPair(serviceName, target)
	case "separate-range":
		return pa.allocateSeparateRanges(serviceName, target)
	case "auto":
		// Try adjacent first, fall back to separate ranges
		allocation, err := pa.allocateAdjacentPair(serviceName, target)
		if err != nil {
			return pa.allocateSeparateRanges(serviceName, target)
		}
		return allocation, nil
	default:
		return pa.allocateAdjacentPair(serviceName, target)
	}
}

func (pa *PortAllocator) AllocateUDPPort(serviceName string, target *UDPTarget, preferredPort int) (*PortAllocation, error) {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	// Check if preferred port is available
	if preferredPort > 0 && pa.isPortAvailable(preferredPort) {
		return pa.allocateSpecificPort(serviceName, target, preferredPort)
	}

	// Allocate from RTP range for general UDP
	rtpRange := pa.config.RTPPortRange
	for port := rtpRange.Start; port <= rtpRange.End; port++ {
		if pa.isPortAvailable(port) {
			return pa.allocateSpecificPort(serviceName, target, port)
		}
	}

	return nil, fmt.Errorf("no available UDP ports in range %d-%d", rtpRange.Start, rtpRange.End)
}

func (pa *PortAllocator) allocateAdjacentPair(serviceName string, target *UDPTarget) (*PortAllocation, error) {
	rtpRange := pa.config.RTPPortRange

	for port := rtpRange.Start; port < rtpRange.End; port += rtpRange.Step {
		rtpPort := port
		rtcpPort := port + 1

		if !pa.isPortAvailable(rtpPort) || !pa.isPortAvailable(rtcpPort) {
			continue
		}

		// Try to bind both ports
		rtpListener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", rtpPort))
		if err != nil {
			continue
		}

		rtcpListener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", rtcpPort))
		if err != nil {
			rtpListener.Close()
			continue
		}

		allocation := &PortAllocation{
			ServiceName: serviceName,
			RTPPort:     rtpPort,
			RTCPPort:    rtcpPort,
			AllocatedAt: time.Now(),
			ExpiresAt:   time.Now().Add(pa.config.AllocationTTL),
			Target:      target,
			Protocol:    "webrtc",
			InUse:       true,
		}

		pa.allocated[rtpPort] = allocation
		pa.allocated[rtcpPort] = allocation
		pa.listeners[rtpPort] = rtpListener
		pa.listeners[rtcpPort] = rtcpListener

		slog.Info("Allocated RTP/RTCP port pair", 
			"service", serviceName, 
			"rtp_port", rtpPort, 
			"rtcp_port", rtcpPort,
			"target", target.URL())

		return allocation, nil
	}

	return nil, fmt.Errorf("no available adjacent port pairs in range %d-%d", rtpRange.Start, rtpRange.End)
}

func (pa *PortAllocator) allocateSeparateRanges(serviceName string, target *UDPTarget) (*PortAllocation, error) {
	rtpRange := pa.config.RTPPortRange
	rtcpRange := pa.config.RTCPPortRange

	// Find available RTP port
	var rtpPort int
	var rtpListener net.PacketConn
	for port := rtpRange.Start; port <= rtpRange.End; port++ {
		if pa.isPortAvailable(port) {
			listener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
			if err != nil {
				continue
			}
			rtpPort = port
			rtpListener = listener
			break
		}
	}

	if rtpPort == 0 {
		return nil, fmt.Errorf("no available RTP ports in range %d-%d", rtpRange.Start, rtpRange.End)
	}

	// Find available RTCP port
	var rtcpPort int
	var rtcpListener net.PacketConn
	for port := rtcpRange.Start; port <= rtcpRange.End; port++ {
		if pa.isPortAvailable(port) {
			listener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
			if err != nil {
				continue
			}
			rtcpPort = port
			rtcpListener = listener
			break
		}
	}

	if rtcpPort == 0 {
		rtpListener.Close()
		return nil, fmt.Errorf("no available RTCP ports in range %d-%d", rtcpRange.Start, rtcpRange.End)
	}

	allocation := &PortAllocation{
		ServiceName: serviceName,
		RTPPort:     rtpPort,
		RTCPPort:    rtcpPort,
		AllocatedAt: time.Now(),
		ExpiresAt:   time.Now().Add(pa.config.AllocationTTL),
		Target:      target,
		Protocol:    "webrtc",
		InUse:       true,
	}

	pa.allocated[rtpPort] = allocation
	pa.allocated[rtcpPort] = allocation
	pa.listeners[rtpPort] = rtpListener
	pa.listeners[rtcpPort] = rtcpListener

	slog.Info("Allocated RTP/RTCP port pair from separate ranges", 
		"service", serviceName, 
		"rtp_port", rtpPort, 
		"rtcp_port", rtcpPort,
		"target", target.URL())

	return allocation, nil
}

func (pa *PortAllocator) allocateSpecificPort(serviceName string, target *UDPTarget, port int) (*PortAllocation, error) {
	listener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to bind UDP port %d: %w", port, err)
	}

	allocation := &PortAllocation{
		ServiceName: serviceName,
		RTPPort:     port,
		RTCPPort:    0, // No RTCP for single UDP port
		AllocatedAt: time.Now(),
		ExpiresAt:   time.Now().Add(pa.config.AllocationTTL),
		Target:      target,
		Protocol:    "udp",
		InUse:       true,
	}

	pa.allocated[port] = allocation
	pa.listeners[port] = listener

	slog.Info("Allocated UDP port", 
		"service", serviceName, 
		"port", port,
		"target", target.URL())

	return allocation, nil
}

func (pa *PortAllocator) isPortAvailable(port int) bool {
	// Check if already allocated
	if _, exists := pa.allocated[port]; exists {
		return false
	}

	// Check if port is actually bindable
	listener, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	listener.Close()
	return true
}

func (pa *PortAllocator) DeallocatePort(port int) error {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	allocation := pa.allocated[port]
	if allocation == nil {
		return fmt.Errorf("port %d not allocated", port)
	}

	// Close listeners
	if listener := pa.listeners[allocation.RTPPort]; listener != nil {
		listener.Close()
		delete(pa.listeners, allocation.RTPPort)
	}
	if allocation.RTCPPort > 0 {
		if listener := pa.listeners[allocation.RTCPPort]; listener != nil {
			listener.Close()
			delete(pa.listeners, allocation.RTCPPort)
		}
		delete(pa.allocated, allocation.RTCPPort)
	}

	delete(pa.allocated, allocation.RTPPort)

	slog.Info("Deallocated port allocation", 
		"service", allocation.ServiceName,
		"rtp_port", allocation.RTPPort,
		"rtcp_port", allocation.RTCPPort)

	return nil
}

func (pa *PortAllocator) DeallocateService(serviceName string) error {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	var toRemove []int
	for port, allocation := range pa.allocated {
		if allocation.ServiceName == serviceName {
			toRemove = append(toRemove, port)
		}
	}

	for _, port := range toRemove {
		allocation := pa.allocated[port]
		
		// Close listeners
		if listener := pa.listeners[allocation.RTPPort]; listener != nil {
			listener.Close()
			delete(pa.listeners, allocation.RTPPort)
		}
		if allocation.RTCPPort > 0 {
			if listener := pa.listeners[allocation.RTCPPort]; listener != nil {
				listener.Close()
				delete(pa.listeners, allocation.RTCPPort)
			}
		}
		
		delete(pa.allocated, port)
	}

	if len(toRemove) > 0 {
		slog.Info("Deallocated all ports for service", "service", serviceName, "ports", len(toRemove))
	}

	return nil
}

func (pa *PortAllocator) GetAllocation(port int) *PortAllocation {
	pa.lock.RLock()
	defer pa.lock.RUnlock()
	return pa.allocated[port]
}

func (pa *PortAllocator) GetAllocatedPorts() []int {
	pa.lock.RLock()
	defer pa.lock.RUnlock()

	ports := make([]int, 0, len(pa.allocated))
	for port := range pa.allocated {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func (pa *PortAllocator) GetServiceAllocations(serviceName string) []*PortAllocation {
	pa.lock.RLock()
	defer pa.lock.RUnlock()

	var allocations []*PortAllocation
	seen := make(map[*PortAllocation]bool)
	
	for _, allocation := range pa.allocated {
		if allocation.ServiceName == serviceName && !seen[allocation] {
			allocations = append(allocations, allocation)
			seen[allocation] = true
		}
	}
	return allocations
}

func (pa *PortAllocator) GetListener(port int) net.PacketConn {
	pa.lock.RLock()
	defer pa.lock.RUnlock()
	return pa.listeners[port]
}

func (pa *PortAllocator) ExtendAllocation(port int, duration time.Duration) error {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	allocation := pa.allocated[port]
	if allocation == nil {
		return fmt.Errorf("port %d not allocated", port)
	}

	allocation.ExpiresAt = time.Now().Add(duration)
	return nil
}

func (pa *PortAllocator) Stop() {
	close(pa.shutdown)
	
	pa.lock.Lock()
	defer pa.lock.Unlock()

	// Close all listeners
	for port, listener := range pa.listeners {
		listener.Close()
		delete(pa.listeners, port)
	}

	// Clear allocations
	pa.allocated = make(map[int]*PortAllocation)

	if pa.cleanup != nil {
		pa.cleanup.Stop()
	}

	slog.Info("Port allocator stopped")
}

func (pa *PortAllocator) startCleanup() {
	pa.cleanup = time.NewTicker(pa.config.CleanupInterval)

	go func() {
		for {
			select {
			case <-pa.cleanup.C:
				pa.cleanupExpiredAllocations()
			case <-pa.shutdown:
				return
			}
		}
	}()
}

func (pa *PortAllocator) cleanupExpiredAllocations() {
	pa.lock.Lock()
	defer pa.lock.Unlock()

	now := time.Now()
	var expired []int

	for port, allocation := range pa.allocated {
		if !allocation.InUse && now.After(allocation.ExpiresAt) {
			expired = append(expired, port)
		}
	}

	for _, port := range expired {
		allocation := pa.allocated[port]
		
		// Close listeners
		if listener := pa.listeners[allocation.RTPPort]; listener != nil {
			listener.Close()
			delete(pa.listeners, allocation.RTPPort)
		}
		if allocation.RTCPPort > 0 {
			if listener := pa.listeners[allocation.RTCPPort]; listener != nil {
				listener.Close()
				delete(pa.listeners, allocation.RTCPPort)
			}
			delete(pa.allocated, allocation.RTCPPort)
		}
		
		delete(pa.allocated, allocation.RTPPort)
	}

	if len(expired) > 0 {
		slog.Debug("Cleaned up expired port allocations", "count", len(expired))
	}
}

func (pa *PortAllocator) GetStats() map[string]interface{} {
	pa.lock.RLock()
	defer pa.lock.RUnlock()

	rtpCount := 0
	webrtcPairs := 0
	services := make(map[string]int)

	seen := make(map[*PortAllocation]bool)
	for _, allocation := range pa.allocated {
		if !seen[allocation] {
			if allocation.Protocol == "webrtc" && allocation.RTCPPort > 0 {
				webrtcPairs++
			} else {
				rtpCount++
			}
			services[allocation.ServiceName]++
			seen[allocation] = true
		}
	}

	return map[string]interface{}{
		"total_allocations": len(seen),
		"udp_ports":        rtpCount,
		"webrtc_pairs":     webrtcPairs,
		"services":         len(services),
		"ports_in_use":     len(pa.allocated),
	}
}