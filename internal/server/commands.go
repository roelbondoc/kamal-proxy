package server

import (
	"errors"
	"log/slog"
	"net"
	"net/rpc"
	"sync"
	"time"
)

var registered sync.Once

type CommandHandler struct {
	rpcListener   net.Listener
	router        *Router
	udpServiceMap *UDPServiceMap
}

type DeployArgs struct {
	Service        string
	TargetURLs     []string
	ReaderURLs     []string
	DeployTimeout  time.Duration
	DrainTimeout   time.Duration
	ServiceOptions ServiceOptions
	TargetOptions  TargetOptions
}

type PauseArgs struct {
	Service      string
	DrainTimeout time.Duration
	PauseTimeout time.Duration
}

type StopArgs struct {
	Service      string
	DrainTimeout time.Duration
	Message      string
}

type ResumeArgs struct {
	Service string
}

type RemoveArgs struct {
	Service string
}

type UDPDeployArgs struct {
	Service        string
	TargetURLs     []string
	Port           int
	DeployTimeout  time.Duration
	SessionConfig  *SessionConfig
}

type WebRTCDeployArgs struct {
	Service        string
	TargetURLs     []string
	PortRanges     []PortRange
	RTCPPolicy     string
	DeployTimeout  time.Duration
	SessionConfig  *SessionConfig
}

type UDPZeroDowntimeDeployArgs struct {
	Service          string
	TargetURLs       []string
	Port             int
	DeployTimeout    time.Duration
	DrainTimeout     time.Duration
	ForceKillTimeout time.Duration
	DrainStrategy    string
	SessionConfig    *SessionConfig
}

type WebRTCZeroDowntimeDeployArgs struct {
	Service          string
	TargetURLs       []string
	PortRanges       []PortRange
	RTCPPolicy       string
	DeployTimeout    time.Duration
	DrainTimeout     time.Duration
	ForceKillTimeout time.Duration
	DrainStrategy    string
	SessionConfig    *SessionConfig
}

type RolloutDeployArgs struct {
	Service       string
	TargetURLs    []string
	ReaderURLs    []string
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
}

type RolloutSetArgs struct {
	Service    string
	Percentage int
	Allowlist  []string
}

type RolloutStopArgs struct {
	Service string
}

type ListResponse struct {
	Targets ServiceDescriptionMap `json:"services"`
}

func NewCommandHandler(router *Router, udpServiceMap *UDPServiceMap) *CommandHandler {
	return &CommandHandler{
		router:        router,
		udpServiceMap: udpServiceMap,
	}
}

func (h *CommandHandler) Start(socketPath string) error {
	var err error
	registered.Do(func() {
		err = rpc.RegisterName("kamal-proxy", h)
	})
	if err != nil {
		slog.Error("Failed to register RPC handler", "error", err)
		return err
	}

	h.rpcListener, err = net.Listen("unix", socketPath)
	if err != nil {
		slog.Error("Failed to start RPC listener", "error", err)
		return err
	}

	go func() {
		for {
			conn, err := h.rpcListener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					slog.Debug("Closing RPC listener")
					return
				} else {
					slog.Error("Error accepting RPC connection", "error", err)
					continue
				}
			}

			go rpc.ServeConn(conn)
		}
	}()

	return nil
}

func (h *CommandHandler) Close() error {
	return h.rpcListener.Close()
}

func (h *CommandHandler) Deploy(args DeployArgs, reply *bool) error {
	return h.router.DeployService(args.Service, args.TargetURLs, args.ReaderURLs, args.ServiceOptions, args.TargetOptions, args.DeployTimeout, args.DrainTimeout)
}

func (h *CommandHandler) Pause(args PauseArgs, reply *bool) error {
	return h.router.PauseService(args.Service, args.DrainTimeout, args.PauseTimeout)
}

func (h *CommandHandler) Stop(args StopArgs, reply *bool) error {
	return h.router.StopService(args.Service, args.DrainTimeout, args.Message)
}

func (h *CommandHandler) Resume(args ResumeArgs, reply *bool) error {
	return h.router.ResumeService(args.Service)
}

func (h *CommandHandler) Remove(args RemoveArgs, reply *bool) error {
	return h.router.RemoveService(args.Service)
}

func (h *CommandHandler) List(args bool, reply *ListResponse) error {
	reply.Targets = h.router.ListActiveServices()

	return nil
}

func (h *CommandHandler) RolloutDeploy(args RolloutDeployArgs, reply *bool) error {
	return h.router.SetRolloutTargets(args.Service, args.TargetURLs, args.ReaderURLs, args.DeployTimeout, args.DrainTimeout)
}

func (h *CommandHandler) RolloutSet(args RolloutSetArgs, reply *bool) error {
	return h.router.SetRolloutSplit(args.Service, args.Percentage, args.Allowlist)
}

func (h *CommandHandler) RolloutStop(args RolloutStopArgs, reply *bool) error {
	return h.router.StopRollout(args.Service)
}

func (h *CommandHandler) DeployUDP(args UDPDeployArgs, reply *bool) error {
	return h.udpServiceMap.DeployService(args.Service, args.Port, args.TargetURLs, args.SessionConfig)
}

func (h *CommandHandler) DeployWebRTC(args WebRTCDeployArgs, reply *bool) error {
	return h.udpServiceMap.DeployWebRTCService(args.Service, args.TargetURLs, args.PortRanges, args.SessionConfig)
}

func (h *CommandHandler) DeployUDPZeroDowntime(args UDPZeroDowntimeDeployArgs, reply *bool) error {
	drainPolicy := SessionDrainPolicy{
		Strategy:     args.DrainStrategy,
		GracePeriod:  args.DrainTimeout,
		MaxWaitTime:  args.ForceKillTimeout,
		NotifyClients: false, // UDP doesn't support WebRTC notifications
	}
	return h.udpServiceMap.DeployServiceZeroDowntime(args.Service, args.Port, args.TargetURLs, args.SessionConfig, drainPolicy)
}

func (h *CommandHandler) DeployWebRTCZeroDowntime(args WebRTCZeroDowntimeDeployArgs, reply *bool) error {
	drainPolicy := SessionDrainPolicy{
		Strategy:     args.DrainStrategy,
		GracePeriod:  args.DrainTimeout,
		MaxWaitTime:  args.ForceKillTimeout,
		NotifyClients: true, // WebRTC supports termination notifications
	}
	return h.udpServiceMap.DeployWebRTCServiceZeroDowntime(args.Service, args.TargetURLs, args.PortRanges, args.SessionConfig, drainPolicy)
}
