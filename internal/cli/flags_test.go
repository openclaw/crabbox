package cli

import (
	"flag"
	"testing"
)

func TestExtractBoolFlag(t *testing.T) {
	args, found := extractBoolFlag([]string{"run_123", "--json", "--tail"}, "json")
	if !found {
		t.Fatalf("flag not found")
	}
	if len(args) != 2 || args[0] != "run_123" || args[1] != "--tail" {
		t.Fatalf("args=%v", args)
	}
}

func TestExtractBoolFlagMissing(t *testing.T) {
	args, found := extractBoolFlag([]string{"run_123"}, "json")
	if found {
		t.Fatalf("flag should not be found")
	}
	if len(args) != 1 || args[0] != "run_123" {
		t.Fatalf("args=%v", args)
	}
}

func TestFlagWasSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	value := fs.String("id", "", "")
	fs.Bool("json", false, "")
	if err := fs.Parse([]string{"--id", "blue-lobster"}); err != nil {
		t.Fatal(err)
	}
	if *value != "blue-lobster" {
		t.Fatalf("id=%q", *value)
	}
	if !flagWasSet(fs, "id") {
		t.Fatal("id should be marked set")
	}
	if !FlagWasSet(fs, "id") {
		t.Fatal("exported id check should be marked set")
	}
	if flagWasSet(fs, "json") {
		t.Fatal("json should not be marked set")
	}
	if FlagWasSet(fs, "json") {
		t.Fatal("exported json check should not be marked set")
	}

	for _, tc := range []struct {
		name string
		args []string
		flag string
		want bool
	}{
		{name: "unset default", flag: "name"},
		{name: "explicit empty", args: []string{"--name="}, flag: "name", want: true},
		{name: "explicit default", args: []string{"--name=default"}, flag: "name", want: true},
		{name: "explicit false", args: []string{"--enabled=false"}, flag: "enabled", want: true},
		{name: "explicit zero", args: []string{"--count=0"}, flag: "count", want: true},
		{name: "repeated setting", args: []string{"--name=first", "--name="}, flag: "name", want: true},
		{name: "exact name", args: []string{"--name=value"}, flag: "NAME"},
		{name: "unknown name", args: []string{"--name=value"}, flag: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			fs.String("name", "default", "")
			fs.Bool("enabled", false, "")
			fs.Int("count", 0, "")
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := FlagWasSet(fs, tc.flag); got != tc.want {
				t.Fatalf("FlagWasSet(%q)=%v, want %v", tc.flag, got, tc.want)
			}
		})
	}
}
