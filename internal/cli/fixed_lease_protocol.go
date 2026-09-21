package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"
)

// FixedIntentField is an ordered field in a persisted fingerprint schema. Order,
// casing, and omission are part of the legacy hash contract, not JSON semantics.
type FixedIntentField struct {
	Name      string
	Value     any
	OmitEmpty bool
}

type FixedIntentFields []FixedIntentField

func (fields FixedIntentFields) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	first := true
	for _, field := range fields {
		value := reflect.ValueOf(field.Value)
		if field.OmitEmpty && (!value.IsValid() || value.IsZero() || ((value.Kind() == reflect.Slice || value.Kind() == reflect.Map) && value.Len() == 0)) {
			continue
		}
		key, err := json.Marshal(field.Name)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(field.Value)
		if err != nil {
			return nil, err
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(key)
		out.WriteByte(':')
		out.Write(data)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// FixedAdmission describes the persisted evidence that permits submission.
// Inventory absence alone never grants authority to repeat an ambiguous create.
type FixedAdmission struct {
	FreshOnly                bool
	RepeatSameIdentity       bool
	PendingKey, PendingValue string
	SubmittedValue           string
}

func (p FixedAdmission) permits(tx *FixedTransaction) bool {
	if p.RepeatSameIdentity {
		return true
	}
	if p.FreshOnly {
		return tx.Fresh && len(tx.Claim.FixedCreateIntent.Attempt) == 0
	}
	attempt := tx.Claim.FixedCreateIntent.Attempt
	return len(attempt) == 0 || (p.PendingKey != "" && attempt[p.PendingKey] == p.PendingValue)
}

// FixedAttemptPlan contains native creation inputs, never persistence actions.
// Core generates nonces, assembles identity labels, and commits this plan before
// admitting the provider mutation. Existing attempts are never regenerated.
type FixedAttemptPlan struct {
	Values                       map[string]string
	NonceKey                     string
	LowerNonce                   bool
	NoncePrefix                  string
	Labels                       map[string]string
	DirectLabels                 *FixedDirectLabels
	FingerprintLabel, NonceLabel string
	Identity                     FixedResourceBinding
}

type FixedDirectLabels struct {
	Config           Config
	Provider, Market string
	Keep             bool
	Now              time.Time
}

func (tx *FixedTransaction) plan(plan FixedAttemptPlan) error {
	if len(tx.Claim.FixedCreateIntent.Attempt) != 0 {
		return nil
	}
	attempt := maps.Clone(plan.Values)
	if attempt == nil {
		attempt = map[string]string{}
	}
	if plan.NonceKey != "" {
		nonce := rand.Text()
		if plan.LowerNonce {
			nonce = strings.ToLower(nonce)
		}
		attempt[plan.NonceKey] = plan.NoncePrefix + nonce
	}
	tx.Claim.FixedCreateIntent.Attempt = attempt
	labels := maps.Clone(plan.Labels)
	if plan.DirectLabels != nil {
		d := plan.DirectLabels
		now := d.Now
		if now.IsZero() {
			now, _ = time.Parse(time.RFC3339Nano, tx.Claim.FixedCreateIntent.CreatedAt)
		}
		labels = DirectLeaseLabels(d.Config, tx.Claim.LeaseID, tx.Claim.Slug, d.Provider, d.Market, d.Keep, now)
		maps.Copy(labels, plan.Labels)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if plan.FingerprintLabel != "" {
		labels[plan.FingerprintLabel] = tx.Claim.FixedCreateIntent.Fingerprint
	}
	if plan.NonceLabel != "" {
		labels[plan.NonceLabel] = attempt[plan.NonceKey]
	}
	if len(labels) != 0 {
		tx.Claim.Labels = labels
	}
	return tx.applyBinding(plan.Identity)
}

// FixedResourceBinding is evidence supplied by a native operation. Identity is
// monotonic; even a rejected readiness check cannot erase a returned native ID.
type FixedResourceBinding struct {
	CloudID, ImmutableID string
	NumericID            int64
	AttemptIdentityKey   string
	AttemptValues        map[string]string
	Labels               map[string]string
	ProviderScope        string
}

func (tx *FixedTransaction) applyBinding(b FixedResourceBinding) error {
	c := tx.Claim
	if (b.CloudID != "" && c.CloudID != "" && c.CloudID != b.CloudID) ||
		(b.ImmutableID != "" && c.CloudImmutableID != "" && c.CloudImmutableID != b.ImmutableID) ||
		(b.NumericID != 0 && c.CloudNumericID != 0 && c.CloudNumericID != b.NumericID) {
		return Exit(4, "lease_id_conflict: fixed lease %s resource identity changed", c.LeaseID)
	}
	values := maps.Clone(b.AttemptValues)
	if b.AttemptIdentityKey != "" {
		if b.CloudID == "" {
			return Exit(4, "lease_id_conflict: fixed lease %s returned no resource identity; claim retained", c.LeaseID)
		}
		if values == nil {
			values = map[string]string{}
		}
		values[b.AttemptIdentityKey] = b.CloudID
	}
	for key, value := range values {
		if old := c.FixedCreateIntent.Attempt[key]; old != "" && old != value {
			return Exit(4, "lease_id_conflict: fixed lease %s attempt %s changed", c.LeaseID, key)
		}
	}
	if b.CloudID != "" {
		c.CloudID = b.CloudID
	}
	if b.ImmutableID != "" {
		c.CloudImmutableID = b.ImmutableID
	}
	if b.NumericID != 0 {
		c.CloudNumericID = b.NumericID
	}
	if len(values) != 0 {
		if c.FixedCreateIntent.Attempt == nil {
			c.FixedCreateIntent.Attempt = map[string]string{}
		}
		maps.Copy(c.FixedCreateIntent.Attempt, values)
	}
	if b.Labels != nil {
		c.Labels = maps.Clone(b.Labels)
	}
	if b.ProviderScope != "" {
		c.ProviderScope = b.ProviderScope
	}
	return nil
}

func (tx *FixedTransaction) Bind(b FixedResourceBinding) error {
	if err := tx.applyBinding(b); err != nil {
		return err
	}
	return tx.Record("bound")
}

func (tx *FixedTransaction) Admit() error {
	if tx.admission != nil && tx.admission.PendingKey != "" {
		tx.Claim.FixedCreateIntent.Attempt[tx.admission.PendingKey] = tx.admission.SubmittedValue
	}
	return tx.Record("submitting")
}

// FixedAttemptFormat describes a legacy payload envelope. There is one reader:
// adapters provide the schema key, required fields and cardinality constraints.
// Missing old evidence is returned as missing, never synthesized from inventory.
type FixedAttemptFormat struct {
	JSONKey       string
	ExactKeys     int
	Required      []string
	Equal         map[string]string
	OptionalEqual map[string]string
}

func ReadFixedAttempt[T any](intent *FixedCreateIntent, format FixedAttemptFormat) (*T, error) {
	if intent == nil {
		return nil, Exit(4, "lease_id_conflict: missing fixed create intent")
	}
	if len(intent.Attempt) == 0 {
		return nil, nil
	}
	if err := validateFixedJournal(intent); err != nil {
		return nil, err
	}
	var result T
	if format.ExactKeys != 0 && len(intent.Attempt) != format.ExactKeys {
		return nil, Exit(4, "lease_id_conflict: invalid fixed attempt fields")
	}
	data, err := json.Marshal(intent.Attempt)
	if format.JSONKey != "" {
		data = []byte(intent.Attempt[format.JSONKey])
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, Exit(4, "lease_id_conflict: invalid fixed attempt payload: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	stringValue := func(key string) string { var s string; _ = json.Unmarshal(fields[key], &s); return s }
	for _, key := range format.Required {
		if stringValue(key) == "" {
			return nil, Exit(4, "lease_id_conflict: fixed attempt is missing %s", key)
		}
	}
	for key, value := range format.Equal {
		if stringValue(key) != value {
			return nil, Exit(4, "lease_id_conflict: fixed attempt %s changed", key)
		}
	}
	for key, value := range format.OptionalEqual {
		if old := stringValue(key); old != "" && old != value {
			return nil, Exit(4, "lease_id_conflict: fixed attempt %s changed", key)
		}
	}
	return &result, nil
}

func WriteFixedAttempt(intent *FixedCreateIntent, key string, value any, persist func() error) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	intent.Attempt = map[string]string{key: string(data)}
	return persist()
}

// FixedClaimRules validates the local envelope independently of native reads.
// Provider descriptors retain stricter legacy checks without private readers.
type FixedClaimRules struct {
	Kind                                                             FixedLeaseKind
	States                                                           []string
	Scope, IntentScope                                               string
	RequireCanonicalID, RequireSlug, RequireTimestamp, RequireSHA256 bool
	NoCheckpoint, NoFailedAttempts, NoNumericID, SameImmutableID     bool
	EmptyAttemptMustBePristine                                       bool
}

func ValidateFixedClaim(c LeaseClaim, r FixedClaimRules) error {
	i := c.FixedCreateIntent
	if !r.Kind.IsFixedClaim(c) || i.Version != r.Kind.IntentVersion || i.Fingerprint == "" || i.Slug != c.Slug ||
		(len(r.States) != 0 && !slices.Contains(r.States, i.State)) ||
		(r.Scope != "" && c.ProviderScope != r.Scope) || (r.IntentScope != "" && i.ProviderScope != r.IntentScope) ||
		(r.RequireCanonicalID && !IsCanonicalLeaseID(c.LeaseID)) || (r.RequireSlug && i.Slug == "") ||
		(r.NoCheckpoint && i.CheckpointID != "") || (r.NoFailedAttempts && len(i.FailedAttempts) != 0) ||
		(r.NoNumericID && c.CloudNumericID != 0) || (r.SameImmutableID && c.CloudImmutableID != c.CloudID) {
		return Exit(4, "lease_id_conflict: invalid fixed %s identity or provider scope for %s", r.Kind.Label, c.LeaseID)
	}
	if r.RequireTimestamp {
		if _, err := time.Parse(time.RFC3339Nano, i.CreatedAt); err != nil {
			return Exit(4, "lease_id_conflict: invalid fixed create timestamp for %s", c.LeaseID)
		}
	}
	if r.RequireSHA256 && !FixedSHA256(i.Fingerprint) {
		return Exit(4, "lease_id_conflict: invalid fixed intent fingerprint for %s", c.LeaseID)
	}
	if r.EmptyAttemptMustBePristine && len(i.Attempt) == 0 && (i.State != "prepared" || c.CloudID != "" || c.CloudNumericID != 0 || c.CloudImmutableID != "" || len(c.Labels) != 0 || c.SSHHost != "" || c.SSHPort != 0) {
		return Exit(4, "lease_id_conflict: fixed lease %s has no durable attempt", c.LeaseID)
	}
	return validateFixedJournal(i)
}

func FixedSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

// CompleteFixedAcquisition runs acknowledgement outside the claim fence. A
// failed acknowledgement retains custody and cannot reopen a single-use ID.
func CompleteFixedAcquisition(lease LeaseTarget, err error, req AcquireRequest) (LeaseTarget, error) {
	if err != nil {
		return LeaseTarget{}, err
	}
	if req.OnAcquired != nil {
		if err := req.OnAcquired(lease); err != nil {
			return LeaseTarget{}, fmt.Errorf("acknowledge fixed acquisition: %w", err)
		}
	}
	return lease, nil
}

// SelectFixedCandidate classifies a complete native candidate set. Match must
// include suspicious identities; attestation happens only after cardinality.
func SelectFixedCandidate[T any](kind FixedLeaseKind, leaseID string, items []T, match func(T) bool) (T, bool, error) {
	var matches []T
	for _, item := range items {
		if match(item) {
			matches = append(matches, item)
		}
	}
	if err := fixedObservationConflict(kind, leaseID, FixedObservation[T]{Candidates: matches}); err != nil {
		var zero T
		return zero, false, err
	}
	if len(matches) == 0 {
		var zero T
		return zero, false, nil
	}
	return matches[0], true, nil
}

func FixedUncertainCustody(leaseID string) error {
	return Exit(4, "lease_id_conflict: lease %s has an unattested creation attempt; claim, attempt and key retained; inspect the original provider resource before retrying", leaseID)
}
