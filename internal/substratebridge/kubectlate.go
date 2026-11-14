package substratebridge

import (
	"context"
	"encoding/json"
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
//
// It lists every actor in the atespace rather than getting one by name,
// because there is no fixed actor to watch: a worker pod hosts actors
// sequentially across its life (suspend one, resume another), and Substrate
// itself tracks the assignment only in the direction actor->worker (an
// Actor's status.workerAssignment.workerPod), never worker->actor -- so
// finding "whichever actor is on this pod right now" means scanning the
// atespace's actors for the one whose assignment names this pod, same as
// kubectl-ate's own `get worker` command does internally
// (filterWorkers/workersHostingAtespace in cmd/kubectl-ate's worker.go).
type KubectlAteFetcher struct {
	Binary     string
	Atespace   string
	WorkerPod  string
	Kubeconfig string

	// VCVMID is the short per-pod token HostAPISocketPath uses in place of
	// the real (and often too-long) worker pod name -- see its doc comment.
	// Unrelated to WorkerPod, which is still the real pod name Substrate's
	// own workerAssignment.workerPod field is matched against.
	VCVMID string
}

func (f *KubectlAteFetcher) FetchActorState(ctx context.Context) (ActorObservation, error) {
	args := []string{"get", "actor", "--atespace", f.Atespace, "-o", "json"}
	if f.Kubeconfig != "" {
		args = append([]string{"--kubeconfig", f.Kubeconfig}, args...)
	}

	cmd := exec.CommandContext(ctx, f.Binary, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ActorObservation{}, fmt.Errorf("%s %v: %w (stderr: %s)", f.Binary, args, err, string(ee.Stderr))
		}
		return ActorObservation{}, fmt.Errorf("running %s %v: %w", f.Binary, args, err)
	}

	obs, err := ParseActorForWorkerPod(out, f.WorkerPod)
	if err != nil {
		return ActorObservation{}, err
	}
	// HostAPISocketPath needs f.WorkerPod, which ParseActorForWorkerPod has
	// no reason to know about (it's a pure actor-list parser) -- filled in
	// here instead, the one place that has both the worker pod name and the
	// actor UID it just found.
	if obs.UID != "" {
		obs.APISocket = HostAPISocketPath(f.VCVMID, obs.UID)
	}
	return obs, nil
}

// actorListJSON is the minimal shape this package needs from
// `kubectl-ate get actor -o json`'s protojson output when listing (no actor
// name given): ateapi.v1.ListActorsResponse, narrowed to metadata.uid,
// status.state, and status.workerAssignment.workerPod. Protojson serializes
// proto field names to lowerCamelCase and enum values to their full name
// string, confirmed against api/ateapi/v1/ateapi.proto's Actor/ActorStatus/
// WorkerAssignment/ListActorsResponse definitions in the real
// agent-substrate/substrate repo.
type actorListJSON struct {
	Actors []struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Status struct {
			State            string `json:"state"`
			WorkerAssignment *struct {
				WorkerPod string `json:"workerPod"`
			} `json:"workerAssignment"`
		} `json:"status"`
	} `json:"actors"`
}

// ParseActorForWorkerPod scans a `kubectl-ate get actor -o json` list
// response (ateapi.v1.ListActorsResponse) for the one actor currently
// assigned to workerPod, and returns its state and UID. Returns a zero
// ActorObservation (state="", uid="") if no actor in the response is
// currently assigned there -- a worker pod between actors (or never used
// yet) is not an error.
func ParseActorForWorkerPod(data []byte, workerPod string) (ActorObservation, error) {
	var list actorListJSON
	if err := json.Unmarshal(data, &list); err != nil {
		return ActorObservation{}, fmt.Errorf("parsing actor list JSON: %w", err)
	}
	for _, a := range list.Actors {
		if a.Status.WorkerAssignment != nil && a.Status.WorkerAssignment.WorkerPod == workerPod {
			return ActorObservation{State: a.Status.State, UID: a.Metadata.UID}, nil
		}
	}
	return ActorObservation{}, nil
}
