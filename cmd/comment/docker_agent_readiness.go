package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type dockerAgentReadiness struct {
	State    dockerAgentMountReadiness     `json:"state"`
	Home     dockerAgentMountReadiness     `json:"home"`
	Runtimes []dockerAgentRuntimeReadiness `json:"runtimes"`
}

type dockerAgentMountReadiness struct {
	Path       string `json:"path"`
	Mounted    bool   `json:"mounted"`
	Persistent bool   `json:"persistent"`
	FSType     string `json:"fs_type,omitempty"`
	Source     string `json:"source,omitempty"`
}

type dockerAgentRuntimeReadiness struct {
	Name          string `json:"name"`
	Installed     bool   `json:"installed"`
	Authenticated bool   `json:"authenticated"`
	Hint          string `json:"hint,omitempty"`
}

var dockerAgentMountInfoPath = "/proc/self/mountinfo"

func gatherDockerAgentReadiness(paths commentbus.Paths) dockerAgentReadiness {
	home := os.Getenv("HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	mounts := dockerAgentMountPoints(dockerAgentMountInfoPath)
	return dockerAgentReadiness{
		State:    dockerAgentMountStatus(paths.Home, mounts),
		Home:     dockerAgentMountStatus(home, mounts),
		Runtimes: dockerAgentRuntimeStatuses(),
	}
}

func dockerAgentReadinessIfSandbox(paths commentbus.Paths) *dockerAgentReadiness {
	if !runningInsideDockerAgentSandbox() {
		return nil
	}
	readiness := gatherDockerAgentReadiness(paths)
	return &readiness
}

func addDockerAgentHealth(payload map[string]any, paths commentbus.Paths) map[string]any {
	if readiness := dockerAgentReadinessIfSandbox(paths); readiness != nil {
		payload["agent_sandbox"] = true
		payload["docker_agent_readiness"] = readiness
	}
	return payload
}

type dockerAgentMountPoint struct {
	Path   string
	FSType string
	Source string
}

func dockerAgentMountStatus(path string, mounts map[string]dockerAgentMountPoint) dockerAgentMountReadiness {
	if strings.TrimSpace(path) == "" {
		return dockerAgentMountReadiness{}
	}
	cleaned := filepath.Clean(path)
	mount, mounted := mounts[cleaned]
	// From inside the container, mountinfo can prove that the path is a
	// dedicated non-ephemeral mount. It cannot reliably distinguish Docker named
	// volumes from bind/anonymous volumes across Docker Desktop/Linux variants.
	return dockerAgentMountReadiness{
		Path:       cleaned,
		Mounted:    mounted,
		Persistent: mounted && !dockerAgentEphemeralMount(mount),
		FSType:     mount.FSType,
		Source:     mount.Source,
	}
}

func dockerAgentEphemeralMount(mount dockerAgentMountPoint) bool {
	fsType := strings.ToLower(strings.TrimSpace(mount.FSType))
	source := strings.ToLower(strings.TrimSpace(mount.Source))
	switch fsType {
	case "tmpfs", "ramfs", "devtmpfs":
		return true
	}
	switch source {
	case "tmpfs", "ramfs", "devtmpfs":
		return true
	default:
		return false
	}
}

func dockerAgentRuntimeStatuses() []dockerAgentRuntimeReadiness {
	out := make([]dockerAgentRuntimeReadiness, 0, len(reportableRuntimes))
	for _, runtime := range reportableRuntimes {
		status := dockerAgentRuntimeReadiness{Name: runtime}
		if _, err := runtimeLookPath(runtime); err == nil {
			status.Installed = true
		}
		if ok, hint := runtimeFileAuthState(runtime); ok {
			status.Authenticated = true
		} else {
			status.Hint = hint
		}
		out = append(out, status)
	}
	return out
}

func dockerAgentMountPoints(path string) map[string]dockerAgentMountPoint {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]dockerAgentMountPoint{}
	}
	out := map[string]dockerAgentMountPoint{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		sep := -1
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}
		mountPath := unescapeMountInfoPath(fields[4])
		out[mountPath] = dockerAgentMountPoint{
			Path:   mountPath,
			FSType: fields[sep+1],
			Source: unescapeMountInfoPath(fields[sep+2]),
		}
	}
	return out
}

func unescapeMountInfoPath(path string) string {
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
	)
	return replacer.Replace(path)
}
