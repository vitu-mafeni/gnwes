package substratebridge

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	gnwesv1alpha1 "gnwes/api/v1alpha1"
)

func TestDesiredStateForActorState(t *testing.T) {
	cases := []struct {
		actorState string
		want       gnwesv1alpha1.VMGPUDesiredState
	}{
		{"ACTOR_STATE_RUNNING", gnwesv1alpha1.VMGPUDesiredAttached},
		{"ACTOR_STATE_RESUMING", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_SUSPENDING", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_SUSPENDED", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_CRASHED", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_DELETING", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_REVERTING", gnwesv1alpha1.VMGPUDesiredDetached},
		{"ACTOR_STATE_UNSPECIFIED", gnwesv1alpha1.VMGPUDesiredDetached},
		{"", gnwesv1alpha1.VMGPUDesiredDetached},
		{"SOME_FUTURE_STATE_THIS_CODE_DOESNT_KNOW_ABOUT", gnwesv1alpha1.VMGPUDesiredDetached},
	}
	for _, c := range cases {
		if got := DesiredStateForActorState(c.actorState); got != c.want {
			t.Errorf("DesiredStateForActorState(%q) = %q, want %q", c.actorState, got, c.want)
		}
	}
}

// fakeFetcher and fakeDriver let ReconcileOnce/Run be tested without
// exec-ing a real kubectl-ate binary or talking to a real Kubernetes API --
// the same interface-injection pattern VMGPUAttachmentReconciler's
// CHDevicePlugger already uses.
type fakeFetcher struct {
	mu        sync.Mutex
	state     string
	uid       string
	apiSocket string
	err       error
	calls     int
}

func (f *fakeFetcher) FetchActorState(context.Context) (ActorObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return ActorObservation{}, f.err
	}
	return ActorObservation{State: f.state, UID: f.uid, APISocket: f.apiSocket}, nil
}

func (f *fakeFetcher) setState(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakeFetcher) setObservation(state, uid, apiSocket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	f.uid = uid
	f.apiSocket = apiSocket
}

type fakeDriver struct {
	mu          sync.Mutex
	current     gnwesv1alpha1.VMGPUDesiredState
	currentSock string
	setErr      error
	setCalls    int
}

func (d *fakeDriver) GetSpec(context.Context) (gnwesv1alpha1.VMGPUDesiredState, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current, d.currentSock, nil
}

func (d *fakeDriver) SetSpec(_ context.Context, s gnwesv1alpha1.VMGPUDesiredState, sock string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setCalls++
	if d.setErr != nil {
		return d.setErr
	}
	d.current = s
	d.currentSock = sock
	return nil
}

func TestReconcileOnce_AppliesChangeWhenStatesDiffer(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api.sock"}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached}

	changed, transition, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if transition.DesiredState != gnwesv1alpha1.VMGPUDesiredAttached {
		t.Errorf("transition.DesiredState = %q, want Attached", transition.DesiredState)
	}
	if driver.current != gnwesv1alpha1.VMGPUDesiredAttached {
		t.Errorf("driver.current = %q, want Attached", driver.current)
	}
	if driver.setCalls != 1 {
		t.Errorf("setCalls = %d, want 1", driver.setCalls)
	}
}

func TestReconcileOnce_NoOpWhenAlreadyCorrect(t *testing.T) {
	sock := "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api.sock"
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: sock}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredAttached, currentSock: sock}

	changed, _, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false, no write needed")
	}
	if driver.setCalls != 0 {
		t.Errorf("setCalls = %d, want 0 (should not write when nothing changed)", driver.setCalls)
	}
}

func TestReconcileOnce_FetchErrorPropagates(t *testing.T) {
	fetcher := &fakeFetcher{err: fmt.Errorf("kubectl-ate: connection refused")}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached}

	_, _, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err == nil {
		t.Fatal("expected an error when the fetcher fails")
	}
	if driver.setCalls != 0 {
		t.Errorf("setCalls = %d, want 0 (must not guess a state on fetch failure)", driver.setCalls)
	}
}

func TestReconcileOnce_AppliesAPISocketFromFetcher(t *testing.T) {
	want := "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api-restore.sock"
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: want}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached, currentSock: ""}

	changed, transition, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true")
	}
	if transition.APISocket != want {
		t.Errorf("transition.APISocket = %q, want %q", transition.APISocket, want)
	}
	if driver.currentSock != want {
		t.Errorf("driver.currentSock = %q, want %q", driver.currentSock, want)
	}
}

// TestReconcileOnce_StaysDetachedWhenRunningButSocketUnknown guards
// against regressing into the 2026-11-15 "dial unix: missing address"
// bug: an actor reporting RUNNING before the bridge has learned its real
// api-socket (a real window right after a fresh, non-restore VM boot --
// HostAPISocketPath legitimately returns "" then) must not make
// ReconcileOnce write Attached with an empty socket.
func TestReconcileOnce_StaysDetachedWhenRunningButSocketUnknown(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: ""}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached, currentSock: ""}

	changed, transition, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false (already Detached with no socket to attach), got transition=%+v", transition)
	}
	if transition.DesiredState != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Errorf("transition.DesiredState = %q, want Detached", transition.DesiredState)
	}
	if driver.current != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Errorf("driver.current = %q, want unchanged Detached", driver.current)
	}
}

func TestReconcileOnce_LeavesAPISocketWhenNoActorAssigned(t *testing.T) {
	fetcher := &fakeFetcher{state: "", uid: "", apiSocket: ""}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredAttached, currentSock: "/var/lib/gnwes/vcvm/worker-1/stale-actor/clh-api-restore.sock"}

	changed, _, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true (DesiredState must still fall back to Detached)")
	}
	if driver.current != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Errorf("driver.current = %q, want Detached", driver.current)
	}
	if driver.currentSock != "/var/lib/gnwes/vcvm/worker-1/stale-actor/clh-api-restore.sock" {
		t.Errorf("driver.currentSock = %q, want unchanged (nothing fresher to report)", driver.currentSock)
	}
}

func TestReconcileOnce_NoOpWhenStateAndSocketBothCurrent(t *testing.T) {
	sock := "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api-restore.sock"
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: sock}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredAttached, currentSock: sock}

	changed, _, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if changed {
		t.Fatal("expected changed=false, nothing actually differs")
	}
	if driver.setCalls != 0 {
		t.Errorf("setCalls = %d, want 0", driver.setCalls)
	}
}

// TestHostAPISocketPath_PrefersWhicheverSocketActuallyExists guards
// against clhAPISocketNameRestore becoming a hardcoded, unconditional
// assumption again (as it was before 2026-11-15): a worker pod that was
// recreated from scratch gets a cold `run` launch (clh-api.sock), not a
// restore, even for an actor with prior checkpoint history elsewhere.
func TestHostAPISocketPath_PrefersWhicheverSocketActuallyExists(t *testing.T) {
	base := GNWESHostVCVMDir + "/worker-1/abc-123/"
	restorePath := base + clhAPISocketNameRestore
	freshPath := base + clhAPISocketNameFresh

	cases := []struct {
		name   string
		exists map[string]bool
		want   string
	}{
		{"only restore exists", map[string]bool{restorePath: true}, restorePath},
		{"only fresh exists (the 2026-11-15 bug's real-world case)", map[string]bool{freshPath: true}, freshPath},
		{"both exist, restore preferred", map[string]bool{restorePath: true, freshPath: true}, restorePath},
		{"neither exists yet", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			orig := osStat
			defer func() { osStat = orig }()
			osStat = func(path string) (os.FileInfo, error) {
				if c.exists[path] {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			if got := HostAPISocketPath("worker-1", "abc-123"); got != c.want {
				t.Errorf("HostAPISocketPath(%q, %q) = %q, want %q", "worker-1", "abc-123", got, c.want)
			}
		})
	}
}

func TestReconcileOnce_SetErrorPropagates(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING", uid: "actor-uid-1", apiSocket: "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api.sock"}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached, setErr: fmt.Errorf("conflict")}

	_, _, err := ReconcileOnce(context.Background(), fetcher, driver)
	if err == nil {
		t.Fatal("expected an error when SetDesiredState fails")
	}
}

func TestRun_TracksActorTransitionsOverMultiplePolls(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_SUSPENDED"}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached}

	var mu sync.Mutex
	var transitions []Transition
	var errs []error

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		Run(ctx, fetcher, driver, 5*time.Millisecond,
			func(tr Transition) {
				mu.Lock()
				transitions = append(transitions, tr)
				mu.Unlock()
			},
			func(e error) {
				mu.Lock()
				errs = append(errs, e)
				mu.Unlock()
			},
		)
		close(done)
	}()

	// Give Run its immediate first tick, then flip the actor to RUNNING and
	// let the next poll pick it up.
	time.Sleep(15 * time.Millisecond)
	fetcher.setObservation("ACTOR_STATE_RUNNING", "actor-uid-1", "/var/lib/gnwes/vcvm/worker-1/actor-uid-1/clh-api.sock")
	time.Sleep(30 * time.Millisecond)
	fetcher.setState("ACTOR_STATE_SUSPENDED")
	time.Sleep(30 * time.Millisecond)

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(transitions) < 2 {
		t.Fatalf("expected at least 2 real transitions (Detached->Attached->Detached), got %d: %+v", len(transitions), transitions)
	}
	first := transitions[0]
	if first.DesiredState != gnwesv1alpha1.VMGPUDesiredAttached {
		t.Errorf("first transition = %+v, want DesiredState=Attached", first)
	}
	last := transitions[len(transitions)-1]
	if last.DesiredState != gnwesv1alpha1.VMGPUDesiredDetached {
		t.Errorf("last transition = %+v, want DesiredState=Detached", last)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_SUSPENDED"}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredDetached}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, fetcher, driver, 5*time.Millisecond, nil, nil)
		close(done)
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop within 1s of context cancellation")
	}
}
