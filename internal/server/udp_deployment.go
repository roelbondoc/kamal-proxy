package server

import (
	"log/slog"
	"sync"
	"time"
)

type UDPDeploymentManager struct {
	services map[string]*UDPServiceDeployment
	lock     sync.RWMutex
	config   UDPDeploymentConfig
}

type UDPDeploymentConfig struct {
	DrainTimeout         time.Duration
	HealthCheckTimeout   time.Duration
	ForceKillTimeout     time.Duration
	SessionCheckInterval time.Duration
}

type UDPServiceDeployment struct {
	ServiceName     string
	ServiceType     string // "udp" or "webrtc"
	CurrentTargets  []*UDPTarget
	NewTargets      []*UDPTarget
	State           DeploymentState
	StartedAt       time.Time
	DrainStartedAt  time.Time
	SessionManager  *SessionManager
	PortAllocator   *PortAllocator
	
	// For WebRTC services
	CurrentAllocations []*PortAllocation
	NewAllocations     []*PortAllocation
	
	// Deployment configuration
	DrainPolicy SessionDrainPolicy
}

type DeploymentState int

const (
	DeploymentStateIdle DeploymentState = iota
	DeploymentStateHealthChecking
	DeploymentStateRouting
	DeploymentStateDraining
	DeploymentStateCompleted
	DeploymentStateFailed
)

func (ds DeploymentState) String() string {
	switch ds {
	case DeploymentStateIdle:
		return "idle"
	case DeploymentStateHealthChecking:
		return "health_checking"
	case DeploymentStateRouting:
		return "routing"
	case DeploymentStateDraining:
		return "draining"
	case DeploymentStateCompleted:
		return "completed"
	case DeploymentStateFailed:
		return "failed"
	default:
		return "unknown"
	}
}


func NewUDPDeploymentManager(config UDPDeploymentConfig) *UDPDeploymentManager {
	// Set defaults if not provided
	if config.DrainTimeout == 0 {
		config.DrainTimeout = DefaultDrainTimeout
	}
	if config.HealthCheckTimeout == 0 {
		config.HealthCheckTimeout = DefaultHealthCheckTimeout
	}
	if config.ForceKillTimeout == 0 {
		config.ForceKillTimeout = time.Duration(DefaultForceKillTimeout) * time.Second
	}
	if config.SessionCheckInterval == 0 {
		config.SessionCheckInterval = time.Duration(DefaultSessionCheckInterval) * time.Second
	}

	return &UDPDeploymentManager{
		services: make(map[string]*UDPServiceDeployment),
		config:   config,
	}
}

func (dm *UDPDeploymentManager) DeployUDPService(serviceName string, newTargets []*UDPTarget, sessionManager *SessionManager, drainPolicy SessionDrainPolicy) error {
	dm.lock.Lock()
	defer dm.lock.Unlock()

	deployment := &UDPServiceDeployment{
		ServiceName:    serviceName,
		ServiceType:    "udp",
		NewTargets:     newTargets,
		State:          DeploymentStateHealthChecking,
		StartedAt:      time.Now(),
		SessionManager: sessionManager,
		DrainPolicy:    drainPolicy,
	}

	// Get current deployment if exists
	if existingDeployment := dm.services[serviceName]; existingDeployment != nil {
		deployment.CurrentTargets = existingDeployment.CurrentTargets
	}

	dm.services[serviceName] = deployment

	go dm.executeUDPDeployment(deployment)
	return nil
}

func (dm *UDPDeploymentManager) DeployWebRTCService(serviceName string, newTargets []*UDPTarget, newAllocations []*PortAllocation, sessionManager *SessionManager, portAllocator *PortAllocator, drainPolicy SessionDrainPolicy) error {
	dm.lock.Lock()
	defer dm.lock.Unlock()

	deployment := &UDPServiceDeployment{
		ServiceName:     serviceName,
		ServiceType:     "webrtc",
		NewTargets:      newTargets,
		NewAllocations:  newAllocations,
		State:           DeploymentStateHealthChecking,
		StartedAt:       time.Now(),
		SessionManager:  sessionManager,
		PortAllocator:   portAllocator,
		DrainPolicy:     drainPolicy,
	}

	// Get current deployment if exists
	if existingDeployment := dm.services[serviceName]; existingDeployment != nil {
		deployment.CurrentTargets = existingDeployment.CurrentTargets
		deployment.CurrentAllocations = existingDeployment.CurrentAllocations
	}

	dm.services[serviceName] = deployment

	go dm.executeWebRTCDeployment(deployment)
	return nil
}

func (dm *UDPDeploymentManager) executeUDPDeployment(deployment *UDPServiceDeployment) {
	serviceName := deployment.ServiceName
	
	slog.Info("Starting UDP deployment", 
		"service", serviceName,
		"new_targets", len(deployment.NewTargets),
		"current_targets", len(deployment.CurrentTargets))

	// Phase 1: Health check new targets
	if !dm.healthCheckTargets(deployment.NewTargets, dm.config.HealthCheckTimeout) {
		deployment.State = DeploymentStateFailed
		slog.Error("UDP deployment failed during health check", "service", serviceName)
		return
	}

	// Phase 2: Start routing new sessions to new targets
	deployment.State = DeploymentStateRouting
	dm.switchTrafficToNewTargets(deployment)

	// Phase 3: Drain existing sessions from old targets
	if len(deployment.CurrentTargets) > 0 {
		deployment.State = DeploymentStateDraining
		deployment.DrainStartedAt = time.Now()
		dm.drainOldTargets(deployment)
	}

	// Phase 4: Complete deployment
	deployment.State = DeploymentStateCompleted
	dm.cleanupOldTargets(deployment.CurrentTargets)
	
	slog.Info("UDP deployment completed", "service", serviceName)
}

func (dm *UDPDeploymentManager) executeWebRTCDeployment(deployment *UDPServiceDeployment) {
	serviceName := deployment.ServiceName
	
	slog.Info("Starting WebRTC deployment", 
		"service", serviceName,
		"new_targets", len(deployment.NewTargets),
		"current_targets", len(deployment.CurrentTargets),
		"new_allocations", len(deployment.NewAllocations))

	// Phase 1: Health check new targets
	if !dm.healthCheckTargets(deployment.NewTargets, dm.config.HealthCheckTimeout) {
		deployment.State = DeploymentStateFailed
		// Cleanup failed allocations
		for _, allocation := range deployment.NewAllocations {
			deployment.PortAllocator.DeallocatePort(allocation.RTPPort)
		}
		slog.Error("WebRTC deployment failed during health check", "service", serviceName)
		return
	}

	// Phase 2: Start routing new sessions to new targets
	deployment.State = DeploymentStateRouting
	dm.switchWebRTCTrafficToNewTargets(deployment)

	// Phase 3: Drain existing sessions from old targets and ports
	if len(deployment.CurrentTargets) > 0 || len(deployment.CurrentAllocations) > 0 {
		deployment.State = DeploymentStateDraining
		deployment.DrainStartedAt = time.Now()
		dm.drainOldWebRTCTargets(deployment)
	}

	// Phase 4: Complete deployment
	deployment.State = DeploymentStateCompleted
	dm.cleanupOldTargets(deployment.CurrentTargets)
	dm.cleanupOldAllocations(deployment.CurrentAllocations, deployment.PortAllocator)
	
	slog.Info("WebRTC deployment completed", "service", serviceName)
}

func (dm *UDPDeploymentManager) healthCheckTargets(targets []*UDPTarget, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	checkInterval := 1 * time.Second

	for time.Now().Before(deadline) {
		allHealthy := true
		for _, target := range targets {
			if !target.IsHealthy() {
				allHealthy = false
				break
			}
		}

		if allHealthy {
			slog.Info("All targets are healthy", "count", len(targets))
			return true
		}

		time.Sleep(checkInterval)
	}

	// Log which targets are unhealthy
	unhealthyCount := 0
	for _, target := range targets {
		if !target.IsHealthy() {
			unhealthyCount++
			slog.Warn("Target failed health check", "target", target.URL())
		}
	}

	slog.Error("Health check timeout", 
		"healthy_targets", len(targets)-unhealthyCount,
		"unhealthy_targets", unhealthyCount,
		"timeout", timeout)
	
	return false
}

func (dm *UDPDeploymentManager) switchTrafficToNewTargets(deployment *UDPServiceDeployment) {
	serviceName := deployment.ServiceName

	// Mark old targets as draining in session manager
	if deployment.SessionManager != nil {
		for _, target := range deployment.CurrentTargets {
			deployment.SessionManager.DrainTarget(target.URL())
		}
	}

	slog.Info("Traffic switched to new targets", 
		"service", serviceName,
		"new_targets", len(deployment.NewTargets),
		"draining_targets", len(deployment.CurrentTargets))
}

func (dm *UDPDeploymentManager) switchWebRTCTrafficToNewTargets(deployment *UDPServiceDeployment) {
	serviceName := deployment.ServiceName

	// Mark old targets as draining in session manager
	if deployment.SessionManager != nil {
		for _, target := range deployment.CurrentTargets {
			deployment.SessionManager.DrainTarget(target.URL())
		}
	}

	// Mark old port allocations as draining
	for _, allocation := range deployment.CurrentAllocations {
		allocation.InUse = false
		slog.Debug("Marking port allocation as draining", 
			"service", serviceName,
			"rtp_port", allocation.RTPPort,
			"rtcp_port", allocation.RTCPPort)
	}

	slog.Info("WebRTC traffic switched to new targets", 
		"service", serviceName,
		"new_targets", len(deployment.NewTargets),
		"new_allocations", len(deployment.NewAllocations),
		"draining_targets", len(deployment.CurrentTargets),
		"draining_allocations", len(deployment.CurrentAllocations))
}

func (dm *UDPDeploymentManager) drainOldTargets(deployment *UDPServiceDeployment) {
	dm.drainWithTimeout(deployment, dm.config.DrainTimeout, dm.config.ForceKillTimeout)
}

func (dm *UDPDeploymentManager) drainOldWebRTCTargets(deployment *UDPServiceDeployment) {
	dm.drainWithTimeout(deployment, dm.config.DrainTimeout, dm.config.ForceKillTimeout)
}

func (dm *UDPDeploymentManager) drainWithTimeout(deployment *UDPServiceDeployment, drainTimeout, forceTimeout time.Duration) {
	ticker := time.NewTicker(dm.config.SessionCheckInterval)
	defer ticker.Stop()

	drainDeadline := time.Now().Add(drainTimeout)
	forceDeadline := time.Now().Add(forceTimeout)

	for {
		select {
		case <-ticker.C:
			activeSessions := dm.countActiveSessionsOnTargets(deployment)

			if activeSessions == 0 {
				slog.Info("All sessions drained gracefully", "service", deployment.ServiceName)
				return
			}

			now := time.Now()
			if now.After(drainDeadline) {
				slog.Warn("Drain timeout reached, waiting for force deadline", 
					"service", deployment.ServiceName, 
					"remaining_sessions", activeSessions)
			}

			if now.After(forceDeadline) {
				slog.Warn("Force killing remaining sessions", 
					"service", deployment.ServiceName, 
					"killed_sessions", activeSessions)
				dm.forceKillSessions(deployment)
				return
			}
		}
	}
}

func (dm *UDPDeploymentManager) countActiveSessionsOnTargets(deployment *UDPServiceDeployment) int {
	if deployment.SessionManager == nil {
		return 0
	}

	count := 0
	for _, target := range deployment.CurrentTargets {
		sessions := deployment.SessionManager.GetSessionsForTarget(target.URL())
		count += len(sessions)
	}
	return count
}

func (dm *UDPDeploymentManager) forceKillSessions(deployment *UDPServiceDeployment) {
	if deployment.SessionManager == nil {
		return
	}

	for _, target := range deployment.CurrentTargets {
		sessions := deployment.SessionManager.GetSessionsForTarget(target.URL())
		for _, session := range sessions {
			session.State = SessionStateExpired
		}
	}
}

func (dm *UDPDeploymentManager) cleanupOldTargets(targets []*UDPTarget) {
	for _, target := range targets {
		target.Stop()
	}
}

func (dm *UDPDeploymentManager) cleanupOldAllocations(allocations []*PortAllocation, portAllocator *PortAllocator) {
	for _, allocation := range allocations {
		portAllocator.DeallocatePort(allocation.RTPPort)
	}
}

func (dm *UDPDeploymentManager) GetDeploymentStatus(serviceName string) *UDPServiceDeployment {
	dm.lock.RLock()
	defer dm.lock.RUnlock()
	return dm.services[serviceName]
}

func (dm *UDPDeploymentManager) ListActiveDeployments() map[string]*UDPServiceDeployment {
	dm.lock.RLock()
	defer dm.lock.RUnlock()

	result := make(map[string]*UDPServiceDeployment)
	for name, deployment := range dm.services {
		if deployment.State != DeploymentStateCompleted && deployment.State != DeploymentStateFailed {
			result[name] = deployment
		}
	}
	return result
}

func (dm *UDPDeploymentManager) GetDeploymentStats() map[string]interface{} {
	dm.lock.RLock()
	defer dm.lock.RUnlock()

	stats := map[string]interface{}{
		"total_deployments": len(dm.services),
		"active_deployments": 0,
		"draining_deployments": 0,
		"completed_deployments": 0,
		"failed_deployments": 0,
	}

	for _, deployment := range dm.services {
		switch deployment.State {
		case DeploymentStateHealthChecking, DeploymentStateRouting:
			stats["active_deployments"] = stats["active_deployments"].(int) + 1
		case DeploymentStateDraining:
			stats["draining_deployments"] = stats["draining_deployments"].(int) + 1
		case DeploymentStateCompleted:
			stats["completed_deployments"] = stats["completed_deployments"].(int) + 1
		case DeploymentStateFailed:
			stats["failed_deployments"] = stats["failed_deployments"].(int) + 1
		}
	}

	return stats
}

func (dm *UDPDeploymentManager) Stop() {
	dm.lock.Lock()
	defer dm.lock.Unlock()

	// Stop any ongoing deployments
	for serviceName, deployment := range dm.services {
		if deployment.State == DeploymentStateDraining {
			slog.Info("Stopping ongoing deployment", "service", serviceName)
			dm.forceKillSessions(deployment)
			deployment.State = DeploymentStateCompleted
		}
	}

	slog.Info("UDP deployment manager stopped")
}