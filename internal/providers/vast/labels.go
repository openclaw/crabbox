package vast

import (
	"strings"
	"unicode"
)

const (
	vastOwnershipLabelPrefix = "cbx1|"
	vastLabelMaxPart         = 48
)

type vastOwnershipLabel struct {
	LeaseID string
	Slug    string
	State   string
}

func encodeVastOwnershipLabel(leaseID, slug, state string) string {
	leaseID = sanitizeVastLabelPart(leaseID, vastLabelMaxPart)
	slug = sanitizeVastLabelPart(slug, vastLabelMaxPart)
	state = sanitizeVastLabelPart(state, 16)
	parts := []string{leaseID, slug, state}
	return vastOwnershipLabelPrefix + strings.Join(parts, "|")
}

func decodeVastOwnershipLabel(label string) (vastOwnershipLabel, bool) {
	if !strings.HasPrefix(label, vastOwnershipLabelPrefix) {
		return vastOwnershipLabel{}, false
	}
	parts := strings.Split(strings.TrimPrefix(label, vastOwnershipLabelPrefix), "|")
	if len(parts) != 3 || parts[0] == "" {
		return vastOwnershipLabel{}, false
	}
	return vastOwnershipLabel{LeaseID: parts[0], Slug: parts[1], State: parts[2]}, true
}

func isVastCrabboxOwnedLabel(label string) bool {
	_, ok := decodeVastOwnershipLabel(label)
	return ok
}

func sanitizeVastLabelPart(value string, limit int) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= limit {
			break
		}
	}
	return strings.Trim(b.String(), "-_.")
}
