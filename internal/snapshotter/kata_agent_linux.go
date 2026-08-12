//go:build linux

package snapshotter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"time"

	"github.com/AgentNaut/SandboxFleet/internal/third_party/kata/agentpb"
	"github.com/containerd/ttrpc"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/emptypb"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

const (
	agentVsockPort = 1024
)

type agentClient struct {
	conn   net.Conn
	client *ttrpc.Client
}

func dialAgent(ctx context.Context, vsockPath string) (*agentClient, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", vsockPath)
	if err != nil {
		return nil, fmt.Errorf("dial hybrid vsock %q: %w", vsockPath, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentVsockPort); err != nil {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT response: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock CONNECT refused: %q", strings.TrimSpace(line))
	}
	_ = conn.SetDeadline(time.Time{})
	return &agentClient{conn: conn, client: ttrpc.NewClient(conn)}, nil
}

func (a *agentClient) Close() error {
	err := a.client.Close()
	_ = a.conn.Close()
	return err
}

func dialAgentRetry(ctx context.Context, vsockPath string, timeout time.Duration) (*agentClient, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ac, err := dialAgent(dctx, vsockPath)
		cancel()
		if err == nil {
			return ac, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// guestNetAgent is the slice of the kata-agent API guest networking needs. Both
// the local exec client and overlay.AgentClient (cold boot) satisfy it, so the
// address plan lives in one place.
type guestNetAgent interface {
	UpdateInterface(ctx context.Context, iface *agentpb.Interface) error
	UpdateRoutes(ctx context.Context, routes []*agentpb.Route) error
	AddARPNeighbors(ctx context.Context, neighbors []*agentpb.ARPNeighbor) error
}

func (a *agentClient) UpdateInterface(ctx context.Context, iface *agentpb.Interface) error {
	req := &agentpb.UpdateInterfaceRequest{Interface: iface}
	return a.client.Call(ctx, "grpc.AgentService", "UpdateInterface", req, &agentpb.Interface{})
}

func (a *agentClient) UpdateRoutes(ctx context.Context, routes []*agentpb.Route) error {
	req := &agentpb.UpdateRoutesRequest{Routes: &agentpb.Routes{Routes: routes}}
	return a.client.Call(ctx, "grpc.AgentService", "UpdateRoutes", req, &agentpb.Routes{})
}

func (a *agentClient) AddARPNeighbors(ctx context.Context, neighbors []*agentpb.ARPNeighbor) error {
	req := &agentpb.AddARPNeighborsRequest{Neighbors: &agentpb.ARPNeighbors{ARPNeighbors: neighbors}}
	return a.client.Call(ctx, "grpc.AgentService", "AddARPNeighbors", req, &emptypb.Empty{})
}

// configureGuestNetwork does the kata shim's guest network setup over the agent:
// eth0 IP/MAC/MTU, connected + default routes, and a permanent ARP entry pinning
// the gateway MAC (a restored guest's frozen neighbor entry stays valid).
func configureGuestNetwork(ctx context.Context, ac guestNetAgent, slotID int32) error {
	netCfg, err := guestNetForSlot(slotID)
	if err != nil {
		return err
	}
	if err := ac.UpdateInterface(ctx, &agentpb.Interface{
		Device: netCfg.Iface,
		Name:   netCfg.Iface,
		HwAddr: netCfg.MAC,
		Mtu:    1500,
		IPAddresses: []*agentpb.IPAddress{
			{Family: agentpb.IPFamily_v4, Address: netCfg.IP, Mask: netCfg.Mask},
		},
	}); err != nil {
		return fmt.Errorf("UpdateInterface: %w", err)
	}
	if err := ac.UpdateRoutes(ctx, []*agentpb.Route{
		{Dest: netCfg.Subnet, Device: netCfg.Iface, Scope: 253, Family: agentpb.IPFamily_v4},
		{Dest: "", Gateway: netCfg.Gateway, Device: netCfg.Iface, Family: agentpb.IPFamily_v4},
	}); err != nil {
		return fmt.Errorf("UpdateRoutes: %w", err)
	}
	return ac.AddARPNeighbors(ctx, []*agentpb.ARPNeighbor{{
		ToIPAddress: &agentpb.IPAddress{Family: agentpb.IPFamily_v4, Address: netCfg.Gateway},
		Device:      netCfg.Iface,
		Lladdr:      netCfg.GatewayMAC,
		State:       0x80, // NUD_PERMANENT
	}})
}

func execViaAgent(ctx context.Context, vsockPath, containerID string, command []string) (sandboxruntime.ExecResult, error) {
	if len(command) == 0 {
		return sandboxruntime.ExecResult{}, fmt.Errorf("command is required")
	}
	if containerID == "" {
		return sandboxruntime.ExecResult{}, fmt.Errorf("container id is required")
	}
	ac, err := dialAgent(ctx, vsockPath)
	if err != nil {
		return sandboxruntime.ExecResult{}, err
	}
	defer ac.Close()

	execID := uuid.NewString()
	if err := ac.client.Call(ctx, "grpc.AgentService", "ExecProcess", &agentpb.ExecProcessRequest{
		ContainerId: containerID,
		ExecId:      execID,
		Process:     kataExecProcess(command),
	}, &emptypb.Empty{}); err != nil {
		return sandboxruntime.ExecResult{}, fmt.Errorf("ExecProcess: %w", err)
	}

	readCtx, cancelReads := context.WithCancel(ctx)
	defer cancelReads()
	stdoutCh := make(chan string, 1)
	stderrCh := make(chan string, 1)
	go func() { stdoutCh <- drainAgentStream(readCtx, ac, containerID, execID, false) }()
	go func() { stderrCh <- drainAgentStream(readCtx, ac, containerID, execID, true) }()

	waitResp := &agentpb.WaitProcessResponse{}
	if err := ac.client.Call(ctx, "grpc.AgentService", "WaitProcess", &agentpb.WaitProcessRequest{
		ContainerId: containerID,
		ExecId:      execID,
	}, waitResp); err != nil {
		cancelReads()
		return sandboxruntime.ExecResult{Stdout: <-stdoutCh, Stderr: <-stderrCh}, fmt.Errorf("WaitProcess: %w", err)
	}
	cancelReads()
	return sandboxruntime.ExecResult{
		ExitCode: waitResp.GetStatus(),
		Stdout:   <-stdoutCh,
		Stderr:   <-stderrCh,
	}, nil
}

func drainAgentStream(ctx context.Context, ac *agentClient, containerID, execID string, stderr bool) string {
	var b strings.Builder
	for {
		select {
		case <-ctx.Done():
			return strings.TrimSuffix(b.String(), "\n")
		default:
		}
		resp := &agentpb.ReadStreamResponse{}
		req := &agentpb.ReadStreamRequest{ContainerId: containerID, ExecId: execID, Len: 32768}
		method := "ReadStdout"
		if stderr {
			method = "ReadStderr"
		}
		err := ac.client.Call(ctx, "grpc.AgentService", method, req, resp)
		if len(resp.GetData()) > 0 {
			b.Write(resp.GetData())
		}
		if err != nil {
			return strings.TrimSuffix(b.String(), "\n")
		}
	}
}
