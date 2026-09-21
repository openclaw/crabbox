package cli

import (
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"testing"
	"time"
)

func TestFixedIntentFieldsPreserveLegacyEncoding(t *testing.T) {
	for _, nilSlice := range []bool{false, true} {
		items := []string{}
		if nilSlice {
			items = nil
		}
		legacy := struct {
			Name     string `json:"nativeName"`
			Optional string `json:"optional,omitempty"`
			TTL      time.Duration
			Items    []string
			Volumes  []string `json:"volumes,omitempty"`
		}{Name: "<fixture>&", TTL: time.Minute, Items: items}
		fields := FixedIntentFields{{Name: "nativeName", Value: legacy.Name}, {Name: "optional", Value: "", OmitEmpty: true}, {Name: "TTL", Value: legacy.TTL}, {Name: "Items", Value: items}, {Name: "volumes", Value: []string{}, OmitEmpty: true}}
		before, _ := json.Marshal(legacy)
		after, err := json.Marshal(fields)
		if err != nil || string(before) != string(after) {
			t.Fatalf("legacy hash payload changed: %s => %s (%v)", before, after, err)
		}
	}
}

func TestFixedProtocolPreservesAdmissionAndBindingEvidence(t *testing.T) {
	claim := LeaseClaim{LeaseID: "cbx_abcdef123456", Provider: "fixture", Slug: "fixture", FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: "hash", Slug: "fixture", State: "prepared"}}
	writes := 0
	tx, err := newFixedTransaction(&claim, true, func() error { writes++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	tx.admission = &FixedAdmission{PendingKey: "submitted", PendingValue: "no", SubmittedValue: "yes"}
	plan := FixedAttemptPlan{Values: map[string]string{"name": "fixture", "submitted": "no"}, NonceKey: "nonce", FingerprintLabel: "hash", NonceLabel: "attempt"}
	if err := tx.plan(plan); err != nil {
		t.Fatal(err)
	}
	original := maps.Clone(claim.FixedCreateIntent.Attempt)
	if err := tx.plan(plan); err != nil || !maps.Equal(original, claim.FixedCreateIntent.Attempt) {
		t.Fatal("replay regenerated an attempt")
	}
	if !tx.admission.permits(tx) {
		t.Fatal("durable pending attempt refused")
	}
	if err := tx.Admit(); err != nil {
		t.Fatal(err)
	}
	if tx.admission.permits(tx) {
		t.Fatal("submitted attempt admitted twice")
	}
	if err := tx.Bind(FixedResourceBinding{CloudID: "native-id", ImmutableID: "generation", AttemptIdentityKey: "native-id"}); err != nil {
		t.Fatal(err)
	}
	prior := writes
	if err := tx.Bind(FixedResourceBinding{CloudID: "replacement", ImmutableID: "generation"}); err == nil || writes != prior || claim.CloudID != "native-id" {
		t.Fatal("binding was retargeted")
	}
	if claim.FixedCreateIntent.Attempt["native-id"] != "native-id" || claim.Labels["attempt"] != original["nonce"] {
		t.Fatal("native evidence lost")
	}
}

func TestFixedAttemptReaderPreservesLegacyPayload(t *testing.T) {
	type attempt struct {
		Name string `json:"name"`
		ID   string `json:"id,omitempty"`
	}
	legacy := &FixedCreateIntent{Attempt: map[string]string{"legacy": `{"name":"fixture","id":"original"}`}}
	format := FixedAttemptFormat{JSONKey: "legacy", ExactKeys: 1, Required: []string{"name"}, Equal: map[string]string{"name": "fixture"}}
	got, err := ReadFixedAttempt[attempt](legacy, format)
	if err != nil || !reflect.DeepEqual(got, &attempt{Name: "fixture", ID: "original"}) || legacy.Journal != nil {
		t.Fatalf("legacy read changed evidence: %+v %v", got, err)
	}
	format.Equal["name"] = "other"
	if _, err := ReadFixedAttempt[attempt](legacy, format); err == nil {
		t.Fatal("accepted changed attempt")
	}
	original := errors.New("durability failure")
	if err := WriteFixedAttempt(legacy, "legacy", got, func() error { return original }); !errors.Is(err, original) {
		t.Fatal(err)
	}
}

func TestFixedBindingCASPreservesExpectedJournal(t *testing.T) {
	isolateTestUserDirs(t)
	const id = "cbx_abcdef123455"
	err := WithDurableLeaseClaimLock(id, func(claim *LeaseClaim, _ bool, persist func() error) error {
		*claim = LeaseClaim{LeaseID: id, Provider: "fixture", Slug: "fixture", FixedCreateIntent: &FixedCreateIntent{Version: 1, Fingerprint: "hash", Slug: "fixture", State: "prepared", Attempt: map[string]string{"name": "fixture"}, Journal: &FixedLeaseJournal{Version: 1, Phase: "prepared", Revision: 1}}}
		return persist()
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ReadLeaseClaim(id)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(claim *LeaseClaim, persist func() error) error {
		return BindFixedClaim(claim, FixedResourceBinding{CloudID: "native-id", ImmutableID: "generation", AttemptIdentityKey: "native-id"}, persist)
	}
	bound, err := CompareAndBindFixedClaim(expected, bind)
	if err != nil || bound.CloudID != "native-id" {
		t.Fatalf("recovery binding failed: %+v %v", bound, err)
	}
	if expected.CloudID != "" || expected.FixedCreateIntent.Journal.Phase != "prepared" || expected.FixedCreateIntent.Attempt["native-id"] != "" {
		t.Fatal("CAS expected record was mutated")
	}
	if _, err := CompareAndBindFixedClaim(expected, bind); err == nil {
		t.Fatal("stale expected claim crossed the CAS fence")
	}
	current, err := ReadLeaseClaim(id)
	if err != nil || !reflect.DeepEqual(current, bound) {
		t.Fatalf("stale bind changed custody: %v", err)
	}
}
