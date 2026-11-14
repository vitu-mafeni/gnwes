// Command fakekubectlate is a minimal stand-in for `kubectl-ate get actor -a
// <atespace> -o json`, used to test cmd/substrate-gpu-bridge end-to-end
// without a real Agent Substrate control plane (config/substrate-integration's
// own setup takes 20-30+ minutes on a cold cache -- not something to require
// for a quick local check of the bridge's own wiring). It ignores its
// arguments (the bridge invokes it the same way it would the real
// kubectl-ate: `get actor --atespace <space> -o json`, listing rather than
// naming one actor, since there is no fixed actor to watch -- see
// internal/substratebridge/kubectlate.go) and prints a canned
// ListActorsResponse-shaped JSON holding one actor, assigned to
// FAKE_WORKER_POD (default "fake-pod"), whose status.state comes from the
// FAKE_ACTOR_STATE_FILE env var -- a plain text file containing one
// ActorState name (e.g. "ACTOR_STATE_RUNNING"), re-read on every invocation
// so a test can flip it live and watch the bridge react on its next poll,
// the same way a real actor's state changes over its real lifecycle.
//
// This is a development/testing tool, not part of the production path.
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	statePath := os.Getenv("FAKE_ACTOR_STATE_FILE")
	state := "ACTOR_STATE_SUSPENDED"
	if statePath != "" {
		if b, err := os.ReadFile(statePath); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				state = s
			}
		}
	}

	workerPod := os.Getenv("FAKE_WORKER_POD")
	if workerPod == "" {
		workerPod = "fake-pod"
	}

	fmt.Printf(`{
  "actors": [
    {
      "metadata": {"atespace": "fake-atespace", "name": "fake-actor", "uid": "fake-actor-uid"},
      "status": {"state": %q, "workerAssignment": {"workerPod": %q}}
    }
  ]
}
`, state, workerPod)
}
