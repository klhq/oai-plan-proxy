package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// shortCommit is how many characters of a VCS revision the version shows.
const shortCommit = 12

// build returns this build's version and commit. Release builds stamp them
// with -ldflags. Other builds, such as go install, fall back to the Go build
// information.
func (env Env) build() (version, commit string) {
	version, commit = env.Version, env.Commit
	read := env.BuildInfo
	if read == nil {
		read = debug.ReadBuildInfo
	}
	if info, ok := read(); ok {
		if version == "" {
			version = info.Main.Version
		}
		for _, s := range info.Settings {
			if commit == "" && s.Key == "vcs.revision" {
				commit = s.Value[:min(len(s.Value), shortCommit)]
			}
		}
	}
	if version == "" {
		version = "(devel)"
	}
	if commit == "" {
		commit = "unknown"
	}
	return version, commit
}

// versionLine describes this build in one line.
func (env Env) versionLine() string {
	version, commit := env.build()
	return fmt.Sprintf("oai-plan-proxy %s (commit %s, %s, %s/%s)",
		version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func runVersion(env Env) int {
	fmt.Fprintln(env.Stdout, env.versionLine())
	return 0
}
