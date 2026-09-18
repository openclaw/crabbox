package cli

import (
	"flag"
	"reflect"
	"testing"
)

func TestReplaceAppendListFlagSnapshotAndCopies(t *testing.T) {
	defaults := []string{"old", " raw "}
	value := newReplaceAppendListFlag(defaults)
	defaults[0] = "changed"
	if value.String() != "old, raw " {
		t.Fatalf("snapshot = %q", value.String())
	}
	copy := value.Get().([]string)
	copy[0] = "changed again"
	if value.String() != "old, raw " {
		t.Fatal("Get returned shared storage")
	}
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.Var(value, "item", "items")
	if fs.Lookup("item").DefValue != "old, raw " {
		t.Fatal("wrong help default")
	}
	if err := fs.Parse([]string{"--item=", "--item= one, ,two,one ", "--item=none", "--item= three "}); err != nil {
		t.Fatal(err)
	}
	if got := value.Get(); !reflect.DeepEqual(got, []string{"one", "two", "one", "none", "three"}) {
		t.Fatalf("Get = %#v", got)
	}
	if value.String() != "one,two,one,none,three" {
		t.Fatalf("String = %q", value.String())
	}
}

func TestReplaceAppendListFlagEmptyGetter(t *testing.T) {
	for _, defaults := range [][]string{nil, {}, {"old"}} {
		value := newReplaceAppendListFlag(defaults)
		if err := value.Set(" , "); err != nil {
			t.Fatal(err)
		}
		if value.values != nil {
			t.Fatalf("first empty Set retained storage: %#v", value.values)
		}
		if got := value.Get().([]string); got == nil || len(got) != 0 {
			t.Fatalf("Get must return defensive nonnil empty slice: %#v", got)
		}
	}
}

func TestNormalizeListValueContract(t *testing.T) {
	for _, input := range [][]string{nil, {}, {"", " \t"}, {" a ", "", "a", "none", " x,y "}} {
		before := append([]string(nil), input...)
		got := NormalizeList(input)
		want := []string{}
		if len(input) == 5 {
			want = []string{"a", "a", "none", "x,y"}
		}
		if got == nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got=%#v want=%#v", got, want)
		}
		if len(got) > 0 {
			got[0] = "changed"
		}
		if !reflect.DeepEqual(append([]string(nil), input...), before) {
			t.Fatal("normalization shared or mutated input storage")
		}
	}
}

func TestReplaceAppendListFlagBlankAfterValues(t *testing.T) {
	value := newReplaceAppendListFlag([]string{"prior"})
	for _, raw := range []string{" , ", "a, a,none", "  "} {
		if err := value.Set(raw); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(value.values, []string{"a", "a", "none"}) {
		t.Fatalf("values=%#v", value.values)
	}
}
