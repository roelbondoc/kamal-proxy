package server

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SessionManager struct {
	sessions      map[string]*UDPSession
	lock          sync.RWMutex
	store         SessionStore
	config        SessionConfig
	cleanup       *time.Ticker
	shutdown      chan struct{}
	cleanupDone   chan struct{}
}

type SessionConfig struct {
	IdleTimeout         time.Duration
	CleanupInterval     time.Duration
	StickinessEnabled   bool
	StickinessStrategy  string // "source-ip", "5-tuple", "source-port"
	MaxSessions         int
	PersistenceEnabled  bool
	StorePath           string
}

type UDPSession struct {
	ID          string                 `json:"id"`
	FiveTuple   FiveTuple             `json:"five_tuple"`
	Target      *UDPTarget            `json:"-"` // Don't serialize target
	TargetURL   string                `json:"target_url"`
	ServiceName string                `json:"service_name"`
	CreatedAt   time.Time             `json:"created_at"`
	LastSeen    time.Time             `json:"last_seen"`
	State       SessionState          `json:"state"`
	Protocol    string                `json:"protocol"`
	Metadata    map[string]interface{} `json:"metadata"`
}

type FiveTuple struct {
	SrcIP    string `json:"src_ip"`
	SrcPort  int    `json:"src_port"`
	DstIP    string `json:"dst_ip"`
	DstPort  int    `json:"dst_port"`
	Protocol string `json:"protocol"`
}

type SessionState int

const (
	SessionStateActive SessionState = iota
	SessionStateDraining
	SessionStateExpired
)

type SessionDrainPolicy struct {
	Strategy     string        // "graceful", "immediate", "timeout"
	GracePeriod  time.Duration // Allow existing flows to complete
	MaxWaitTime  time.Duration // Hard cutoff
	NotifyClients bool         // Send termination signals for WebRTC
}

func (s SessionState) String() string {
	switch s {
	case SessionStateActive:
		return "active"
	case SessionStateDraining:
		return "draining"
	case SessionStateExpired:
		return "expired"
	default:
		return "unknown"
	}
}

func (ft FiveTuple) String() string {
	return fmt.Sprintf("%s:%d->%s:%d/%s", ft.SrcIP, ft.SrcPort, ft.DstIP, ft.DstPort, ft.Protocol)
}

func (ft FiveTuple) Hash() string {
	data := fmt.Sprintf("%s:%d:%s:%d:%s", ft.SrcIP, ft.SrcPort, ft.DstIP, ft.DstPort, ft.Protocol)
	hash := md5.Sum([]byte(data))
	return fmt.Sprintf("%x", hash[:8]) // Use first 8 bytes for shorter ID
}

func NewSessionManager(config SessionConfig) *SessionManager {
	sm := &SessionManager{
		sessions:    make(map[string]*UDPSession),
		config:      config,
		shutdown:    make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}

	if config.PersistenceEnabled {
		sm.store = NewFileSessionStore(config.StorePath)
		sm.loadPersistedSessions()
	}

	sm.startCleanup()
	return sm
}

func (sm *SessionManager) RoutePacket(addr net.Addr, dstPort int, serviceName string, targets []*UDPTarget) (*UDPTarget, *UDPSession, error) {
	udpAddr := addr.(*net.UDPAddr)
	fiveTuple := FiveTuple{
		SrcIP:    udpAddr.IP.String(),
		SrcPort:  udpAddr.Port,
		DstIP:    "0.0.0.0", // Will be set by listener
		DstPort:  dstPort,
		Protocol: "udp",
	}

	sessionID := sm.generateSessionID(fiveTuple)

	sm.lock.Lock()
	defer sm.lock.Unlock()

	session := sm.sessions[sessionID]
	if session != nil && session.State == SessionStateActive {
		session.LastSeen = time.Now()
		
		// Verify target is still valid
		if session.Target != nil && session.Target.IsHealthy() {
			return session.Target, session, nil
		}
		
		// Target is unhealthy, need to find new target
		slog.Warn("Session target is unhealthy, selecting new target", 
			"session", sessionID, "old_target", session.TargetURL)
	}

	// Create new session or update existing one with new target
	target := sm.selectTarget(fiveTuple, targets)
	if target == nil {
		return nil, nil, fmt.Errorf("no healthy targets available for service %s", serviceName)
	}

	if session == nil {
		session = &UDPSession{
			ID:          sessionID,
			FiveTuple:   fiveTuple,
			ServiceName: serviceName,
			CreatedAt:   time.Now(),
			State:       SessionStateActive,
			Protocol:    sm.detectProtocol([]byte{}), // Will be enhanced later
			Metadata:    make(map[string]interface{}),
		}
		sm.sessions[sessionID] = session
		
		slog.Debug("Created new UDP session", 
			"session", sessionID, 
			"service", serviceName,
			"client", fiveTuple.String())
	}

	session.Target = target
	session.TargetURL = target.URL()
	session.LastSeen = time.Now()

	// Persist session if enabled
	if sm.config.PersistenceEnabled && sm.store != nil {
		go sm.store.SaveSession(session)
	}

	return target, session, nil
}

func (sm *SessionManager) generateSessionID(fiveTuple FiveTuple) string {
	switch sm.config.StickinessStrategy {
	case "source-ip":
		hash := md5.Sum([]byte(fiveTuple.SrcIP))
		return fmt.Sprintf("ip-%x", hash[:8])
	case "source-port":
		hash := md5.Sum([]byte(fmt.Sprintf("%s:%d", fiveTuple.SrcIP, fiveTuple.SrcPort)))
		return fmt.Sprintf("port-%x", hash[:8])
	case "5-tuple":
		fallthrough
	default:
		return fmt.Sprintf("5tuple-%s", fiveTuple.Hash())
	}
}

func (sm *SessionManager) selectTarget(fiveTuple FiveTuple, targets []*UDPTarget) *UDPTarget {
	if len(targets) == 0 {
		return nil
	}

	// Filter healthy targets
	healthyTargets := make([]*UDPTarget, 0, len(targets))
	for _, target := range targets {
		if target.IsHealthy() {
			healthyTargets = append(healthyTargets, target)
		}
	}

	if len(healthyTargets) == 0 {
		slog.Warn("No healthy targets available", "total_targets", len(targets))
		return nil
	}

	// Use consistent hashing based on session ID for stickiness
	if sm.config.StickinessEnabled {
		sessionID := sm.generateSessionID(fiveTuple)
		hash := md5.Sum([]byte(sessionID))
		index := int(hash[0]) % len(healthyTargets)
		return healthyTargets[index]
	}

	// Simple round-robin for non-sticky sessions
	// In real implementation, this would need proper round-robin state
	return healthyTargets[0]
}

func (sm *SessionManager) detectProtocol(data []byte) string {
	// Basic protocol detection - will be enhanced in Phase 5
	if len(data) == 0 {
		return "udp"
	}
	
	// RTP detection (version bits should be 2)
	if len(data) >= 12 && (data[0]>>6) == 2 {
		return "rtp"
	}
	
	// RTCP detection
	if len(data) >= 8 && (data[0]>>6) == 2 && data[1] >= 200 && data[1] <= 204 {
		return "rtcp"
	}
	
	return "udp"
}

func (sm *SessionManager) GetSession(sessionID string) *UDPSession {
	sm.lock.RLock()
	defer sm.lock.RUnlock()
	return sm.sessions[sessionID]
}

func (sm *SessionManager) GetActiveSessionCount() int {
	sm.lock.RLock()
	defer sm.lock.RUnlock()
	
	count := 0
	for _, session := range sm.sessions {
		if session.State == SessionStateActive {
			count++
		}
	}
	return count
}

func (sm *SessionManager) GetSessionsForTarget(targetURL string) []*UDPSession {
	sm.lock.RLock()
	defer sm.lock.RUnlock()
	
	var sessions []*UDPSession
	for _, session := range sm.sessions {
		if session.TargetURL == targetURL && session.State == SessionStateActive {
			sessions = append(sessions, session)
		}
	}
	return sessions
}

func (sm *SessionManager) DrainTarget(targetURL string) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	
	count := 0
	for _, session := range sm.sessions {
		if session.TargetURL == targetURL && session.State == SessionStateActive {
			session.State = SessionStateDraining
			count++
		}
	}
	
	slog.Info("Started draining sessions from target", "target", targetURL, "sessions", count)
}

func (sm *SessionManager) DrainTargetWithPolicy(targetURL string, policy SessionDrainPolicy) {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	
	sessions := make([]*UDPSession, 0)
	for _, session := range sm.sessions {
		if session.TargetURL == targetURL && session.State == SessionStateActive {
			sessions = append(sessions, session)
		}
	}
	
	switch policy.Strategy {
	case "graceful":
		sm.drainGracefully(sessions, policy)
	case "immediate":
		sm.terminateImmediately(sessions)
	case "timeout":
		sm.drainWithTimeout(sessions, policy)
	default:
		sm.drainGracefully(sessions, policy)
	}
}

func (sm *SessionManager) drainGracefully(sessions []*UDPSession, policy SessionDrainPolicy) {
	for _, session := range sessions {
		// Mark session as draining
		session.State = SessionStateDraining
		
		// For WebRTC, send termination signals if possible
		if session.Protocol == "webrtc" || session.Protocol == "rtp" || session.Protocol == "rtcp" {
			if policy.NotifyClients {
				sm.sendWebRTCBye(session)
			}
		}
		
		// Set expiry based on grace period
		if policy.GracePeriod > 0 {
			session.LastSeen = time.Now().Add(-sm.config.IdleTimeout + policy.GracePeriod)
		}
	}
	
	slog.Info("Started graceful session draining", "sessions", len(sessions))
}

func (sm *SessionManager) terminateImmediately(sessions []*UDPSession) {
	for _, session := range sessions {
		session.State = SessionStateExpired
	}
	
	slog.Info("Terminated sessions immediately", "sessions", len(sessions))
}

func (sm *SessionManager) drainWithTimeout(sessions []*UDPSession, policy SessionDrainPolicy) {
	for _, session := range sessions {
		session.State = SessionStateDraining
		
		// Set expiry based on max wait time
		if policy.MaxWaitTime > 0 {
			session.LastSeen = time.Now().Add(-sm.config.IdleTimeout + policy.MaxWaitTime)
		}
	}
	
	slog.Info("Started timeout-based session draining", "sessions", len(sessions), "timeout", policy.MaxWaitTime)
}

func (sm *SessionManager) sendWebRTCBye(session *UDPSession) {
	// Placeholder for WebRTC BYE message implementation
	// In a real implementation, this would send RTCP BYE packets
	// or WebRTC close messages to gracefully terminate the session
	slog.Debug("Sending WebRTC BYE message", "session", session.ID, "protocol", session.Protocol)
	
	// Add WebRTC termination metadata
	if session.Metadata == nil {
		session.Metadata = make(map[string]interface{})
	}
	session.Metadata["bye_sent"] = time.Now()
	session.Metadata["termination_reason"] = "deployment_drain"
}

func (sm *SessionManager) Stop() {
	close(sm.shutdown)
	<-sm.cleanupDone
	
	if sm.config.PersistenceEnabled && sm.store != nil {
		sm.persistAllSessions()
	}
}

func (sm *SessionManager) startCleanup() {
	sm.cleanup = time.NewTicker(sm.config.CleanupInterval)
	
	go func() {
		defer close(sm.cleanupDone)
		
		for {
			select {
			case <-sm.cleanup.C:
				sm.cleanupExpiredSessions()
			case <-sm.shutdown:
				sm.cleanup.Stop()
				return
			}
		}
	}()
}

func (sm *SessionManager) cleanupExpiredSessions() {
	sm.lock.Lock()
	defer sm.lock.Unlock()
	
	now := time.Now()
	expired := make([]string, 0)
	
	for sessionID, session := range sm.sessions {
		if session.State == SessionStateExpired {
			expired = append(expired, sessionID)
			continue
		}
		
		// Check if session has timed out
		if now.Sub(session.LastSeen) > sm.config.IdleTimeout {
			session.State = SessionStateExpired
			expired = append(expired, sessionID)
		}
	}
	
	// Remove expired sessions
	for _, sessionID := range expired {
		delete(sm.sessions, sessionID)
		if sm.config.PersistenceEnabled && sm.store != nil {
			go sm.store.DeleteSession(sessionID)
		}
	}
	
	if len(expired) > 0 {
		slog.Debug("Cleaned up expired sessions", "count", len(expired))
	}
}

func (sm *SessionManager) loadPersistedSessions() {
	if sm.store == nil {
		return
	}
	
	sessions, err := sm.store.LoadSessions()
	if err != nil {
		slog.Error("Failed to load persisted sessions", "error", err)
		return
	}
	
	now := time.Now()
	loaded := 0
	
	for _, session := range sessions {
		// Skip expired sessions
		if now.Sub(session.LastSeen) > sm.config.IdleTimeout {
			continue
		}
		
		sm.sessions[session.ID] = session
		loaded++
	}
	
	if loaded > 0 {
		slog.Info("Loaded persisted sessions", "count", loaded)
	}
}

func (sm *SessionManager) persistAllSessions() {
	if sm.store == nil {
		return
	}
	
	sm.lock.RLock()
	sessions := make([]*UDPSession, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		if session.State == SessionStateActive {
			sessions = append(sessions, session)
		}
	}
	sm.lock.RUnlock()
	
	for _, session := range sessions {
		sm.store.SaveSession(session)
	}
}

// SessionStore interface for persistence
type SessionStore interface {
	SaveSession(session *UDPSession) error
	LoadSessions() ([]*UDPSession, error)
	DeleteSession(sessionID string) error
}

// FileSessionStore implements SessionStore using filesystem
type FileSessionStore struct {
	storePath string
}

func NewFileSessionStore(storePath string) *FileSessionStore {
	// Ensure directory exists
	os.MkdirAll(storePath, 0755)
	return &FileSessionStore{storePath: storePath}
}

func (fs *FileSessionStore) SaveSession(session *UDPSession) error {
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	
	filename := filepath.Join(fs.storePath, session.ID+".json")
	return os.WriteFile(filename, data, 0644)
}

func (fs *FileSessionStore) LoadSessions() ([]*UDPSession, error) {
	files, err := os.ReadDir(fs.storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []*UDPSession{}, nil
		}
		return nil, err
	}
	
	var sessions []*UDPSession
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".json" {
			filename := filepath.Join(fs.storePath, file.Name())
			data, err := os.ReadFile(filename)
			if err != nil {
				continue
			}
			
			var session UDPSession
			if err := json.Unmarshal(data, &session); err != nil {
				continue
			}
			
			sessions = append(sessions, &session)
		}
	}
	
	return sessions, nil
}

func (fs *FileSessionStore) DeleteSession(sessionID string) error {
	filename := filepath.Join(fs.storePath, sessionID+".json")
	err := os.Remove(filename)
	if os.IsNotExist(err) {
		return nil // Already deleted
	}
	return err
}