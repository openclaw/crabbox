package unikraftcloud

import (
	"context"
	"sync"
	"testing"
	"time"
)

type unikraftCloudStatusWaitAPI struct {
	*fakeUnikraftCloudAPI
	polled chan struct{}
	once   sync.Once
}

func (api *unikraftCloudStatusWaitAPI) UserUUID(context.Context) (string, error) {
	return testUserUUID, nil
}

func (api *unikraftCloudStatusWaitAPI) ListInstances(context.Context) ([]ukcInstance, error) {
	api.once.Do(func() { close(api.polled) })
	return nil, nil
}

func TestStatusWaitSleepsOutsideLeaseOperationLock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := &fakeUnikraftCloudAPI{baseURL: "https://api.fra.unikraft.cloud"}
	api := &unikraftCloudStatusWaitAPI{
		fakeUnikraftCloudAPI: base,
		polled:               make(chan struct{}),
	}
	b := testBackend(api, nil, nil)
	b.pollInterval = 50 * time.Millisecond
	leaseID := newLeaseID()
	createReq := createInstanceRequest{
		Name:      leaseProviderName(leaseID, ""),
		Image:     b.cfg.UnikraftCloud.Image,
		MemoryMB:  b.cfg.UnikraftCloud.MemoryMB,
		Autostart: true,
	}
	preflight, err := b.createIntentClaim(
		leaseID,
		"status-wait-lock",
		testClaimScope(t, api.BaseURL()),
		testUserUUID,
		WarmupRequest{Repo: Repo{Root: t.TempDir(), Name: "demo"}},
		createReq,
	)
	if err != nil {
		t.Fatalf("create preflight claim: %v", err)
	}
	intent, err := transitionUnikraftCloudCreateState(preflight, ukcStateCreateIntent)
	if err != nil {
		t.Fatalf("arm create intent: %v", err)
	}

	statusErr := make(chan error, 1)
	go func() {
		_, waitErr := b.Status(context.Background(), StatusRequest{ID: intent.LeaseID, Wait: true, WaitTimeout: 500 * time.Millisecond})
		statusErr <- waitErr
	}()
	select {
	case <-api.polled:
	case <-time.After(time.Second):
		t.Fatal("Status did not poll pending create")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := b.Stop(stopCtx, StopRequest{ID: intent.LeaseID}); err != nil {
		t.Fatalf("Stop while Status waits: %v", err)
	}
	select {
	case err := <-statusErr:
		if err == nil {
			t.Fatal("Status succeeded after Stop removed pending claim")
		}
	case <-time.After(time.Second):
		t.Fatal("Status did not observe removed pending claim")
	}
	if len(base.deletedIDs) != 0 {
		t.Fatalf("deleted IDs = %#v, want no remote delete", base.deletedIDs)
	}
}
