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
	// ID is the runsc container id (create/restore/exec/delete). For app
	// containers this must match the checkpointed CRI id (usually the
	// container name), or gVisor rejects restore (savedMFOwners mismatch).
	ID string `json:"id"`
	// Name is io.kubernetes.cri.container-name. Substrate sets this to "pause"
	// for the sandbox container and to the app name for workloads.
	Name string `json:"name,omitempty"`
	// Sandbox marks the root/pause container (created first).
	Sandbox bool `json:"sandbox,omitempty"`
	// Image is the containerd image ref used to mount an overlay rootfs at
	// restore (substrate-style). Pause defaults to the CRI sandbox image.
	Image string `json:"image,omitempty"`
	// RootfsTar is deprecated; ignored when Image is set. Kept for old snapshots.
	RootfsTar string `json:"rootfsTar,omitempty"`
}

func gvisorCRIContainers(appName, appImage string) []gvisorRestoreContainer {
	// Match substrate atelet annotations: pause Name="pause", app ID/Name =
	// container name (ac.GetName()). App runsc ID must match the CRI checkpoint
	// MF owner (savedMFOwners).
	return []gvisorRestoreContainer{
		{ID: "pause", Name: "pause", Sandbox: true, Image: pauseImageRef()},
		{ID: appName, Name: appName, Image: appImage},
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

func fillGVisorContainerImages(containers []gvisorRestoreContainer, appImage string) error {
	for i := range containers {
		if containers[i].Sandbox {
			if containers[i].Image == "" {
				containers[i].Image = pauseImageRef()
			}
			continue
		}
		if containers[i].Image == "" {
			containers[i].Image = appImage
		}
		if containers[i].Image == "" {
			return fmt.Errorf("container %s missing image (and no AppImage fallback)", containers[i].ID)
		}
	}
	return nil
}
