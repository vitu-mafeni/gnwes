package substratebridge

import (
	"context"
	"fmt"
	"os/exec"
)

// KubectlAteFetcher implements ActorStateFetcher by shelling out to
// kubectl-ate, the same tool and invocation pattern
// config/substrate-integration already established for this repo (rather
// than importing agent-substrate's internal Go packages across module
// boundaries). kubectl-ate does not build natively on Windows
// (internal/imagecache uses syscall.Stat_t) -- see RUNBOOK.md's Tier 3
// section for the cross-compiled-binary-in-a-throwaway-container pattern
// already worked out for exactly this reason. Shared by every command that
// needs to watch a real actor's lifecycle (cmd/substrate-gpu-bridge,
// cmd/hami-substrate-bridge), so the real, already-proven invocation isn't
// duplicated per command.
type KubectlAteFetcher struct {
	Binary     string
	Atespace   string
	ActorName  string
	Kubeconfig string
}

func (f *KubectlAteFetcher) FetchActorState(ctx context.Context) (string, error) {
	args := []string{"get", "actor", f.ActorName, "--atespace", f.Atespace, "-o", "json"}
	if f.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", f.Kubeconfig}, args...)
	}

	cmd := exec.CommandContext(ctx, f.Binary, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %v: %w (stderr: %s)", f.Binary, args, err, string(ee.Stderr))
		}
		return "", fmt.Errorf("running %s %v: %w", f.Binary, args, err)
	}
	return ParseActorState(out)
}
