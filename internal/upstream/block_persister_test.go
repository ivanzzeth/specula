package upstream

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeBlockPersister struct {
	mu    sync.Mutex
	byKey map[string]BlockState
}

func (f *fakeBlockPersister) Load(_ context.Context, name string) (BlockState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.byKey[name], nil
}

func (f *fakeBlockPersister) Save(_ context.Context, name string, state BlockState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byKey == nil {
		f.byKey = make(map[string]BlockState)
	}
	f.byKey[name] = state
	return nil
}

func (f *fakeBlockPersister) Delete(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byKey, name)
	return nil
}

func TestBlockTracker_PersistedCrossRuntimeSync(t *testing.T) {
	store := &fakeBlockPersister{}
	rtA := NewRuntimeWithBlockPersister("oci", store)
	rtB := NewRuntimeWithBlockPersister("oci", store)

	bt := newBlockTrackerWithPersister(store, 2, time.Minute)

	bt.recordFailure("mirror")
	bt.recordFailure("mirror") // threshold=2
	if !bt.isBlocked("mirror") {
		t.Fatal("expected blocked after threshold")
	}

	if !rtA.blocker.isBlocked("mirror") {
		t.Fatal("runtime A should see persisted block")
	}
	if !rtB.blocker.isBlocked("mirror") {
		t.Fatal("runtime B should see persisted block")
	}

	rtA.blocker.recordSuccess("mirror")
	if rtB.blocker.isBlocked("mirror") {
		t.Fatal("success on A should clear block for B")
	}
}

func TestRegistry_SharesBlockPersisterAcrossRuntimes(t *testing.T) {
	store := &fakeBlockPersister{}
	reg := NewRegistryWithBlockPersister(func(protocol string) BlockPersister {
		if protocol != "oci" {
			t.Fatalf("unexpected protocol %q", protocol)
		}
		return store
	})

	rt1 := reg.Runtime("oci")
	rt2 := reg.Runtime("oci")
	if rt1 != rt2 {
		t.Fatal("expected stable runtime instance")
	}

	for i := 0; i < defaultMaxFailures; i++ {
		rt1.RecordFailure("mid", err503, true)
	}

	s := stateByName(rt2.Snapshot([]Upstream{{Name: "mid"}}))["mid"]
	if s.Health != HealthBlocked {
		t.Fatalf("health=%q want blocked", s.Health)
	}
}

var err503 = &statusErr{code: 503}

type statusErr struct{ code int }

func (e *statusErr) Error() string { return "HTTP 503" }
