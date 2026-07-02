package ephemeral

import (
	"context"
	"fmt"
	"os/exec"
)

// Preflight verifies Docker is usable before a container engine tries to stand up
// an environment. It distinguishes the two failure modes with clear messages:
//   - the docker binary is not installed (exec.LookPath fails), and
//   - the docker daemon is not reachable (`docker version` fails).
//
// It is called at the top of the container engines' restore paths (postgres,
// restic) and by `salvage check`, so every Docker-dependent command reports the
// same honest error instead of failing on the first raw `docker` call. The exec
// engine (spec 0020) is Docker-free and does NOT call this — that is exactly why
// the preflight lives here, on the container path, rather than as a blanket check
// in the CLI that would wrongly block Docker-less exec runs.
func Preflight(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed")
	}
	if err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return fmt.Errorf("docker daemon not reachable (is it running?)")
	}
	return nil
}
