package snapshotter

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/AgentNaut/SandboxFleet/internal/third_party/kata/agentpb"
)

const (
	// Match gVisor / substrate default PATH so kata-agent ExecProcess can find
	// bare binaries (e.g. python for e2e readyz) when Env would otherwise be empty.
	kataDefaultPATH = "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// resolveKataArgv0 turns a relative Process.Args[0] into an in-guest absolute
// path by searching kataDefaultPATH under rootfs. The kata-agent CreateContainer
// lookup is rootfs+argv0 (no PATH), so "sleep" becomes ENOENT while /bin/sleep
// exists — CI e2e-kata Lifecycle failed that way on busybox.
func resolveKataArgv0(rootfs string, args []string) []string {
	if rootfs == "" || len(args) == 0 || args[0] == "" {
		return args
	}
	argv0 := args[0]
	if filepath.IsAbs(argv0) {
		return args
	}
	pathVal := strings.TrimPrefix(kataDefaultPATH, "PATH=")
	for _, dir := range strings.Split(pathVal, ":") {
		if dir == "" {
			continue
		}
		host := filepath.Join(rootfs, filepath.FromSlash(strings.TrimPrefix(dir, "/")), argv0)
		fi, err := os.Stat(host)
		if err != nil || fi.IsDir() {
			continue
		}
		out := append([]string(nil), args...)
		out[0] = dir + "/" + argv0
		return out
	}
	return args
}

// kataExecProcess builds the agent Process for restored-sandbox exec.
func kataExecProcess(command []string) *agentpb.Process {
	return &agentpb.Process{
		Terminal: false,
		Args:     command,
		Env:      []string{kataDefaultPATH},
		Cwd:      "/",
	}
}
