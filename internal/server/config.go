package server

import (
	"cmp"
	"os"
	"path"
	"syscall"
)

const (
	DefaultHttpPort        = 80
	DefaultHttpsPort       = 443
	DefaultUdpPort         = 8080
	DefaultSessionTimeout  = 60 // seconds
	DefaultCleanupInterval = 30 // seconds
	
	// WebRTC Port Range Defaults
	DefaultRTPPortStart    = 10000
	DefaultRTPPortEnd      = 20000
	DefaultRTCPPortStart   = 20001
	DefaultRTCPPortEnd     = 30000
	DefaultPortAllocationTTL = 300 // seconds
	
	// Zero-downtime deployment defaults - use service.go constants for drain/health check
	DefaultForceKillTimeout    = 120 // seconds
	DefaultSessionCheckInterval = 5  // seconds
)

type Config struct {
	Bind        string
	HttpPort    int
	HttpsPort   int
	UdpPort     int
	MetricsPort int

	AlternateConfigDir string
}

func (c Config) SocketPath() string {
	return path.Join(c.runtimeDirectory(), "kamal-proxy.sock")
}

func (c Config) StatePath() string {
	return path.Join(c.dataDirectory(), "kamal-proxy.state")
}

func (c Config) CertificatePath() string {
	return path.Join(c.dataDirectory(), "certs")
}

func (c Config) UdpStatePath() string {
	return path.Join(c.dataDirectory(), "kamal-proxy-udp.state")
}

func (c Config) SessionStorePath() string {
	return path.Join(c.dataDirectory(), "udp-sessions")
}

// Private

func (c Config) runtimeDirectory() string {
	return cmp.Or(os.Getenv("XDG_RUNTIME_DIR"), os.TempDir())
}

func (c Config) dataDirectory() string {
	return cmp.Or(c.AlternateConfigDir, c.defaultDataDirectory())
}

func (c Config) defaultDataDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}

	dir := path.Join(home, ".config", "kamal-proxy")

	err = os.MkdirAll(dir, syscall.S_IRUSR|syscall.S_IWUSR|syscall.S_IXUSR)
	if err != nil {
		dir = os.TempDir()
	}

	return dir
}
