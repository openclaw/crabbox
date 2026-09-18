package cli

import "strings"

// appendTrimmedListFlag retains inherited items and appends each whole occurrence.
// Empty strings and commas are values, not clearing or splitting instructions.
type appendTrimmedListFlag struct{ stringListFlag }

func newAppendTrimmedListFlag(defaults []string) *appendTrimmedListFlag {
	return &appendTrimmedListFlag{stringListFlag(append([]string(nil), defaults...))}
}

func (s *appendTrimmedListFlag) Set(value string) error {
	return s.stringListFlag.Set(strings.TrimSpace(value))
}

// appendTrimmedNonemptyListFlag ignores blank occurrences but retains whole values.
type appendTrimmedNonemptyListFlag struct{ stringListFlag }

func newAppendTrimmedNonemptyListFlag(defaults []string) *appendTrimmedNonemptyListFlag {
	return &appendTrimmedNonemptyListFlag{stringListFlag(append([]string(nil), defaults...))}
}

func (s *appendTrimmedNonemptyListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return s.stringListFlag.Set(value)
}

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
	s.values = append(s.values, NormalizeList(strings.Split(value, ","))...)
	return nil
}

func (s *replaceAppendListFlag) Get() any {
	return append([]string{}, s.values...)
}
