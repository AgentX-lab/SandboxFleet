//go:build linux

package snapshotter

import (
	"testing"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/types"
)

func TestKataSandboxIDPrefersUID(t *testing.T) {
	t.Parallel()
	got := kataSandboxID(sandboxruntime.SandboxIdentity{
		Name: "snap-parent",
		UID:  types.UID("uid-abc"),
	})
	if got != "uid-abc" {
		t.Fatalf("kataSandboxID = %q, want uid-abc", got)
	}
}

func TestKataOverlayWorkloadID(t *testing.T) {
	t.Parallel()
	if got := kataOverlayWorkloadID("app"); got != "app_ovl" {
		t.Fatalf("kataOverlayWorkloadID = %q", got)
	}
}

func TestKataBootVMConfigSharedMemAndFs(t *testing.T) {
	t.Parallel()
	cfg := kataBootVMConfig("sid", "/k", "/img", "agent.log=debug", "/tmp/serial", 512, 1)
	if !cfg.Memory.Shared {
		t.Fatal("Memory.Shared must be true for sparse snapshots")
	}
	if cfg.Memory.Size != 512*1024*1024 {
		t.Fatalf("Memory.Size = %d", cfg.Memory.Size)
	}
	if len(cfg.Fs) != 1 || cfg.Fs[0].Tag != overlay.FsTag || cfg.Fs[0].PciSegment != 1 {
		t.Fatalf("Fs = %+v", cfg.Fs)
	}
	if cfg.Platform == nil || cfg.Platform.NumPciSegments != 2 {
		t.Fatalf("Platform = %+v", cfg.Platform)
	}
	if cfg.Vsock == nil || cfg.Vsock.Cid != 3 {
		t.Fatalf("Vsock = %+v", cfg.Vsock)
	}
}

func TestKataWorkloadSpecAgentRequirements(t *testing.T) {
	t.Parallel()
	spec := kataWorkloadSpec(sandboxruntime.CreateRequest{
		Identity: sandboxruntime.SandboxIdentity{Name: "snap-parent", UID: types.UID("uid-1")},
		Container: sandboxruntime.ContainerConfig{
			Command: []string{"python"},
			Args:    []string{"-c", "print(1)"},
		},
	}, "")
	if spec.Process == nil || spec.Process.Capabilities == nil || len(spec.Process.Capabilities.Bounding) == 0 {
		t.Fatal("Process.Capabilities required by kata-agent")
	}
	if spec.Linux == nil || spec.Linux.Resources == nil || len(spec.Linux.Resources.Devices) == 0 {
		t.Fatal("Linux.Resources required by kata-agent")
	}
	if spec.Linux.CgroupsPath != "/ateomchv/uid-1" {
		t.Fatalf("CgroupsPath = %q", spec.Linux.CgroupsPath)
	}
	var hasRun bool
	for _, m := range spec.Mounts {
		if m.Destination == "/run" {
			hasRun = true
			break
		}
	}
	if !hasRun {
		t.Fatal("missing /run mount")
	}
	var hasNetNS bool
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			hasNetNS = true
			break
		}
	}
	if !hasNetNS {
		t.Fatal("missing network namespace (substrate ensureKataCompatibleSpec shape)")
	}

	pb := overlay.SpecToAgentPB(spec)
	if pb.Linux == nil {
		t.Fatal("SpecToAgentPB Linux nil")
	}
	for _, ns := range pb.Linux.Namespaces {
		if ns.Type == string(specs.NetworkNamespace) {
			t.Fatal("SpecToAgentPB must drop network namespace so workload shares sandbox eth0")
		}
	}
}
