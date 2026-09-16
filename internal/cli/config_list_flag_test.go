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
