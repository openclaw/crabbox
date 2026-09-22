package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
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
	KeyedRetry               *FixedKeyedRetry
	FreshOnly                bool
	RepeatSameIdentity       bool
	PendingKey, PendingValue string
	SubmittedValue           string
}

// FixedKeyedRetry permits one recovery submission of an unbound attempt under
// a provider's expiring idempotency key. The engine owns its clock and counter.
type FixedKeyedRetry struct {
	AttemptKey string
	Window     time.Duration
}

func (p FixedAdmission) permits(tx *FixedTransaction) bool {
	if retry := p.KeyedRetry; retry != nil {
		if !p.FreshOnly || p.RepeatSameIdentity || retry.AttemptKey == "" || retry.Window <= 0 {
			return false
		}
		if tx.fresh && len(tx.Claim.FixedCreateIntent.Attempt) == 0 {
			return true
		}
		j := tx.Claim.FixedCreateIntent.Journal
		if j == nil || j.Submission == nil || j.Submission.Count != 1 || j.Submission.Key != tx.Claim.FixedCreateIntent.Attempt[retry.AttemptKey] {
			return false
		}
		first, err := time.Parse(time.RFC3339Nano, j.Submission.FirstSubmittedAt)
		now := tx.now()
		return err == nil && !now.Before(first) && now.Before(first.Add(retry.Window))
	}
	if p.RepeatSameIdentity {
		return true
	}
	if p.FreshOnly {
		return tx.fresh && len(tx.Claim.FixedCreateIntent.Attempt) == 0
	}
	attempt := tx.Claim.FixedCreateIntent.Attempt
	return len(attempt) == 0 || (p.PendingKey != "" && attempt[p.PendingKey] == p.PendingValue)
}

// FixedAttemptPlan contains native creation inputs, never persistence actions.
// Core generates nonces, assembles identity labels, and commits this plan before
// admitting the provider mutation. Existing attempts are never regenerated.
type FixedAttemptPlan struct {
	OwnerLabel                   string
	PrivateLabels                map[string]string
	UniqueLabel                  string
	UniqueProviders              []string
	AttemptLabels                map[string]string
	JSONKey                      string
	Payload                      any
	NonceBytes                   int
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
		if plan.NonceBytes > 0 {
			data := make([]byte, plan.NonceBytes)
			if _, err := rand.Read(data); err != nil {
				return err
			}
			nonce = hex.EncodeToString(data)
		}
		if plan.LowerNonce {
			nonce = strings.ToLower(nonce)
		}
		attempt[plan.NonceKey] = plan.NoncePrefix + nonce
	}
	if plan.JSONKey != "" {
		data, err := json.Marshal(plan.Payload)
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for key, value := range attempt {
			data, _ := json.Marshal(value)
			fields[key] = data
		}
		data, err = json.Marshal(fields)
		if err != nil {
			return err
		}
		tx.Claim.FixedCreateIntent.Attempt = map[string]string{plan.JSONKey: string(data)}
	} else {
		tx.Claim.FixedCreateIntent.Attempt = attempt
	}
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
	if plan.OwnerLabel != "" {
		labels[plan.OwnerLabel] = tx.Claim.Provider
	}
	if plan.FingerprintLabel != "" {
		labels[plan.FingerprintLabel] = tx.Claim.FixedCreateIntent.Fingerprint
	}
	if plan.NonceLabel != "" {
		labels[plan.NonceLabel] = attempt[plan.NonceKey]
	}
	for label, key := range plan.AttemptLabels {
		labels[label] = attempt[key]
	}
	tx.createLabels = maps.Clone(labels)
	maps.Copy(labels, plan.PrivateLabels)
	if len(labels) != 0 {
		tx.Claim.Labels = labels
	}
	if err := tx.applyBinding(plan.Identity); err != nil {
		return err
	}
	if len(plan.UniqueProviders) != 0 {
		return ValidateFixedLocalClaimUniqueness(FixedLeaseKind{ClaimProvider: tx.Claim.Provider, Label: plan.UniqueLabel}, *tx.Claim, plan.UniqueProviders...)
	}
	return nil
}

// FixedResourceBinding is evidence supplied by a native operation. Identity is
// monotonic; even a rejected readiness check cannot erase a returned native ID.
type FixedResourceBinding struct {
	ImageEvidence        *ImageEvidence
	SSH                  *SSHTarget
	AttemptJSONKey       string
	OnlyUnbound          bool
	FingerprintLabel     string
	CloudID, ImmutableID string
	NumericID            int64
	AttemptIdentityKey   string
	AttemptValues        map[string]string
	Labels               map[string]string
	ProviderScope        string
}

func (tx *FixedTransaction) applyBinding(b FixedResourceBinding) error {
	c := tx.Claim
	if b.OnlyUnbound && c.CloudID != "" {
		return nil
	}
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
	attempt := c.FixedCreateIntent.Attempt
	var payload map[string]json.RawMessage
	if b.AttemptJSONKey != "" {
		if err := json.Unmarshal([]byte(attempt[b.AttemptJSONKey]), &payload); err != nil {
			return Exit(4, "lease_id_conflict: invalid fixed attempt payload")
		}
		attempt = map[string]string{}
		for key, data := range payload {
			var value string
			if json.Unmarshal(data, &value) == nil {
				attempt[key] = value
			}
		}
	}
	for key, value := range values {
		if old := attempt[key]; old != "" && old != value {
			return Exit(4, "lease_id_conflict: fixed lease %s attempt %s changed", c.LeaseID, key)
		}
	}

	cloudID, numericID, immutableID := c.CloudID, c.CloudNumericID, c.CloudImmutableID
	if b.CloudID != "" {
		cloudID = b.CloudID
	}
	if b.NumericID != 0 {
		numericID = b.NumericID
	}
	if b.ImmutableID != "" {
		immutableID = b.ImmutableID
	}
	SetLeaseClaimResourceIdentity(c, cloudID, numericID, immutableID, b.ImageEvidence)
	if b.SSH != nil {
		c.SSHHost = b.SSH.Host
		if port, err := strconv.Atoi(strings.TrimSpace(b.SSH.Port)); err == nil && port > 0 {
			c.SSHPort = port
		}
	}

	if len(values) != 0 {
		if c.FixedCreateIntent.Attempt == nil {
			c.FixedCreateIntent.Attempt = map[string]string{}
		}
		if b.AttemptJSONKey != "" {
			for key, value := range values {
				data, _ := json.Marshal(value)
				payload[key] = data
			}
			data, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			c.FixedCreateIntent.Attempt[b.AttemptJSONKey] = string(data)
		} else {
			maps.Copy(c.FixedCreateIntent.Attempt, values)
		}
	}
	if b.Labels != nil {
		c.Labels = maps.Clone(b.Labels)
	}
	if b.FingerprintLabel != "" {
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		c.Labels[b.FingerprintLabel] = c.FixedCreateIntent.Fingerprint
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
	if tx.admission != nil && tx.admission.KeyedRetry != nil {
		intent := tx.Claim.FixedCreateIntent
		key := intent.Attempt[tx.admission.KeyedRetry.AttemptKey]
		if key == "" {
			return FixedUncertainCustody(tx.Claim.LeaseID)
		}
		var submission FixedKeyedSubmission
		if intent.Journal.Submission == nil {
			if !tx.fresh {
				return FixedUncertainCustody(tx.Claim.LeaseID)
			}
			submission = FixedKeyedSubmission{Key: key, FirstSubmittedAt: tx.now().UTC().Format(time.RFC3339Nano), Count: 1}
		} else {
			if !tx.admission.permits(tx) {
				return FixedUncertainCustody(tx.Claim.LeaseID)
			}
			submission = *intent.Journal.Submission
			submission.Count++
		}
		intent.Journal.Submission = &submission
	}
	if tx.admission != nil && tx.admission.PendingKey != "" {
		tx.Claim.FixedCreateIntent.Attempt[tx.admission.PendingKey] = tx.admission.SubmittedValue
	}
	return tx.Record("submitting")
}

// FixedAttemptFormat describes a legacy payload envelope. There is one reader:
// adapters provide the schema key, required fields and cardinality constraints.
// Missing old evidence is returned as missing, never synthesized from inventory.
type FixedAttemptFormat struct {
	RejectEmptyObject bool
	PositiveIntegers  []string
	Trimmed, SHA256   []string
	JSONKey           string
	ExactKeys         int
	Required          []string
	Equal             map[string]string
	OptionalEqual     map[string]string
}

func ReadFixedAttempt[T any](intent *FixedCreateIntent, format FixedAttemptFormat) (*T, error) {
	if intent == nil {
		return nil, Exit(4, "lease_id_conflict: missing fixed create intent")
	}
	if err := validateFixedJournal(intent); err != nil {
		return nil, err
	}
	if len(intent.Attempt) == 0 {
		if intent.Attempt != nil && format.RejectEmptyObject {
			return nil, Exit(4, "lease_id_conflict: empty fixed attempt has no creation identity; claim retained")
		}
		return nil, nil
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
	for _, key := range format.PositiveIntegers {
		value := stringValue(key)
		number, err := strconv.Atoi(value)
		if err != nil || number <= 0 || strconv.Itoa(number) != value {
			return nil, Exit(4, "lease_id_conflict: fixed attempt %s is not a canonical positive integer", key)
		}
	}
	for _, key := range format.Trimmed {
		if value := stringValue(key); value != strings.TrimSpace(value) {
			return nil, Exit(4, "lease_id_conflict: fixed attempt %s is not canonical", key)
		}
	}
	for _, key := range format.SHA256 {
		if !FixedSHA256(stringValue(key)) {
			return nil, Exit(4, "lease_id_conflict: fixed attempt %s is not a canonical digest", key)
		}
	}
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
	RequireIntentScope, NumericMatchesID, GenerationAfterPrepared    bool
	ExpectedID                                                       string
	RequiredLabels                                                   []string
	BoundLabels                                                      map[string]string
	RequireBound, UnboundMustBeBare                                  bool
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
		(r.IntentScope != "" && i.ProviderScope != r.IntentScope) ||
		(r.RequireCanonicalID && !IsCanonicalLeaseID(c.LeaseID)) || (r.RequireSlug && i.Slug == "") ||
		(r.NoCheckpoint && i.CheckpointID != "") || (r.NoFailedAttempts && len(i.FailedAttempts) != 0) {
		return Exit(4, "lease_id_conflict: invalid fixed %s identity or provider scope for %s", r.Kind.Label, c.LeaseID)
	}
	if (r.Scope != "" && c.ProviderScope != r.Scope) || (r.NoNumericID && c.CloudNumericID != 0) || (r.SameImmutableID && c.CloudImmutableID != c.CloudID) {
		return Exit(4, "lease_id_conflict: fixed %s lease %s has inconsistent immutable identity or provider scope", r.Kind.Label, c.LeaseID)
	}
	if (r.RequireIntentScope && i.ProviderScope == "") || (r.ExpectedID != "" && c.CloudID != r.ExpectedID) ||
		(r.NumericMatchesID && c.CloudID != strconv.FormatInt(c.CloudNumericID, 10)) ||
		(r.GenerationAfterPrepared && i.State != "prepared" && c.CloudImmutableID == "") {
		return Exit(4, "lease_id_conflict: fixed lease %s has inconsistent durable identity", c.LeaseID)
	}
	for _, key := range r.RequiredLabels {
		if c.Labels[key] == "" {
			return Exit(4, "lease_id_conflict: fixed lease %s has no durable %s", c.LeaseID, key)
		}
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
	if r.RequireBound && c.CloudID == "" {
		return Exit(4, "lease_id_conflict: fixed lease %s has no bound identity", c.LeaseID)
	}
	if c.CloudID == "" && r.UnboundMustBeBare && (i.State == "acquired" || len(c.Labels) != 0 || c.SSHHost != "" || c.SSHPort != 0) {
		return Exit(4, "lease_id_conflict: fixed lease %s has inconsistent unbound identity", c.LeaseID)
	}
	if c.CloudID != "" {
		for key, value := range r.BoundLabels {
			if c.Labels[key] != value {
				return Exit(4, "lease_id_conflict: fixed lease %s has inconsistent bound label %s", c.LeaseID, key)
			}
		}
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
	return Exit(4, "lease_id_conflict: lease %s has an unresolved or unattested creation attempt; no replacement allocated; claim, attempt and key retained; inspect the original provider resource before retrying", leaseID)
}

func BindFixedClaim(claim *LeaseClaim, binding FixedResourceBinding, persist func() error) error {
	if binding.OnlyUnbound && claim.CloudID != "" {
		return nil
	}
	tx, err := newFixedTransaction(claim, false, persist)
	if err != nil {
		return err
	}
	// The caller owns the publication phase: acquisition journals the bind,
	// while a resolution CAS preserves the existing create-intent receipt.
	if err := tx.applyBinding(binding); err != nil {
		return err
	}
	return persist()
}

// ValidateFixedLocalClaimUniqueness rejects another local owner, including an
// owner with unknown scope. A terminal record no longer owns a live resource.
func ValidateFixedLocalClaimUniqueness(kind FixedLeaseKind, claim LeaseClaim, providers ...string) error {
	claims, err := ListLeaseClaims()
	if err != nil {
		return err
	}
	for _, other := range claims {
		if other.LeaseID == claim.LeaseID || other.CloudID != claim.CloudID || !slices.Contains(providers, other.Provider) {
			continue
		}
		if other.ProviderScope != "" && other.ProviderScope != claim.ProviderScope {
			continue
		}
		if kind.IsFixedClaim(other) && other.FixedCreateIntent.State == "released" {
			continue
		}
		return Exit(4, "lease_id_conflict: multiple local %s claims bind resource %s", kind.Label, claim.CloudID)
	}
	return nil
}

func FixedIdentityLabels(provider, leaseID, slug, fingerprint string, native map[string]string) map[string]string {
	labels := map[string]string{"crabbox": "true", "provider": provider, "lease": leaseID, "slug": slug, "provider_key": ProviderKeyForLease(leaseID), "fixed_intent_sha256": fingerprint}
	maps.Copy(labels, native)
	return labels
}

// PrepareFixedSSHKey preserves the stricter replay-key contract selected by
// each format before obtaining a key. It never recreates a required lost key.
type FixedKeyPolicy struct{ RequireExisting, UseStored, PreserveProviderKey bool }

func PrepareFixedSSHKey(cfg *Config, leaseID string, policy FixedKeyPolicy) (string, error) {
	if policy.RequireExisting {
		target := SSHTarget{}
		if err := UseStoredTestboxKey(&target, leaseID); err != nil {
			return "", err
		}
		if policy.UseStored {
			cfg.SSHKey = target.Key
		}
	}
	keyPath, publicKey, err := EnsureTestboxKeyForConfig(*cfg, leaseID)
	if err != nil {
		return "", err
	}
	cfg.SSHKey = keyPath
	if !policy.PreserveProviderKey {
		cfg.ProviderKey = ProviderKeyForLease(leaseID)
	}
	return publicKey, nil
}

func (k FixedLeaseKind) ReadTerminal(leaseID string) (bool, error) {
	claim, exists, err := ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		return true, err
	}
	if !exists || !k.IsFixedClaim(claim) || claim.FixedCreateIntent.State != "released" {
		return false, nil
	}
	return true, k.ValidateTerminalClaim(claim, claim, leaseID, nil)
}

func (k FixedLeaseKind) ResolveTerminal(claim LeaseClaim, releaseOnly bool) (LeaseTarget, bool, error) {
	if claim.FixedCreateIntent.State != "released" {
		return LeaseTarget{}, false, nil
	}
	if !releaseOnly {
		return LeaseTarget{}, true, Exit(4, "%s fixed lease is terminal", k.Label)
	}
	return LeaseTarget{LeaseID: claim.LeaseID}, true, k.ValidateTerminalClaim(claim, claim, claim.LeaseID, nil)
}

func (tx *FixedTransaction) CreateLabels() map[string]string { return maps.Clone(tx.createLabels) }

func FixedCreateTime(claim LeaseClaim) time.Time {
	created, _ := time.Parse(time.RFC3339Nano, claim.FixedCreateIntent.CreatedAt)
	return created
}

// Observe retains partial native evidence before further attestation or errors.
func (tx *FixedTransaction) Observe(binding FixedResourceBinding) error {
	if err := tx.applyBinding(binding); err != nil {
		return err
	}
	return tx.Record("observed")
}

// FixedPristineRecord recognizes formats that never erased submitted attempts.
// Other formats must use fresh-invocation admission instead of this predicate.
func FixedPristineRecord(claim LeaseClaim, kind FixedLeaseKind, scopePrefix string) bool {
	intent := claim.FixedCreateIntent
	if !kind.IsFixedClaim(claim) || intent.Version != kind.IntentVersion || intent.State != "prepared" ||
		!IsCanonicalLeaseID(claim.LeaseID) || intent.Slug == "" || claim.Slug != intent.Slug || claim.ProviderScope != intent.ProviderScope ||
		len(intent.Attempt) != 0 || len(intent.FailedAttempts) != 0 || len(claim.Labels) != 0 ||
		claim.CloudID != "" || claim.CloudImmutableID != "" || claim.CloudNumericID != 0 || claim.SSHHost != "" || claim.SSHPort != 0 ||
		claim.StaticHost != "" || claim.StaticUser != "" || claim.StaticPort != "" || claim.StaticWorkRoot != "" {
		return false
	}
	if j := intent.Journal; j != nil && (j.Version != 1 || j.Revision == 0 || j.Phase != "prepared") {
		return false
	}
	scope, ok := strings.CutPrefix(intent.ProviderScope, scopePrefix)
	scopeHash, scopeErr := hex.DecodeString(scope)
	fingerprint, fingerprintErr := hex.DecodeString(intent.Fingerprint)
	_, timeErr := time.Parse(time.RFC3339Nano, intent.CreatedAt)
	return ok && scopeErr == nil && len(scopeHash) == 32 && fingerprintErr == nil && len(fingerprint) == 32 && timeErr == nil
}

func CheckFixedAttemptActive(claim LeaseClaim, identityKey string, cleanupKeys ...string) error {
	intent := claim.FixedCreateIntent
	if intent == nil || intent.State == "released" || intent.Attempt[identityKey] == "" {
		return Exit(4, "fixed lease %s has no active create attempt; it cannot allocate a replacement", claim.LeaseID)
	}
	for _, key := range cleanupKeys {
		if intent.Attempt[key] != "" {
			return Exit(4, "fixed lease %s has entered cleanup and cannot be reused; retry stop to reconcile deletion", claim.LeaseID)
		}
	}
	return nil
}

// CloneLeaseClaim snapshots all custody and access evidence, including journals.
func CloneLeaseClaim(claim LeaseClaim) LeaseClaim {
	claim = cloneLeaseClaim(claim)
	if claim.FixedCreateIntent != nil && claim.FixedCreateIntent.Journal != nil {
		journal := *claim.FixedCreateIntent.Journal
		if journal.Submission != nil {
			submission := *journal.Submission
			journal.Submission = &submission
		}
		claim.FixedCreateIntent.Journal = &journal
	}
	return claim
}

// CompareAndBindFixedClaim keeps the expected CAS record separate from the
// journal the binding callback advances. A shallow intent copy loses that fence.
func CompareAndBindFixedClaim(claim LeaseClaim, bind func(*LeaseClaim, func() error) error) (LeaseClaim, error) {
	expected := CloneLeaseClaim(claim)
	claim = CloneLeaseClaim(claim)
	err := bind(&claim, func() error {
		var err error
		claim, err = ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, expected, claim)
		return err
	})
	return claim, err
}

func (k FixedLeaseKind) RetainMatchingFingerprint(leaseID string, previous LeaseClaim, fingerprint string) (bool, error) {
	return k.RetainClaimAfterRelease(leaseID, previous, fingerprint != "", nil, func(claim LeaseClaim) error {
		if fingerprint != "" && fingerprint != claim.FixedCreateIntent.Fingerprint {
			return Exit(4, "lease_id_conflict: fixed lease %s resource label differs from its terminal tombstone", leaseID)
		}
		return nil
	})
}

// FixedReadinessRecovery keeps uncertainty and Keep policy in core while native
// callbacks prove exact rollback and reconcile runtime-specific residue.
type FixedReadinessRecovery struct {
	Expected               LeaseClaim
	Keep, IdentityConflict bool
	Prepare                func()
	RollbackExact          func() error
	Reconcile              func() (bool, error)
}

func ReconcileFixedReadiness(p FixedReadinessRecovery) (bool, error) {
	if p.IdentityConflict {
		if err := VerifyLeaseClaimUnchanged(p.Expected.LeaseID, p.Expected); err != nil {
			return false, err
		}
		return true, nil
	}
	if p.Prepare != nil {
		p.Prepare()
	}
	if !p.Keep {
		rollbackErr := p.RollbackExact()
		if rollbackErr == nil {
			return false, nil
		}
		retained, reconcileErr := p.Reconcile()
		return retained, errors.Join(rollbackErr, reconcileErr)
	}
	return p.Reconcile()
}

// RecordFixedWitness stores a native acknowledgement without allowing a known
// witness to be retargeted. The owning transaction supplies the journal fence.
func RecordFixedWitness(claim *LeaseClaim, key, value string, persist func() error) error {
	if claim.FixedCreateIntent == nil || claim.FixedCreateIntent.Attempt == nil || key == "" || value == "" {
		return Exit(4, "lease_id_conflict: incomplete fixed attempt witness")
	}
	if old := claim.FixedCreateIntent.Attempt[key]; old != "" && old != value {
		return Exit(4, "lease_id_conflict: fixed attempt witness %s changed", key)
	}
	claim.FixedCreateIntent.Attempt[key] = value
	return persist()
}

func (tx *FixedTransaction) PersistDeletionEvidence() error { return tx.Record("deleting") }
