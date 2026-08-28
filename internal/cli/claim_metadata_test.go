package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// This child holds the real operation flock, not the parent's in-process mutex.
func TestClaimMetadataOwnerProcess(t *testing.T) {
	path := os.Getenv("CRABBOX_TEST_CLAIM_OWNER_PATH")
	if path == "" {
		return
	}
	if err := withLeaseClaimLock(path, func() error {
		fmt.Fprintln(os.Stdout, "locked")
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func holdClaimMetadataOwner(t *testing.T, claim leaseClaim) (release func(), assertOwned func()) {
	t.Helper()
	path, err := leaseClaimPath(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestClaimMetadataOwnerProcess$")
	cmd.Env = append(os.Environ(), "CRABBOX_TEST_CLAIM_OWNER_PATH="+path)
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		line, _ := bufio.NewReader(output).ReadString('\n')
		ready <- line
		_, _ = io.Copy(io.Discard, output)
	}()
	var once sync.Once
	release = func() {
		once.Do(func() {
			_ = input.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("owner child: %v", err)
			}
			<-readDone
		})
	}
	t.Cleanup(release)
	select {
	case line := <-ready:
		if line != "locked\n" {
			t.Fatalf("owner handshake=%q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not acquire flock")
	}
	lockPath, err := leaseClaimLockPath(path)
	if err != nil {
		t.Fatal(err)
	}
	lockInfo, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	assertOwned = func() {
		t.Helper()
		probe := flock.New(lockPath)
		locked, err := probe.TryLock()
		_ = probe.Close()
		if err != nil || locked {
			t.Fatalf("owner flock lost: locked=%t err=%v", locked, err)
		}
		current, err := os.Stat(lockPath)
		if err != nil || !os.SameFile(lockInfo, current) {
			t.Fatalf("owner lock file replaced/deleted: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatalf("owner claim changed: %v", err)
		}
	}
	assertOwned()
	return release, assertOwned
}

func TestClaimMetadataObservationDuringReplacementFencesCleanup(t *testing.T) {
	isolateTestUserDirs(t)
	before := seedClaimContract(t)
	replacement := cloneLeaseClaim(before)
	replacement.RepoRoot = "/repo/replacement"
	entered, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	var updated leaseClaim
	var replaceErr error
	go func() {
		defer close(finished)
		updated, replaceErr = replaceLeaseClaimIfUnchangedDurableAfter(before.LeaseID, before, replacement, func() error {
			close(entered)
			<-proceed
			return nil
		})
	}()
	defer func() { release(); <-finished }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement action did not enter")
	}
	var observed leaseClaim
	requireClaimMetadataProgress(t, release, func() error {
		var err error
		observed, err = readLeaseClaim(before.LeaseID)
		return err
	})
	if !reflect.DeepEqual(observed, before) {
		t.Fatalf("reader saw unpublished candidate: %#v", observed)
	}
	started, cleaned := make(chan struct{}), make(chan error, 1)
	called := make(chan struct{}, 1)
	go func() {
		close(started)
		cleaned <- removeLeaseClaimIfUnchangedAfter(before.LeaseID, observed, func() error {
			called <- struct{}{}
			return nil
		})
	}()
	<-started
	release()
	err := <-cleaned
	<-finished
	if replaceErr != nil || updated.Revision == before.Revision {
		t.Fatalf("replacement err=%v revision=%q", replaceErr, updated.Revision)
	}
	if err == nil || !strings.Contains(err.Error(), "claim changed") || len(called) != 0 {
		t.Fatalf("stale cleanup ran: calls=%d err=%v", len(called), err)
	}
	assertClaimContractStored(t, before.LeaseID, updated)
}

// A failing baseline must release its own owner and join the blocked reader.
func requireClaimMetadataProgress(t *testing.T, release func(), action func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- action() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		release()
		err := <-done
		t.Fatalf("metadata waited on live owner; after releasing fixture: %v", err)
	}
}

func TestClaimMetadataDiscoveryUnderExternalOwner(t *testing.T) {
	for _, operation := range []string{"read", "list", "snapshot", "resolve", "requested slug", "generated slug", "direct slug"} {
		t.Run(operation, func(t *testing.T) {
			isolateTestUserDirs(t)
			const nextID = "tbx_independent"
			owner := leaseClaim{LeaseID: "cbx_metadata_owner", Revision: "initial", Provider: "aws", Slug: newLeaseSlug(nextID), RepoRoot: "/repo/owner"}
			writeClaimsListFixture(t, owner.LeaseID+".json", owner)
			release, assertOwned := holdClaimMetadataOwner(t, owner)
			requireClaimMetadataProgress(t, release, func() error {
				switch operation {
				case "read", "resolve":
					var got leaseClaim
					var err error
					if operation == "read" {
						got, err = readLeaseClaim(owner.LeaseID)
					} else {
						got, _, err = resolveLeaseClaim(owner.Slug)
					}
					if err != nil || !reflect.DeepEqual(got, owner) {
						return fmt.Errorf("active claim missing: got=%#v err=%v", got, err)
					}
				case "list", "snapshot":
					var claims []leaseClaim
					var err error
					if operation == "list" {
						claims, err = listLeaseClaims()
					} else {
						var snapshot leaseClaimsSnapshot
						snapshot, err = snapshotLeaseClaims()
						claims = snapshot.claims
					}
					if err != nil || !reflect.DeepEqual(claims, []leaseClaim{owner}) {
						return fmt.Errorf("active claim omitted: claims=%#v err=%v", claims, err)
					}
				default:
					var slug string
					var err error
					switch operation {
					case "requested slug":
						slug, err = allocateClaimLeaseSlug(nextID, owner.Slug)
					case "generated slug":
						slug, err = allocateClaimLeaseSlug(nextID, "")
					case "direct slug":
						slug, err = allocateDirectLeaseSlug(nextID, owner.Slug, nil)
					}
					if err != nil || slug == owner.Slug || !strings.HasPrefix(slug, owner.Slug+"-") {
						return fmt.Errorf("busy slug was not reserved: slug=%q err=%v", slug, err)
					}
				}
				return nil
			})
			assertOwned()
		})
	}
}

func TestClaimMetadataSSHResolveUnderExternalOwner(t *testing.T) {
	isolateTestUserDirs(t)
	owner := leaseClaim{LeaseID: "cbx_metadata_owner", Provider: "aws"}
	writeClaimsListFixture(t, owner.LeaseID+".json", owner)
	want := leaseClaim{LeaseID: "cbx_metadata_sibling", Revision: "initial", Provider: "external", ProviderScope: "scope", CloudID: "resource", RepoRoot: "/repo", Slug: "sibling"}
	writeClaimsListFixture(t, want.LeaseID+".json", want)
	release, assertOwned := holdClaimMetadataOwner(t, owner)
	requireClaimMetadataProgress(t, release, func() error {
		lease, err := resolveSSHLeaseTarget(context.Background(), resolveResultBackend{
			testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: want.Provider}},
			lease:          LeaseTarget{LeaseID: want.LeaseID, Server: Server{Provider: want.Provider, CloudID: want.CloudID}},
		}, ResolveRequest{ID: want.LeaseID, Repo: Repo{Root: want.RepoRoot}, Options: LeaseOptions{ProviderScope: want.ProviderScope}})
		if err != nil {
			return err
		}
		if !lease.Server.claimSnapshotSet || !lease.Server.claimSnapshotExists || !reflect.DeepEqual(lease.Server.claimSnapshot, want) {
			return fmt.Errorf("SSH resolve lost exact ownership snapshot: %#v", lease.Server)
		}
		replacement := cloneLeaseClaim(want)
		replacement.RepoRoot = "/repo/replacement"
		if err := replaceLeaseClaimIfUnchanged(want.LeaseID, want, replacement); err != nil {
			return err
		}
		called := false
		err = withLeaseClaimUnchanged(want.LeaseID, lease.Server.claimSnapshot, func() error { called = true; return nil })
		if called || err == nil || !strings.Contains(err.Error(), "claim changed") {
			return fmt.Errorf("stale SSH snapshot authorized action: called=%t err=%v", called, err)
		}
		return nil
	})
	assertOwned()
}
