package substratebridge

import (
	"context"
	"fmt"
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

func TestParseActorState(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		want    string
		wantErr bool
	}{
		{
			name: "running actor, full shape",
			json: `{
				"metadata": {"atespace": "ate-demo-counter", "name": "gnwes-actor-1"},
				"status": {"state": "ACTOR_STATE_RUNNING", "assignedNode": "node-a"}
			}`,
			want: "ACTOR_STATE_RUNNING",
		},
		{
			name: "suspended actor, minimal shape",
			json: `{"status": {"state": "ACTOR_STATE_SUSPENDED"}}`,
			want: "ACTOR_STATE_SUSPENDED",
		},
		{
			name:    "missing status.state",
			json:    `{"metadata": {"name": "x"}}`,
			wantErr: true,
		},
		{
			name:    "not JSON at all",
			json:    `kubectl-ate: command not found`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseActorState([]byte(c.json))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got state %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("ParseActorState() = %q, want %q", got, c.want)
			}
		})
	}
}

// fakeFetcher and fakeDriver let ReconcileOnce/Run be tested without
// exec-ing a real kubectl-ate binary or talking to a real Kubernetes API --
// the same interface-injection pattern VMGPUAttachmentReconciler's
// CHDevicePlugger already uses.
type fakeFetcher struct {
	mu    sync.Mutex
	state string
	err   error
	calls int
}

func (f *fakeFetcher) FetchActorState(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.state, nil
}

func (f *fakeFetcher) setState(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

type fakeDriver struct {
	mu       sync.Mutex
	current  gnwesv1alpha1.VMGPUDesiredState
	setErr   error
	setCalls int
}

func (d *fakeDriver) GetDesiredState(context.Context) (gnwesv1alpha1.VMGPUDesiredState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current, nil
}

func (d *fakeDriver) SetDesiredState(_ context.Context, s gnwesv1alpha1.VMGPUDesiredState) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setCalls++
	if d.setErr != nil {
		return d.setErr
	}
	d.current = s
	return nil
}

func TestReconcileOnce_AppliesChangeWhenStatesDiffer(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING"}
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
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING"}
	driver := &fakeDriver{current: gnwesv1alpha1.VMGPUDesiredAttached}

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

func TestReconcileOnce_SetErrorPropagates(t *testing.T) {
	fetcher := &fakeFetcher{state: "ACTOR_STATE_RUNNING"}
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
	fetcher.setState("ACTOR_STATE_RUNNING")
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
