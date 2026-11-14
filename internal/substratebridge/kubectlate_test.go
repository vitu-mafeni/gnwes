package substratebridge

import "testing"

func TestParseActorForWorkerPod(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		workerPod string
		wantState string
		wantUID   string
		wantErr   bool
	}{
		{
			name: "finds the actor assigned to this worker pod among several",
			json: `{"actors": [
				{"metadata": {"uid": "uid-other"}, "status": {"state": "ACTOR_STATE_SUSPENDED"}},
				{"metadata": {"uid": "uid-1"}, "status": {"state": "ACTOR_STATE_RUNNING", "workerAssignment": {"workerPod": "worker-1"}}}
			]}`,
			workerPod: "worker-1",
			wantState: "ACTOR_STATE_RUNNING",
			wantUID:   "uid-1",
		},
		{
			name:      "no actor assigned to this worker pod",
			json:      `{"actors": [{"metadata": {"uid": "uid-other"}, "status": {"state": "ACTOR_STATE_SUSPENDED"}}]}`,
			workerPod: "worker-1",
			wantState: "",
			wantUID:   "",
		},
		{
			name:      "empty actor list",
			json:      `{"actors": []}`,
			workerPod: "worker-1",
			wantState: "",
			wantUID:   "",
		},
		{
			name:      "not JSON at all",
			json:      `kubectl-ate: command not found`,
			workerPod: "worker-1",
			wantErr:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs, err := ParseActorForWorkerPod([]byte(c.json), c.workerPod)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", obs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if obs.State != c.wantState || obs.UID != c.wantUID {
				t.Errorf("ParseActorForWorkerPod() = %+v, want state=%q uid=%q", obs, c.wantState, c.wantUID)
			}
		})
	}
}
