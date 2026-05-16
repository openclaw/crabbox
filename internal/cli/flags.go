package cli

import (
	"flag"
	"strings"
)

func flagWasSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

func FlagWasSet(fs *flag.FlagSet, name string) bool {
	return flagWasSet(fs, name)
}

func extractBoolFlag(args []string, name string) ([]string, bool) {
	want := "--" + name
	out := make([]string, 0, len(args))
	found := false
	for _, arg := range args {
		if arg == want {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}

func extractFirstPositionalArg(args []string, valueFlags map[string]bool) ([]string, string) {
	out := make([]string, 0, len(args))
	positional := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			out = append(out, arg)
			name := strings.TrimLeft(arg, "-")
			if before, _, ok := strings.Cut(name, "="); ok {
				name = before
			}
			if valueFlags[name] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
			continue
		}
		if positional == "" {
			positional = arg
			continue
		}
		out = append(out, arg)
	}
	return out, positional
}
