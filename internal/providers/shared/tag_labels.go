package shared

import (
	"slices"
	"sort"
	"strings"
)

type tagMerge uint8

const (
	tagLast tagMerge = iota
	tagOwnership
	tagState
	tagTimestamp
)

// TagLabelField extends the logical lease schema, not a provider's wire format.
type TagLabelField struct {
	Key       string
	Lowercase bool
	Exact     bool
	merge     tagMerge
}

type TagLabelSchema struct {
	keys   []string
	fields map[string]TagLabelField
}

// LeaseTagSchema owns the common metadata meaning. Adapters opt into additional
// fields and retain tag limits, escaping, version decoding, and resource checks.
func LeaseTagSchema(extra ...TagLabelField) TagLabelSchema {
	fields := []TagLabelField{
		{Key: "provider", Lowercase: true, merge: tagOwnership},
		{Key: "lease", merge: tagOwnership}, {Key: "slug", merge: tagOwnership},
		{Key: "state", Lowercase: true, merge: tagState}, {Key: "keep", Lowercase: true},
		{Key: "target", Lowercase: true, merge: tagOwnership},
		{Key: "class"}, {Key: "server_type"}, {Key: "provider_key"},
		{Key: "ttl_secs"}, {Key: "idle_timeout"}, {Key: "idle_timeout_secs"},
		{Key: "expires_at", merge: tagTimestamp}, {Key: "created_at"},
		{Key: "last_touched_at", merge: tagTimestamp}, {Key: "updated_at"},
		{Key: "profile"}, {Key: "market"}, {Key: "desktop"}, {Key: "desktop_env"},
		{Key: "browser"}, {Key: "code"}, {Key: "pond"}, {Key: "crabbox_exposed_ports"},
	}
	schema := TagLabelSchema{fields: make(map[string]TagLabelField, len(fields)+len(extra))}
	for _, field := range append(fields, extra...) {
		if _, exists := schema.fields[field.Key]; exists {
			panic("duplicate lease tag field: " + field.Key)
		}
		schema.fields[field.Key] = field
		if field.Key != "provider" {
			schema.keys = append(schema.keys, field.Key)
		}
	}
	return schema
}

func TailscaleTagFields() []TagLabelField {
	return []TagLabelField{
		{Key: "tailscale", Lowercase: true}, {Key: "tailscale_state", Lowercase: true},
		{Key: "tailscale_hostname", Exact: true}, {Key: "tailscale_tags", Exact: true},
		{Key: "tailscale_ipv4", Exact: true}, {Key: "tailscale_fqdn", Exact: true},
		{Key: "tailscale_error", Exact: true}, {Key: "tailscale_exit_node", Exact: true},
		{Key: "tailscale_exit_node_allow_lan_access", Lowercase: true},
	}
}

func (s TagLabelSchema) Keys() []string { return slices.Clone(s.keys) }

func (s TagLabelSchema) Exact(key string) bool { return s.fields[key].Exact }

type TagLabelReducer struct {
	schema    TagLabelSchema
	labels    map[string]string
	conflicts map[string]bool
}

func (s TagLabelSchema) Reducer() *TagLabelReducer {
	return &TagLabelReducer{schema: s, labels: map[string]string{}, conflicts: map[string]bool{}}
}

func (r *TagLabelReducer) MarkOwned() {
	r.labels["crabbox"], r.labels["created_by"] = "true", "crabbox"
}

// Apply receives a decoded logical field. Unknown fields never grant authority.
func (r *TagLabelReducer) Apply(key, value string) {
	field, ok := r.schema.fields[key]
	if !ok {
		return
	}
	if field.Lowercase {
		value = strings.ToLower(value)
	}
	switch field.merge {
	case tagOwnership:
		recordUniqueTagValue(r.labels, r.conflicts, key, value)
	case tagState:
		if tagStatePriority(value) >= tagStatePriority(r.labels[key]) {
			r.labels[key] = value
		}
	case tagTimestamp:
		if numericTagValue(value) >= numericTagValue(r.labels[key]) {
			r.labels[key] = value
		}
	default:
		r.labels[key] = value
	}
}

// Overlay replaces a legacy value but never clears an earlier identity conflict.
// Incomplete or conflicting encoded ownership fields remain permanently rejected.
func (r *TagLabelReducer) Overlay(key, value string, conflict bool) {
	delete(r.labels, key)
	if !conflict {
		r.Apply(key, value)
	} else if r.schema.fields[key].merge == tagOwnership {
		r.conflicts[key] = true
	}
}

func (r *TagLabelReducer) Finish(conflictLabel string) map[string]string {
	if len(r.conflicts) > 0 {
		keys := make([]string, 0, len(r.conflicts))
		for key := range r.conflicts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		r.labels[conflictLabel] = strings.Join(keys, ",")
	}
	return r.labels
}

// TagValueSet reduces one encoding generation. A conflict is sticky and counts
// as present, so a broken newer value cannot fall back to an older generation.
type TagValueSet struct {
	values    map[string]string
	conflicts map[string]bool
}

func (s *TagValueSet) Record(key, value string) {
	if s.values == nil {
		s.values = map[string]string{}
	}
	if s.conflicts == nil {
		s.conflicts = map[string]bool{}
	}
	recordUniqueTagValue(s.values, s.conflicts, key, value)
}

func (s *TagValueSet) Reject(key string) {
	if s.conflicts == nil {
		s.conflicts = map[string]bool{}
	}
	delete(s.values, key)
	s.conflicts[key] = true
}

func (s *TagValueSet) Get(key string) (value string, present, conflict bool) {
	value, present = s.values[key]
	conflict = s.conflicts[key]
	return value, present || conflict, conflict
}

func recordUniqueTagValue(values map[string]string, conflicts map[string]bool, key, value string) {
	if conflicts[key] {
		return
	}
	if existing, ok := values[key]; ok && existing != value {
		delete(values, key)
		conflicts[key] = true
		return
	}
	values[key] = value
}

func tagStatePriority(state string) int {
	switch state {
	case "running":
		return 50
	case "ready", "active":
		return 40
	case "leased":
		return 30
	case "provisioning":
		return 20
	case "":
		return 0
	default:
		return 10
	}
}

func numericTagValue(value string) int64 {
	var n int64
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}
