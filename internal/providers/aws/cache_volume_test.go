package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeAWSCacheVolumeCloud struct {
	accountID     string
	nextID        int
	volumes       map[string]core.AWSCacheVolume
	sizes         map[string]int32
	deleted       []string
	failAt        int
	ambiguousAt   int
	attachPending int
	validationErr error
	failCreates   bool
	findErr       error
	describeErr   error
}

func newFakeAWSCacheVolumeCloud() *fakeAWSCacheVolumeCloud {
	return &fakeAWSCacheVolumeCloud{
		accountID: "123456789012",
		volumes:   map[string]core.AWSCacheVolume{},
		sizes:     map[string]int32{},
	}
}

func (f *fakeAWSCacheVolumeCloud) CallerAccountID(context.Context) (string, error) {
	return f.accountID, nil
}

func (f *fakeAWSCacheVolumeCloud) ValidateCacheVolumeInstanceType(context.Context, string) error {
	return f.validationErr
}

func (f *fakeAWSCacheVolumeCloud) CreateCacheVolume(_ context.Context, az string, sizeGB int32, tags map[string]string, _ string) (string, error) {
	f.nextID++
	if f.failCreates || (f.failAt > 0 && f.nextID == f.failAt) {
		return "", fmt.Errorf("injected create failure")
	}
	id := fmt.Sprintf("vol-%08d", f.nextID)
	f.volumes[id] = core.AWSCacheVolume{
		ID:               id,
		State:            core.AWSCacheVolumeAvailable,
		AvailabilityZone: az,
		Encrypted:        true,
		VolumeType:       "gp3",
		SizeGB:           sizeGB,
		Tags:             cloneCacheTags(tags),
	}
	f.sizes[id] = sizeGB
	if f.ambiguousAt > 0 && f.nextID == f.ambiguousAt {
		return "", fmt.Errorf("ambiguous create timeout")
	}
	return id, nil
}

func TestAWSCacheVolumeLifecycleRollsBackAndRecoversDurableReservations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	cloud.failAt = 2
	lifecycle := &awsCacheVolumeLifecycle{}
	_, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          "lease-rollback",
		RepoScope:        "repo-scope",
		Region:           "us-west-2",
		AvailabilityZone: "us-west-2a",
		SSHUser:          "crabbox",
		Volumes: []core.CacheVolumeConfig{
			{Name: "one", Key: "one", Path: "/var/cache/one", SizeGB: 20},
			{Name: "two", Key: "two", Path: "/var/cache/two", SizeGB: 20},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "injected create failure") {
		t.Fatalf("error=%v, want injected second cache creation failure", err)
	}
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var first *awsCacheVolumeRecord
	for index := range registry.Records {
		if registry.Records[index].Name == "one" {
			first = &registry.Records[index]
			break
		}
	}
	if first == nil {
		t.Fatalf("first durable reservation missing: %#v", registry.Records)
	}
	if first.State != core.AWSCacheVolumeAvailable || first.LeaseID != "" {
		t.Fatalf("first reservation was stranded: %#v", first)
	}

	cloud.failAt = 0
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-recover", "us-west-2a")
	id := plan.Bindings[0].VolumeID
	creates := cloud.nextID
	replayed := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-recover", "us-west-2a")
	if replayed.Bindings[0].VolumeID != id || cloud.nextID != creates {
		t.Fatalf("committed reservation replayed=%s id=%s creates=%d want %d", replayed.Bindings[0].VolumeID, id, cloud.nextID, creates)
	}
	registry, path, err = loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	record := findAWSCacheVolumeRecord(registry, id)
	record.VolumeID = ""
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	recovered := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-recover", "us-west-2a")
	if recovered.Bindings[0].VolumeID != id {
		t.Fatalf("durable pending member recovered %s want %s", recovered.Bindings[0].VolumeID, id)
	}
}

func TestAWSCacheVolumeLifecycleRejectsDuplicateBindingsAndNonNitro(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	request := core.AWSCacheVolumePrepareRequest{
		LeaseID: "lease-invalid", RepoScope: "repo", Region: "us-west-2",
		AvailabilityZone: "us-west-2a", ServerType: "m5.large", SSHUser: "crabbox",
		Volumes: []core.CacheVolumeConfig{
			{Name: "one", Key: "one", Path: "/var/cache/shared", SizeGB: 20},
			{Name: "two", Key: "two", Path: "/var/cache/shared", SizeGB: 20},
		},
	}
	if _, err := lifecycle.Prepare(context.Background(), cloud, request); err == nil {
		t.Fatal("duplicate cache mount path was accepted")
	}
	if cloud.nextID != 0 {
		t.Fatal("duplicate cache bindings mutated cloud state")
	}
	request.Volumes = request.Volumes[:1]
	cloud.validationErr = fmt.Errorf("AWS cache volumes require a Nitro/NVMe instance type")
	if _, err := lifecycle.Prepare(context.Background(), cloud, request); err == nil {
		t.Fatal("non-Nitro instance type was accepted")
	}
	cloud.validationErr = nil
	request.Volumes = make([]core.CacheVolumeConfig, awsCacheVolumeMaxBindings+1)
	for index := range request.Volumes {
		request.Volumes[index] = core.CacheVolumeConfig{
			Name: fmt.Sprintf("cache-%d", index), Key: fmt.Sprintf("key-%d", index),
			Path: fmt.Sprintf("/var/cache/cache-%d", index), SizeGB: 20,
		}
	}
	if _, err := lifecycle.Prepare(context.Background(), cloud, request); err == nil {
		t.Fatal("too many cache bindings were accepted")
	}
	request.Volumes = []core.CacheVolumeConfig{{
		Name: "protected", Key: "protected", Path: "/var/cache/protected", SizeGB: 20,
	}}
	request.WorkRoot = "/workspaces"
	for _, path := range []string{
		"/root",
		"/home",
		"/home/crabbox",
		"/home/crabbox/.ssh",
		"/home/crabbox/.ssh/authorized_keys",
		"/var",
		"/var/lib",
		"/var/lib/cloud",
		"/var/lib/cloud/instance",
		"/var/lib/crabbox",
		"/var/lib/crabbox/cache-volumes",
		"/workspaces",
	} {
		request.Volumes[0].Path = path
		if _, err := lifecycle.Prepare(context.Background(), cloud, request); err == nil {
			t.Fatalf("protected mount path %s was accepted", path)
		}
	}
	for _, path := range []string{"/home/crabbox/.cache/build", "/workspaces/.cache/build", "/var/cache/protected"} {
		request.Volumes[0].Path = path
		if err := validateAWSCacheVolumeMountPath(request.Volumes[0], request.SSHUser, request.WorkRoot); err != nil {
			t.Fatalf("safe cache subdirectory %s rejected: %v", path, err)
		}
	}
}

func TestAWSCacheVolumeLifecycleReusesCurrentNameAndPath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	first := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-old-path", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "lease-old-path", false); err != nil {
		t.Fatal(err)
	}
	plan, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          "lease-new-path",
		RepoScope:        "repo-secret",
		Region:           "us-west-2",
		AvailabilityZone: "us-west-2a",
		SSHUser:          "crabbox",
		Volumes: []core.CacheVolumeConfig{{
			Name: "renamed", Key: "cache-secret", Path: "/var/cache/renamed", SizeGB: 20,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Bindings[0].VolumeID != first.Bindings[0].VolumeID ||
		plan.Bindings[0].Name != "renamed" ||
		plan.Bindings[0].Path != "/var/cache/renamed" {
		t.Fatalf("reuse kept stale binding: %#v", plan.Bindings[0])
	}
}

func (f *fakeAWSCacheVolumeCloud) FindCacheVolumes(_ context.Context, az string, tags map[string]string) ([]core.AWSCacheVolume, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	var matches []core.AWSCacheVolume
	for _, volume := range f.volumes {
		if volume.AvailabilityZone == az && cacheTagsContain(volume.Tags, tags) {
			matches = append(matches, volume)
		}
	}
	return matches, nil
}

func (f *fakeAWSCacheVolumeCloud) DescribeCacheVolume(_ context.Context, id string) (core.AWSCacheVolume, error) {
	if f.describeErr != nil {
		return core.AWSCacheVolume{}, f.describeErr
	}
	volume, ok := f.volumes[id]
	if !ok {
		return core.AWSCacheVolume{}, fmt.Errorf("aws cache volume not found: %s", id)
	}
	volume.Attachments = append([]string(nil), volume.Attachments...)
	volume.Tags = cloneCacheTags(volume.Tags)
	return volume, nil
}

func (f *fakeAWSCacheVolumeCloud) AttachCacheVolume(_ context.Context, id, instanceID, _ string) error {
	if f.attachPending > 0 {
		f.attachPending--
		return fmt.Errorf("IncorrectInstanceState: instance is pending")
	}
	volume := f.volumes[id]
	volume.State = core.AWSCacheVolumeAttached
	volume.Attachments = []string{instanceID}
	f.volumes[id] = volume
	return nil
}

func (f *fakeAWSCacheVolumeCloud) DetachCacheVolume(_ context.Context, id, instanceID string) error {
	volume := f.volumes[id]
	if len(volume.Attachments) != 1 || volume.Attachments[0] != instanceID {
		return fmt.Errorf("unexpected attachment")
	}
	volume.State = core.AWSCacheVolumeAvailable
	volume.Attachments = nil
	f.volumes[id] = volume
	return nil
}

func (f *fakeAWSCacheVolumeCloud) DeleteCacheVolume(_ context.Context, id string) error {
	delete(f.volumes, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func TestAWSCacheVolumeLifecycleReuseConcurrencyAndAZ(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}

	first := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-1", "us-west-2a")
	wrongLease := first
	wrongLease.LeaseID = "lease-other"
	if err := lifecycle.Attach(ctx, cloud, wrongLease, "i-wrong"); err == nil {
		t.Fatal("cache reservation attached for the wrong lease")
	}
	cloud.attachPending = 1
	if err := lifecycle.Attach(ctx, cloud, first, "i-1"); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Release(ctx, cloud, "lease-1", false); err != nil {
		t.Fatal(err)
	}
	reused := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-2", "us-west-2a")
	if got, want := reused.Bindings[0].VolumeID, first.Bindings[0].VolumeID; got != want {
		t.Fatalf("sequential reuse volume=%q want %q", got, want)
	}
	busy := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-3", "us-west-2a")
	if busy.Bindings[0].VolumeID == reused.Bindings[0].VolumeID {
		t.Fatal("concurrent reservation reused a busy member")
	}
	otherAZ := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-4", "us-west-2b")
	if otherAZ.Bindings[0].VolumeID == reused.Bindings[0].VolumeID {
		t.Fatal("cache member crossed availability zones")
	}
}

func TestAWSCacheVolumeLifecycleCrashRecoveryPurgeAndExternalRefusal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}

	preInstance := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-crash", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "lease-crash", false); err != nil {
		t.Fatal(err)
	}
	recovered := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-recovered", "us-west-2a")
	if recovered.Bindings[0].VolumeID != preInstance.Bindings[0].VolumeID {
		t.Fatal("pre-instance reservation was not recovered")
	}
	if err := lifecycle.Attach(ctx, cloud, recovered, "i-owned"); err != nil {
		t.Fatal(err)
	}
	id := recovered.Bindings[0].VolumeID
	volume := cloud.volumes[id]
	volume.Attachments = []string{"i-external"}
	cloud.volumes[id] = volume
	if err := lifecycle.Release(ctx, cloud, "lease-recovered", true); err == nil {
		t.Fatal("external attachment was accepted")
	}
	if _, ok := cloud.volumes[id]; !ok {
		t.Fatal("externally attached volume was deleted")
	}

	purge := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-purge", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "lease-purge", true); err != nil {
		t.Fatal(err)
	}
	if _, ok := cloud.volumes[purge.Bindings[0].VolumeID]; ok {
		t.Fatal("purge left the cache volume behind")
	}
}

func TestAWSCacheVolumeLifecycleOpaqueTagsBootstrapABIAndGC(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-abi", "us-west-2a")
	id := plan.Bindings[0].VolumeID

	for key, value := range cloud.volumes[id].Tags {
		joined := key + "=" + value
		for _, forbidden := range []string{"repo-secret", "cache-secret", "/var/cache/build", "lease-abi", cloud.accountID} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("cloud tag leaked %q: %s", forbidden, joined)
			}
		}
	}
	if !strings.Contains(plan.Bootstrap, strings.ReplaceAll(id, "-", "")) ||
		!strings.Contains(plan.Bootstrap, "for cache_wait in $(seq 1 120)") ||
		!strings.Contains(plan.Bootstrap, "mount -o nodev,nosuid") ||
		!strings.Contains(plan.Bootstrap, "readlink -f -- \"$cache_path\"") ||
		!strings.Contains(plan.Bootstrap, "chown 'crabbox:crabbox'") ||
		!strings.Contains(plan.Bootstrap, "mkfs.ext4") {
		t.Fatalf("bootstrap lacks exact serial, hardened mount, or new-volume format path:\n%s", plan.Bootstrap)
	}
	if err := lifecycle.Release(ctx, cloud, "lease-abi", false); err != nil {
		t.Fatal(err)
	}
	reused := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-repair", "us-west-2a")
	if !strings.Contains(reused.Bootstrap, "e2fsck") ||
		!strings.Contains(reused.Bootstrap, "[ ! -f \"$cache_path/.crabbox-cache-abi\" ]") ||
		!strings.Contains(reused.Bootstrap, "umount \"$cache_path\"") ||
		!strings.Contains(reused.Bootstrap, "wipefs") {
		t.Fatalf("reused bootstrap lacks exact-owned corruption reset:\n%s", reused.Bootstrap)
	}
	if err := lifecycle.Release(ctx, cloud, "lease-repair", false); err != nil {
		t.Fatal(err)
	}
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	registry.Records[0].ABIDigest = "obsolete-abi"
	registry.Records[0].UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	volume := cloud.volumes[id]
	volume.Tags["crabbox_cache_abi"] = "obsolete-abi"
	cloud.volumes[id] = volume
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	replacement := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-new-abi", "us-west-2a")
	if replacement.Bindings[0].VolumeID == id || replacement.Bindings[0].Generation <= plan.Bindings[0].Generation {
		t.Fatal("ABI mismatch did not allocate a newer member")
	}
	if err := lifecycle.Release(ctx, cloud, "lease-new-abi", false); err != nil {
		t.Fatal(err)
	}
	registry, path, err = loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for index := range registry.Records {
		registry.Records[index].UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	}
	pendingID := replacement.Bindings[0].VolumeID
	pending := findAWSCacheVolumeRecord(registry, pendingID)
	pending.VolumeID = ""
	pending.State = core.AWSCacheVolumeReserving
	pending.LeaseID = "lease-crashed-before-commit"
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	deleted, err := lifecycle.GarbageCollect(ctx, cloud, "us-west-2", time.Now().Add(-7*24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted=%v want both exact aged records", deleted)
	}

	tagOnlyID, err := cloud.CreateCacheVolume(ctx, "us-west-2a", 20, map[string]string{"crabbox": "true"}, "tag-only")
	if err != nil {
		t.Fatal(err)
	}
	deleted, err = lifecycle.GarbageCollect(ctx, cloud, "us-west-2", time.Now().Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("tag-only discovery was deleted: %v", deleted)
	}
	if _, ok := cloud.volumes[tagOnlyID]; !ok {
		t.Fatal("tag-only volume disappeared")
	}
}

func TestAWSCacheVolumeLifecycleFailedCreatesDoNotConsumeMemberCapacity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	cloud.failCreates = true
	lifecycle := &awsCacheVolumeLifecycle{}
	for index := 0; index < awsCacheVolumeMaxMembers+1; index++ {
		_, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
			LeaseID: "lease-failure-" + fmt.Sprint(index), RepoScope: "repo-failures",
			Region: "us-west-2", AvailabilityZone: "us-west-2a", ServerType: "m5.large",
			SSHUser: "crabbox", WorkRoot: "/workspaces",
			Volumes: []core.CacheVolumeConfig{{
				Name: "build", Key: "failure-key", Path: "/var/cache/build", SizeGB: 20, Required: true,
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "injected create failure") {
			t.Fatalf("failure %d error=%v", index, err)
		}
	}
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for index := range registry.Records {
		if registry.Records[index].State != core.AWSCacheVolumeQuarantined || registry.Records[index].VolumeID != "" {
			t.Fatalf("failed create record counted as live member: %#v", registry.Records[index])
		}
		registry.Records[index].UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	}
	registry.Records[0].State = core.AWSCacheVolumeReserving
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.GarbageCollect(ctx, cloud, "us-west-2", time.Now().Add(-7*24*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	registry, _, err = loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Records) != 0 {
		t.Fatalf("stale zero-provider reservations remained: %#v", registry.Records)
	}
	cloud.failCreates = false
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-recovered-capacity", "us-west-2a")
	if plan.Bindings[0].VolumeID == "" {
		t.Fatal("capacity remained poisoned after repeated create failures")
	}
}

func TestAWSCacheVolumeLifecycleDiscoveryErrorsDoNotConsumeMemberCapacity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	cloud.findErr = fmt.Errorf("injected discovery failure")
	lifecycle := &awsCacheVolumeLifecycle{}
	for index := 0; index < awsCacheVolumeMaxMembers+1; index++ {
		_, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
			LeaseID: "lease-discovery-" + fmt.Sprint(index), RepoScope: "repo-discovery",
			Region: "us-west-2", AvailabilityZone: "us-west-2a", ServerType: "m5.large",
			SSHUser: "crabbox", WorkRoot: "/workspaces",
			Volumes: []core.CacheVolumeConfig{{
				Name: "build", Key: "discovery-key", Path: "/var/cache/build", SizeGB: 20, Required: true,
			}},
		})
		if err == nil || !strings.Contains(err.Error(), "injected discovery failure") {
			t.Fatalf("discovery failure %d error=%v", index, err)
		}
	}
	registry, _, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range registry.Records {
		if record.State != core.AWSCacheVolumeQuarantined ||
			record.VolumeID != "" ||
			record.LastError != "injected discovery failure" ||
			record.LastErrorAt == "" ||
			record.RetryCount == 0 {
			t.Fatalf("discovery error was not durable retry evidence: %#v", record)
		}
	}
	cloud.findErr = nil
	if plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-after-discovery", "us-west-2a"); plan.Bindings[0].VolumeID == "" {
		t.Fatal("discovery errors poisoned member capacity")
	}
}

func TestAWSCacheVolumeLifecycleDescribeErrorsDoNotConsumeMemberCapacity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	first := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-before-describe", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "lease-before-describe", false); err != nil {
		t.Fatal(err)
	}
	cloud.describeErr = fmt.Errorf("injected describe failure")
	_, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID: "lease-describe", RepoScope: "repo-secret",
		Region: "us-west-2", AvailabilityZone: "us-west-2a", ServerType: "m5.large",
		SSHUser: "crabbox", WorkRoot: "/workspaces",
		Volumes: []core.CacheVolumeConfig{{
			Name: "build", Key: "cache-secret", Path: "/var/cache/build", SizeGB: 20, Required: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "injected describe failure") {
		t.Fatalf("describe error=%v", err)
	}
	registry, _, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	record := findAWSCacheVolumeRecord(registry, first.Bindings[0].VolumeID)
	if record == nil || record.State != core.AWSCacheVolumeQuarantined || record.LastError != "injected describe failure" || record.RetryCount == 0 {
		t.Fatalf("describe error was not durable retry evidence: %#v", record)
	}
	cloud.describeErr = nil
	replacement := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-after-describe", "us-west-2a")
	if replacement.Bindings[0].VolumeID == first.Bindings[0].VolumeID {
		t.Fatal("describe-error member was reused")
	}
}

func TestAWSCacheVolumeLifecycleRejectsMutatedTaggedVolumes(t *testing.T) {
	cases := map[string]func(*core.AWSCacheVolume){
		"unencrypted":  func(volume *core.AWSCacheVolume) { volume.Encrypted = false },
		"wrong type":   func(volume *core.AWSCacheVolume) { volume.VolumeType = "io2" },
		"wrong size":   func(volume *core.AWSCacheVolume) { volume.SizeGB++ },
		"multi attach": func(volume *core.AWSCacheVolume) { volume.MultiAttach = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			ctx := context.Background()
			cloud := newFakeAWSCacheVolumeCloud()
			lifecycle := &awsCacheVolumeLifecycle{}
			first := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-original", "us-west-2a")
			if err := lifecycle.Release(ctx, cloud, "lease-original", false); err != nil {
				t.Fatal(err)
			}
			id := first.Bindings[0].VolumeID
			volume := cloud.volumes[id]
			mutate(&volume)
			cloud.volumes[id] = volume
			replacement := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-replacement", "us-west-2a")
			if replacement.Bindings[0].VolumeID == id {
				t.Fatalf("mutated exact-tag volume was reused: %#v", volume)
			}
		})
	}
}

func TestAWSCacheVolumeLifecycleRefusesIncompatibleTagCloneAdoption(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-original", "us-west-2a")
	id := plan.Bindings[0].VolumeID

	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	record := findAWSCacheVolumeRecord(registry, id)
	record.VolumeID = ""
	record.State = core.AWSCacheVolumeReserving
	record.LeaseID = "lease-adopt"
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	volume := cloud.volumes[id]
	volume.VolumeType = "io2"
	cloud.volumes[id] = volume

	_, err = lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          "lease-adopt",
		RepoScope:        "repo-secret",
		Region:           "us-west-2",
		AvailabilityZone: "us-west-2a",
		ServerType:       "m5.large",
		SSHUser:          "crabbox",
		WorkRoot:         "/workspaces",
		Volumes: []core.CacheVolumeConfig{{
			Name: "build", Key: "cache-secret", Path: "/var/cache/build", SizeGB: 20, Required: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "incompatible volume") {
		t.Fatalf("tag clone adoption error=%v", err)
	}
	if _, ok := cloud.volumes[id]; !ok {
		t.Fatal("incompatible exact-tag clone was deleted")
	}
	registry, _, err = loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if registry.Records[0].VolumeID != "" || registry.Records[0].State != core.AWSCacheVolumeQuarantined {
		t.Fatalf("incompatible exact-tag clone was durably adopted: %#v", registry.Records[0])
	}
}

func TestAWSCacheVolumeLifecycleRejectsBlankReleaseAndGCsQuarantinedReservation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-quarantined", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "", true); err == nil {
		t.Fatal("blank lease release was accepted")
	}
	if _, ok := cloud.volumes[plan.Bindings[0].VolumeID]; !ok {
		t.Fatal("blank lease release deleted a cache member")
	}
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	record := findAWSCacheVolumeRecord(registry, plan.Bindings[0].VolumeID)
	record.State = core.AWSCacheVolumeQuarantined
	record.UpdatedAt = time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	deleted, err := lifecycle.GarbageCollect(ctx, cloud, "us-west-2", time.Now().Add(-7*24*time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != plan.Bindings[0].VolumeID {
		t.Fatalf("quarantined reservation GC deleted=%v", deleted)
	}
}

func TestAWSCacheVolumeLifecycleReconcilesDeletionTombstone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}
	plan := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-delete", "us-west-2a")
	registry, path, err := loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	record := findAWSCacheVolumeRecord(registry, plan.Bindings[0].VolumeID)
	record.State = core.AWSCacheVolumeDeleting
	delete(cloud.volumes, record.VolumeID)
	if err := writeAWSCacheVolumeRegistry(path, registry); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.GarbageCollect(ctx, cloud, "us-west-2", time.Now(), false); err != nil {
		t.Fatal(err)
	}
	registry, _, err = loadAWSCacheVolumeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Records) != 0 {
		t.Fatalf("deletion tombstone remained: %#v", registry.Records)
	}
}

func TestAWSCacheVolumeLifecycleReplacesUndersizedAndGCsAmbiguousCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()
	cloud := newFakeAWSCacheVolumeCloud()
	lifecycle := &awsCacheVolumeLifecycle{}

	first := prepareAWSCacheVolumeForTest(t, lifecycle, cloud, "lease-small", "us-west-2a")
	if err := lifecycle.Release(ctx, cloud, "lease-small", false); err != nil {
		t.Fatal(err)
	}
	large, err := lifecycle.Prepare(ctx, cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          "lease-large",
		RepoScope:        "repo-secret",
		Region:           "us-west-2",
		AvailabilityZone: "us-west-2a",
		SSHUser:          "crabbox",
		Volumes: []core.CacheVolumeConfig{{
			Name: "build", Key: "cache-secret", Path: "/var/cache/build", SizeGB: 100, Required: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if large.Bindings[0].VolumeID == first.Bindings[0].VolumeID {
		t.Fatal("undersized cache member was reused")
	}
	if got := cloud.sizes[large.Bindings[0].VolumeID]; got != 100 {
		t.Fatalf("replacement size=%d want 100", got)
	}

	ambiguousCloud := newFakeAWSCacheVolumeCloud()
	ambiguousCloud.ambiguousAt = 1
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ambiguousLifecycle := &awsCacheVolumeLifecycle{}
	_, err = ambiguousLifecycle.Prepare(ctx, ambiguousCloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          "lease-ambiguous",
		RepoScope:        "other-repo",
		Region:           "us-west-2",
		AvailabilityZone: "us-west-2a",
		SSHUser:          "crabbox",
		Volumes: []core.CacheVolumeConfig{{
			Name: "ambiguous", Key: "ambiguous", Path: "/var/cache/ambiguous", SizeGB: 20, Required: true,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous create timeout") {
		t.Fatalf("error=%v", err)
	}
	deleted, err := ambiguousLifecycle.GarbageCollect(ctx, ambiguousCloud, "us-west-2", time.Now().Add(time.Hour), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 {
		t.Fatalf("ambiguous create GC deleted=%v want one exact recovered volume", deleted)
	}
}

func prepareAWSCacheVolumeForTest(t *testing.T, lifecycle *awsCacheVolumeLifecycle, cloud *fakeAWSCacheVolumeCloud, leaseID, az string) core.AWSCacheVolumePlan {
	t.Helper()
	plan, err := lifecycle.Prepare(context.Background(), cloud, core.AWSCacheVolumePrepareRequest{
		LeaseID:          leaseID,
		RepoScope:        "repo-secret",
		Region:           "us-west-2",
		AvailabilityZone: az,
		ServerType:       "m5.large",
		SSHUser:          "crabbox",
		WorkRoot:         "/workspaces",
		Volumes: []core.CacheVolumeConfig{{
			Name:     "build",
			Key:      "cache-secret",
			Path:     "/var/cache/build",
			SizeGB:   20,
			Required: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func cloneCacheTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	for key, value := range tags {
		out[key] = value
	}
	return out
}

func cacheTagsContain(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}
