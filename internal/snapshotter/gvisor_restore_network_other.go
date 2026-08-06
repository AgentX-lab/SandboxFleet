//go:build !linux

package snapshotter

import (
	"context"
	"fmt"
)

type restoreNetInfo struct {
	Netns  string
	Veth   string
	SlotID int32
	IP     string
}

func (g *GVisor) createRestoreNetwork(context.Context, int32, string) (restoreNetInfo, error) {
	return restoreNetInfo{}, fmt.Errorf("gVisor restore networking requires linux")
}

func deleteRestoreNetwork(context.Context, restoreNetInfo) error { return nil }

func (g *GVisor) runInNetworkNamespace(context.Context, string, []string) error {
	return fmt.Errorf("gVisor restore networking requires linux")
}

func (g *GVisor) loadRestoreNetInfo(string) (restoreNetInfo, error) {
	return restoreNetInfo{}, fmt.Errorf("gVisor restore networking requires linux")
}

func (g *GVisor) restoreNetInfoPath(string) string { return "" }
