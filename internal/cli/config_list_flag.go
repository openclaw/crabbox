package cli

import "strings"

// replaceAppendListFlag replaces its snapshot on the first occurrence, then
// appends trimmed comma-separated items in order, including duplicates.
type replaceAppendListFlag struct {
	values []string
	set    bool
}

func newReplaceAppendListFlag(defaults []string) *replaceAppendListFlag {
	return &replaceAppendListFlag{values: append([]string(nil), defaults...)}
}

func (s *replaceAppendListFlag) String() string { return strings.Join(s.values, ",") }

func (s *replaceAppendListFlag) Set(value string) error {
	if !s.set {
		s.values = nil
		s.set = true
	}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			s.values = append(s.values, item)
		}
	}
	return nil
}

func (s *replaceAppendListFlag) Get() any {
	return append([]string{}, s.values...)
}
