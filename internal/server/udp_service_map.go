package server

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type UDPServiceMap struct {
	services          map[int]*UDPService
	portServices      map[string]*UDPService // serviceName -> service for port range services
	sessionManager    *SessionManager
	portAllocator     *PortAllocator
	deploymentManager *UDPDeploymentManager
	lock              sync.RWMutex
}

type UDPService struct {
	name           string
	port           int
	portRanges     []PortRange
	targets        []*UDPTarget
	currentTarget  int
	sessionConfig  SessionConfig
	serviceType    string // "udp", "webrtc"
	allocations    []*PortAllocation
	lock           sync.RWMutex
}

func NewUDPServiceMap(config *Config) *UDPServiceMap {
	sessionConfig := SessionConfig{
		IdleTimeout:         time.Duration(DefaultSessionTimeout) * time.Second,
		CleanupInterval:     time.Duration(DefaultCleanupInterval) * time.Second,
		StickinessEnabled:   true,
		StickinessStrategy:  "5-tuple",
		MaxSessions:         10000,
		PersistenceEnabled:  true,
		StorePath:           config.SessionStorePath(),
	}
	
	portConfig := PortConfig{
		RTPPortRange: PortRange{
			Start: DefaultRTPPortStart,
			End:   DefaultRTPPortEnd,
			Step:  2,
		},
		RTCPPortRange: PortRange{
			Start: DefaultRTCPPortStart,
			End:   DefaultRTCPPortEnd,
			Step:  1,
		},
		AllocationTTL:   time.Duration(DefaultPortAllocationTTL) * time.Second,
		RTCPPolicy:      "adjacent",
		CleanupInterval: time.Duration(DefaultCleanupInterval) * time.Second,
	}
	
	deploymentConfig := UDPDeploymentConfig{
		DrainTimeout:         DefaultDrainTimeout,
		HealthCheckTimeout:   DefaultHealthCheckTimeout,
		ForceKillTimeout:     time.Duration(DefaultForceKillTimeout) * time.Second,
		SessionCheckInterval: time.Duration(DefaultSessionCheckInterval) * time.Second,
	}
	
	return &UDPServiceMap{
		services:          make(map[int]*UDPService),
		portServices:      make(map[string]*UDPService),
		sessionManager:    NewSessionManager(sessionConfig),
		portAllocator:     NewPortAllocator(portConfig),
		deploymentManager: NewUDPDeploymentManager(deploymentConfig),
	}
}

func (m *UDPServiceMap) ServiceForPort(port int) *UDPService {
	m.lock.RLock()
	defer m.lock.RUnlock()
	
	// Check for specific port service
	if service := m.services[port]; service != nil {
		return service
	}
	
	// Check if port belongs to a port range service
	allocation := m.portAllocator.GetAllocation(port)
	if allocation != nil {
		return m.portServices[allocation.ServiceName]
	}
	
	return nil
}

func (m *UDPServiceMap) DeployWebRTCServiceZeroDowntime(serviceName string, targetURLs []string, portRanges []PortRange, sessionConfig *SessionConfig, drainPolicy SessionDrainPolicy) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Create new targets
	var newTargets []*UDPTarget
	for _, targetURL := range targetURLs {
		target, err := NewUDPTarget(targetURL)
		if err != nil {
			return fmt.Errorf("failed to create UDP target %s: %w", targetURL, err)
		}

		err = target.Start()
		if err != nil {
			return fmt.Errorf("failed to start UDP target %s: %w", targetURL, err)
		}

		newTargets = append(newTargets, target)
	}

	// Allocate new port pairs
	var newAllocations []*PortAllocation
	for _, target := range newTargets {
		allocation, err := m.portAllocator.AllocateRTPRTCPPair(serviceName, target)
		if err != nil {
			// Cleanup on failure
			for _, t := range newTargets {
				t.Stop()
			}
			for _, a := range newAllocations {
				m.portAllocator.DeallocatePort(a.RTPPort)
			}
			return fmt.Errorf("failed to allocate port pair for target %s: %w", target.URL(), err)
		}
		newAllocations = append(newAllocations, allocation)
	}

	// Start zero-downtime deployment
	return m.deploymentManager.DeployWebRTCService(serviceName, newTargets, newAllocations, m.sessionManager, m.portAllocator, drainPolicy)
}

func (m *UDPServiceMap) DeployServiceZeroDowntime(serviceName string, port int, targetURLs []string, sessionConfig *SessionConfig, drainPolicy SessionDrainPolicy) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Create new targets
	var newTargets []*UDPTarget
	for _, targetURL := range targetURLs {
		target, err := NewUDPTarget(targetURL)
		if err != nil {
			return fmt.Errorf("failed to create UDP target %s: %w", targetURL, err)
		}

		err = target.Start()
		if err != nil {
			return fmt.Errorf("failed to start UDP target %s: %w", targetURL, err)
		}

		newTargets = append(newTargets, target)
	}

	// Start zero-downtime deployment
	return m.deploymentManager.DeployUDPService(serviceName, newTargets, m.sessionManager, drainPolicy)
}

func (m *UDPServiceMap) DeployWebRTCService(serviceName string, targetURLs []string, portRanges []PortRange, sessionConfig *SessionConfig) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Use provided session config or default
	if sessionConfig == nil {
		sessionConfig = &SessionConfig{
			IdleTimeout:         time.Duration(DefaultSessionTimeout) * time.Second,
			CleanupInterval:     time.Duration(DefaultCleanupInterval) * time.Second,
			StickinessEnabled:   true,
			StickinessStrategy:  "5-tuple",
		}
	}

	service := &UDPService{
		name:          serviceName,
		port:          0, // No specific port for WebRTC services
		portRanges:    portRanges,
		sessionConfig: *sessionConfig,
		serviceType:   "webrtc",
	}

	// Create targets
	for _, targetURL := range targetURLs {
		target, err := NewUDPTarget(targetURL)
		if err != nil {
			return fmt.Errorf("failed to create UDP target %s: %w", targetURL, err)
		}

		err = target.Start()
		if err != nil {
			return fmt.Errorf("failed to start UDP target %s: %w", targetURL, err)
		}

		service.targets = append(service.targets, target)

		// Allocate WebRTC port pairs for each target
		allocation, err := m.portAllocator.AllocateRTPRTCPPair(serviceName, target)
		if err != nil {
			slog.Error("Failed to allocate port pair for target", "target", targetURL, "error", err)
			continue
		}
		service.allocations = append(service.allocations, allocation)
	}

	if len(service.allocations) == 0 {
		return fmt.Errorf("failed to allocate any ports for WebRTC service %s", serviceName)
	}

	// Remove existing service if it exists
	if existingService, exists := m.portServices[serviceName]; exists {
		slog.Info("Replacing existing WebRTC service", "service", serviceName)
		existingService.Stop()
		m.portAllocator.DeallocateService(serviceName)
	}

	m.portServices[serviceName] = service
	slog.Info("WebRTC service deployed", 
		"service", serviceName, 
		"targets", len(service.targets),
		"port_pairs", len(service.allocations))

	return nil
}

func (m *UDPServiceMap) DeployService(serviceName string, port int, targetURLs []string, sessionConfig *SessionConfig) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	// Use provided session config or default
	if sessionConfig == nil {
		sessionConfig = &SessionConfig{
			IdleTimeout:         time.Duration(DefaultSessionTimeout) * time.Second,
			CleanupInterval:     time.Duration(DefaultCleanupInterval) * time.Second,
			StickinessEnabled:   true,
			StickinessStrategy:  "5-tuple",
		}
	}

	service := &UDPService{
		name:          serviceName,
		port:          port,
		sessionConfig: *sessionConfig,
		serviceType:   "udp",
	}

	for _, targetURL := range targetURLs {
		target, err := NewUDPTarget(targetURL)
		if err != nil {
			return fmt.Errorf("failed to create UDP target %s: %w", targetURL, err)
		}

		err = target.Start()
		if err != nil {
			return fmt.Errorf("failed to start UDP target %s: %w", targetURL, err)
		}

		service.targets = append(service.targets, target)
	}

	if existingService, exists := m.services[port]; exists {
		slog.Info("Replacing existing UDP service", "service", serviceName, "port", port)
		existingService.Stop()
	}

	m.services[port] = service
	slog.Info("UDP service deployed", "service", serviceName, "port", port, "targets", len(service.targets))

	return nil
}

func (m *UDPServiceMap) RemoveService(port int) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	service, exists := m.services[port]
	if !exists {
		return fmt.Errorf("no UDP service found on port %d", port)
	}

	service.Stop()
	delete(m.services, port)
	
	slog.Info("UDP service removed", "service", service.name, "port", port)
	return nil
}

func (m *UDPServiceMap) ListServices() map[int]*UDPService {
	m.lock.RLock()
	defer m.lock.RUnlock()

	result := make(map[int]*UDPService)
	for port, service := range m.services {
		result[port] = service
	}
	return result
}

func (s *UDPService) ServeUDP(conn net.PacketConn, addr net.Addr, data []byte, sessionManager *SessionManager) error {
	// Use session-aware routing
	target, session, err := sessionManager.RoutePacket(addr, s.port, s.name, s.targets)
	if err != nil {
		return err
	}

	if target == nil {
		return fmt.Errorf("no healthy targets available for service %s", s.name)
	}

	slog.Debug("Routing UDP packet", 
		"service", s.name, 
		"session", session.ID, 
		"target", target.URL(),
		"client", addr.String())

	return target.ForwardPacket(addr, data, conn)
}

func (s *UDPService) selectTarget() *UDPTarget {
	s.lock.RLock()
	defer s.lock.RUnlock()

	if len(s.targets) == 0 {
		return nil
	}

	healthyTargets := make([]*UDPTarget, 0, len(s.targets))
	for _, target := range s.targets {
		if target.IsHealthy() {
			healthyTargets = append(healthyTargets, target)
		}
	}

	if len(healthyTargets) == 0 {
		slog.Warn("No healthy targets available", "service", s.name)
		return nil
	}

	target := healthyTargets[s.currentTarget%len(healthyTargets)]
	s.currentTarget++
	return target
}

func (s *UDPService) Stop() {
	s.lock.Lock()
	defer s.lock.Unlock()

	for _, target := range s.targets {
		target.Stop()
	}
	s.targets = nil
	s.allocations = nil
}

func (s *UDPService) Name() string {
	return s.name
}

func (s *UDPService) Port() int {
	return s.port
}

func (s *UDPService) TargetCount() int {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return len(s.targets)
}

func (s *UDPService) HealthyTargetCount() int {
	s.lock.RLock()
	defer s.lock.RUnlock()

	count := 0
	for _, target := range s.targets {
		if target.IsHealthy() {
			count++
		}
	}
	return count
}

func (m *UDPServiceMap) GetSessionManager() *SessionManager {
	return m.sessionManager
}

func (m *UDPServiceMap) Stop() {
	m.lock.Lock()
	defer m.lock.Unlock()
	
	// Stop all specific port services
	for _, service := range m.services {
		service.Stop()
	}
	
	// Stop all port range services
	for _, service := range m.portServices {
		service.Stop()
	}
	
	// Stop deployment manager
	if m.deploymentManager != nil {
		m.deploymentManager.Stop()
	}
	
	// Stop port allocator
	if m.portAllocator != nil {
		m.portAllocator.Stop()
	}
	
	// Stop session manager
	if m.sessionManager != nil {
		m.sessionManager.Stop()
	}
}

func (m *UDPServiceMap) GetSessionStats() map[string]interface{} {
	stats := map[string]interface{}{
		"udp_services":    len(m.services),
		"webrtc_services": len(m.portServices),
	}
	
	if m.sessionManager != nil {
		stats["active_sessions"] = m.sessionManager.GetActiveSessionCount()
	}
	
	if m.portAllocator != nil {
		portStats := m.portAllocator.GetStats()
		for k, v := range portStats {
			stats[k] = v
		}
	}
	
	if m.deploymentManager != nil {
		deploymentStats := m.deploymentManager.GetDeploymentStats()
		for k, v := range deploymentStats {
			stats[k] = v
		}
	}
	
	return stats
}

func (m *UDPServiceMap) GetPortAllocator() *PortAllocator {
	return m.portAllocator
}

func (m *UDPServiceMap) GetDeploymentManager() *UDPDeploymentManager {
	return m.deploymentManager
}

func (m *UDPServiceMap) ListAllServices() map[string]interface{} {
	m.lock.RLock()
	defer m.lock.RUnlock()
	
	result := map[string]interface{}{
		"udp_services":    map[int]string{},
		"webrtc_services": map[string][]string{},
	}
	
	// List UDP services
	udpServices := result["udp_services"].(map[int]string)
	for port, service := range m.services {
		udpServices[port] = service.name
	}
	
	// List WebRTC services
	webrtcServices := result["webrtc_services"].(map[string][]string)
	for name, service := range m.portServices {
		var ports []string
		for _, allocation := range service.allocations {
			if allocation.RTCPPort > 0 {
				ports = append(ports, fmt.Sprintf("RTP:%d,RTCP:%d", allocation.RTPPort, allocation.RTCPPort))
			} else {
				ports = append(ports, fmt.Sprintf("UDP:%d", allocation.RTPPort))
			}
		}
		webrtcServices[name] = ports
	}
	
	return result
}