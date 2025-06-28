package cmd

import (
	"fmt"
	"net/rpc"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basecamp/kamal-proxy/internal/server"
)

type deployUDPCommand struct {
	cmd              *cobra.Command
	args             server.UDPDeployArgs
	webrtcArgs       server.WebRTCDeployArgs
	sessionTimeout   time.Duration
	sessionAffinity  bool
	affinityStrategy string
	protocol         string
	portRanges       []string
	rtcpPolicy       string
	
	// Zero-downtime deployment options
	drainTimeout     time.Duration
	drainStrategy    string
	forceKillTimeout time.Duration
	zeroDowntime     bool
}

func newDeployUDPCommand() *deployUDPCommand {
	deployUDPCommand := &deployUDPCommand{}
	deployUDPCommand.cmd = &cobra.Command{
		Use:   "deploy-udp <service>",
		Short: "Deploy a UDP service target",
		Long: `Deploy a UDP/WebRTC service to the proxy.

Examples:
  # Deploy a simple UDP service
  kamal-proxy deploy-udp gameserver --target game:9999 --port 9999
  
  # Deploy a LiveKit WebRTC service with dynamic port allocation
  kamal-proxy deploy-udp livekit --target livekit:7880 --protocol webrtc --port-ranges 10000-20000,20001-30000
  
  # Deploy with session affinity and custom RTCP policy
  kamal-proxy deploy-udp media --target media:8000 --protocol webrtc --port-ranges 15000-16000 --rtcp-policy separate-range --session-timeout 300s
  
  # Deploy with zero-downtime using graceful session draining
  kamal-proxy deploy-udp gameserver --target game:9999 --port 9999 --zero-downtime --drain-strategy graceful --drain-timeout 60s`,
		PreRunE: deployUDPCommand.preRun,
		RunE:    deployUDPCommand.run,
		Args:    cobra.ExactArgs(1),
	}

	deployUDPCommand.cmd.Flags().StringSliceVar(&deployUDPCommand.args.TargetURLs, "target", []string{}, "Target host(s) to deploy")
	deployUDPCommand.cmd.Flags().IntVar(&deployUDPCommand.args.Port, "port", 0, "UDP port to bind to (for simple UDP services)")
	deployUDPCommand.cmd.Flags().DurationVar(&deployUDPCommand.args.DeployTimeout, "deploy-timeout", server.DefaultDeployTimeout, "Maximum time to wait for the new target to become healthy")
	
	// Protocol options
	deployUDPCommand.cmd.Flags().StringVar(&deployUDPCommand.protocol, "protocol", "udp", "Protocol type (udp, webrtc)")
	deployUDPCommand.cmd.Flags().StringSliceVar(&deployUDPCommand.portRanges, "port-ranges", []string{}, "Port ranges for WebRTC (e.g., 10000-20000,20001-30000)")
	deployUDPCommand.cmd.Flags().StringVar(&deployUDPCommand.rtcpPolicy, "rtcp-policy", "adjacent", "RTCP port policy for WebRTC (adjacent, separate-range, auto)")
	
	// Session management options
	deployUDPCommand.cmd.Flags().BoolVar(&deployUDPCommand.sessionAffinity, "session-affinity", true, "Enable session stickiness")
	deployUDPCommand.cmd.Flags().DurationVar(&deployUDPCommand.sessionTimeout, "session-timeout", time.Duration(server.DefaultSessionTimeout)*time.Second, "Session idle timeout")
	deployUDPCommand.cmd.Flags().StringVar(&deployUDPCommand.affinityStrategy, "affinity-strategy", "5-tuple", "Session affinity strategy (5-tuple, source-ip, source-port)")
	
	// Zero-downtime deployment options
	deployUDPCommand.cmd.Flags().BoolVar(&deployUDPCommand.zeroDowntime, "zero-downtime", false, "Enable zero-downtime deployment with session draining")
	deployUDPCommand.cmd.Flags().DurationVar(&deployUDPCommand.drainTimeout, "drain-timeout", server.DefaultDrainTimeout, "Time to wait for sessions to drain gracefully")
	deployUDPCommand.cmd.Flags().StringVar(&deployUDPCommand.drainStrategy, "drain-strategy", "graceful", "Session drain strategy (graceful, immediate, timeout)")
	deployUDPCommand.cmd.Flags().DurationVar(&deployUDPCommand.forceKillTimeout, "force-kill-timeout", time.Duration(server.DefaultForceKillTimeout)*time.Second, "Maximum time before force-killing remaining sessions")

	deployUDPCommand.cmd.MarkFlagRequired("target")

	return deployUDPCommand
}

func (cmd *deployUDPCommand) preRun(command *cobra.Command, args []string) error {
	serviceName := args[0]
	cmd.args.Service = serviceName
	cmd.webrtcArgs.Service = serviceName

	if len(cmd.args.TargetURLs) == 0 {
		return fmt.Errorf("at least one target must be specified")
	}

	// Validate protocol
	if cmd.protocol != "udp" && cmd.protocol != "webrtc" {
		return fmt.Errorf("invalid protocol: %s. Valid options: udp, webrtc", cmd.protocol)
	}

	// Protocol-specific validation
	if cmd.protocol == "udp" {
		if cmd.args.Port <= 0 || cmd.args.Port > 65535 {
			return fmt.Errorf("UDP services require a specific port between 1 and 65535")
		}
	} else if cmd.protocol == "webrtc" {
		if len(cmd.portRanges) == 0 {
			return fmt.Errorf("WebRTC services require port ranges")
		}
		
		// Parse port ranges
		ranges, err := cmd.parsePortRanges()
		if err != nil {
			return fmt.Errorf("invalid port ranges: %w", err)
		}
		cmd.webrtcArgs.PortRanges = ranges
		cmd.webrtcArgs.TargetURLs = cmd.args.TargetURLs
		cmd.webrtcArgs.RTCPPolicy = cmd.rtcpPolicy
		cmd.webrtcArgs.DeployTimeout = cmd.args.DeployTimeout

		// Validate RTCP policy
		validPolicies := []string{"adjacent", "separate-range", "auto"}
		valid := false
		for _, policy := range validPolicies {
			if cmd.rtcpPolicy == policy {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("invalid RTCP policy: %s. Valid options: %v", cmd.rtcpPolicy, validPolicies)
		}
	}

	// Validate affinity strategy
	validStrategies := []string{"5-tuple", "source-ip", "source-port"}
	valid := false
	for _, strategy := range validStrategies {
		if cmd.affinityStrategy == strategy {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid affinity strategy: %s. Valid options: %v", cmd.affinityStrategy, validStrategies)
	}

	// Validate drain strategy
	validDrainStrategies := []string{"graceful", "immediate", "timeout"}
	valid = false
	for _, strategy := range validDrainStrategies {
		if cmd.drainStrategy == strategy {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid drain strategy: %s. Valid options: %v", cmd.drainStrategy, validDrainStrategies)
	}

	// Build session config
	sessionConfig := &server.SessionConfig{
		IdleTimeout:         cmd.sessionTimeout,
		CleanupInterval:     time.Duration(server.DefaultCleanupInterval) * time.Second,
		StickinessEnabled:   cmd.sessionAffinity,
		StickinessStrategy:  cmd.affinityStrategy,
		MaxSessions:         10000,
		PersistenceEnabled:  true,
	}
	
	cmd.args.SessionConfig = sessionConfig
	cmd.webrtcArgs.SessionConfig = sessionConfig

	return nil
}

func (cmd *deployUDPCommand) parsePortRanges() ([]server.PortRange, error) {
	var ranges []server.PortRange
	
	for _, rangeStr := range cmd.portRanges {
		parts := strings.Split(rangeStr, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port range format: %s (expected start-end)", rangeStr)
		}
		
		start, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil, fmt.Errorf("invalid start port: %s", parts[0])
		}
		
		end, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("invalid end port: %s", parts[1])
		}
		
		if start <= 0 || end <= 0 || start > 65535 || end > 65535 {
			return nil, fmt.Errorf("ports must be between 1 and 65535")
		}
		
		if start >= end {
			return nil, fmt.Errorf("start port must be less than end port")
		}
		
		ranges = append(ranges, server.PortRange{
			Start: start,
			End:   end,
			Step:  2, // Default for RTP/RTCP pairs
		})
	}
	
	return ranges, nil
}

func (cmd *deployUDPCommand) run(command *cobra.Command, args []string) error {
	client, err := rpc.Dial("unix", globalConfig.SocketPath())
	if err != nil {
		return fmt.Errorf("failed to connect to kamal-proxy: %w (is the server running?)", err)
	}
	defer client.Close()

	var reply bool
	
	if cmd.protocol == "webrtc" {
		if cmd.zeroDowntime {
			// Use zero-downtime WebRTC deployment
			zeroDowntimeArgs := server.WebRTCZeroDowntimeDeployArgs{
				Service:          cmd.webrtcArgs.Service,
				TargetURLs:       cmd.webrtcArgs.TargetURLs,
				PortRanges:       cmd.webrtcArgs.PortRanges,
				RTCPPolicy:       cmd.webrtcArgs.RTCPPolicy,
				DeployTimeout:    cmd.webrtcArgs.DeployTimeout,
				DrainTimeout:     cmd.drainTimeout,
				ForceKillTimeout: cmd.forceKillTimeout,
				DrainStrategy:    cmd.drainStrategy,
				SessionConfig:    cmd.webrtcArgs.SessionConfig,
			}
			
			err = client.Call("kamal-proxy.DeployWebRTCZeroDowntime", zeroDowntimeArgs, &reply)
			if err != nil {
				return fmt.Errorf("WebRTC zero-downtime deployment failed: %w", err)
			}
			
			fmt.Printf("Deployed WebRTC service '%s' with zero-downtime strategy\n", cmd.webrtcArgs.Service)
		} else {
			err = client.Call("kamal-proxy.DeployWebRTC", cmd.webrtcArgs, &reply)
			if err != nil {
				return fmt.Errorf("WebRTC deployment failed: %w", err)
			}
			
			fmt.Printf("Deployed WebRTC service '%s' with targets: %v\n", 
				cmd.webrtcArgs.Service, cmd.webrtcArgs.TargetURLs)
		}
		
		affinityStatus := "disabled"
		if cmd.sessionAffinity {
			affinityStatus = fmt.Sprintf("enabled (%s)", cmd.affinityStrategy)
		}
		
		fmt.Printf("Port ranges: %v, RTCP policy: %s\n", 
			cmd.portRanges, cmd.rtcpPolicy)
		fmt.Printf("Session affinity: %s, timeout: %v\n", 
			affinityStatus, cmd.sessionTimeout)
		
		if cmd.zeroDowntime {
			fmt.Printf("Zero-downtime: drain strategy=%s, drain timeout=%v, force kill=%v\n",
				cmd.drainStrategy, cmd.drainTimeout, cmd.forceKillTimeout)
		}
			
	} else {
		if cmd.zeroDowntime {
			// Use zero-downtime UDP deployment
			zeroDowntimeArgs := server.UDPZeroDowntimeDeployArgs{
				Service:          cmd.args.Service,
				TargetURLs:       cmd.args.TargetURLs,
				Port:             cmd.args.Port,
				DeployTimeout:    cmd.args.DeployTimeout,
				DrainTimeout:     cmd.drainTimeout,
				ForceKillTimeout: cmd.forceKillTimeout,
				DrainStrategy:    cmd.drainStrategy,
				SessionConfig:    cmd.args.SessionConfig,
			}
			
			err = client.Call("kamal-proxy.DeployUDPZeroDowntime", zeroDowntimeArgs, &reply)
			if err != nil {
				return fmt.Errorf("UDP zero-downtime deployment failed: %w", err)
			}
			
			fmt.Printf("Deployed UDP service '%s' with zero-downtime strategy\n", cmd.args.Service)
		} else {
			err = client.Call("kamal-proxy.DeployUDP", cmd.args, &reply)
			if err != nil {
				return fmt.Errorf("UDP deployment failed: %w", err)
			}
			
			fmt.Printf("Deployed UDP service '%s' with targets: %v\n", 
				cmd.args.Service, cmd.args.TargetURLs)
		}
		
		affinityStatus := "disabled"
		if cmd.sessionAffinity {
			affinityStatus = fmt.Sprintf("enabled (%s)", cmd.affinityStrategy)
		}
		
		fmt.Printf("Port: %d\n", cmd.args.Port)
		fmt.Printf("Session affinity: %s, timeout: %v\n", 
			affinityStatus, cmd.sessionTimeout)
			
		if cmd.zeroDowntime {
			fmt.Printf("Zero-downtime: drain strategy=%s, drain timeout=%v, force kill=%v\n",
				cmd.drainStrategy, cmd.drainTimeout, cmd.forceKillTimeout)
		}
	}

	return nil
}