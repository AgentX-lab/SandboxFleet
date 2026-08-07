package snapshotter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Written next to runsc checkpoint files so Upload ships it with the snapshot.
// Restore reads it to create/restore every container (pause + apps), matching
// substrate's multi-container runsc restore protocol.
const gvisorContainersFile = "sandboxfleet-containers.json"

// gvisorRestoreContainer describes one runsc container in a checkpointed sandbox.
type gvisorRestoreContainer struct {
	// ID is the local runsc container id used at restore (exec/delete).
	ID string `json:"id"`
	// Name is io.kubernetes.cri.container-name. Empty for the CRI pause/sandbox
	// container so gVisor registers it as __no_name_N (creation order).
	Name string `json:"name,omitempty"`
	// Sandbox marks the root/pause container (created first).
	Sandbox bool `json:"sandbox,omitempty"`
	// RootfsTar is the snapshot-local archive of this container's host rootfs
	// (packed at Save, unpacked into the restore OCI bundle).
	RootfsTar string `json:"rootfsTar,omitempty"`
}

func gvisorCRIContainers(appName string) []gvisorRestoreContainer {
	return []gvisorRestoreContainer{
		{ID: "pause", Sandbox: true},
		{ID: "app", Name: appName},
	}
}

func writeGVisorContainersFile(dir string, containers []gvisorRestoreContainer) error {
	if len(containers) == 0 {
		return fmt.Errorf("containers list is empty")
	}
	for i, c := range containers {
		if c.ID == "" {
			return fmt.Errorf("containers[%d]: id is required", i)
		}
	}
	raw, err := json.MarshalIndent(containers, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, gvisorContainersFile), raw, 0o600)
}

func readGVisorContainersFile(dir string) ([]gvisorRestoreContainer, error) {
	raw, err := os.ReadFile(filepath.Join(dir, gvisorContainersFile))
	if err != nil {
		return nil, err
	}
	var containers []gvisorRestoreContainer
	if err := json.Unmarshal(raw, &containers); err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("%s is empty", gvisorContainersFile)
	}
	return containers, nil
}

func gvisorAppContainerID(containers []gvisorRestoreContainer) string {
	for i := len(containers) - 1; i >= 0; i-- {
		if !containers[i].Sandbox {
			return containers[i].ID
		}
	}
	if len(containers) > 0 {
		return containers[len(containers)-1].ID
	}
	return ""
}
