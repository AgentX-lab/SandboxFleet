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

func configureGuestNetwork(ctx context.Context, ac *agentClient, slotID int32) error {
	netCfg, err := guestNetForSlot(slotID)
	if err != nil {
		return err
	}
	if err := ac.client.Call(ctx, "grpc.AgentService", "UpdateInterface", &agentpb.UpdateInterfaceRequest{
		Interface: &agentpb.Interface{
			Device: netCfg.Iface,
			Name:   netCfg.Iface,
			HwAddr: netCfg.MAC,
			Mtu:    1500,
			IPAddresses: []*agentpb.IPAddress{
				{Family: agentpb.IPFamily_v4, Address: netCfg.IP, Mask: netCfg.Mask},
			},
		},
	}, &agentpb.Interface{}); err != nil {
		return fmt.Errorf("UpdateInterface: %w", err)
	}
	if err := ac.client.Call(ctx, "grpc.AgentService", "UpdateRoutes", &agentpb.UpdateRoutesRequest{
		Routes: &agentpb.Routes{Routes: []*agentpb.Route{
			{Dest: netCfg.Subnet, Device: netCfg.Iface, Scope: 253, Family: agentpb.IPFamily_v4},
			{Dest: "", Gateway: netCfg.Gateway, Device: netCfg.Iface, Family: agentpb.IPFamily_v4},
		}},
	}, &agentpb.Routes{}); err != nil {
		return fmt.Errorf("UpdateRoutes: %w", err)
	}
	return ac.client.Call(ctx, "grpc.AgentService", "AddARPNeighbors", &agentpb.AddARPNeighborsRequest{
		Neighbors: &agentpb.ARPNeighbors{ARPNeighbors: []*agentpb.ARPNeighbor{{
			ToIPAddress: &agentpb.IPAddress{Family: agentpb.IPFamily_v4, Address: netCfg.Gateway},
			Device:      netCfg.Iface,
			Lladdr:      netCfg.GatewayMAC,
			State:       0x80,
		}}},
	}, &emptypb.Empty{})
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
		Process:     &agentpb.Process{Terminal: false, Args: command},
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
