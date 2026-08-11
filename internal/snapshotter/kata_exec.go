package snapshotter

import "github.com/AgentNaut/SandboxFleet/internal/third_party/kata/agentpb"

const (
	// Match gVisor / substrate default PATH so kata-agent ExecProcess can find
	// bare binaries (e.g. python for e2e readyz) when Env would otherwise be empty.
	kataDefaultPATH = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// kataExecProcess builds the agent Process for restored-sandbox exec.
func kataExecProcess(command []string) *agentpb.Process {
	return &agentpb.Process{
		Terminal: false,
		Args:     command,
		Env:      []string{kataDefaultPATH},
		Cwd:      "/",
	}
}
