package cli

import (
	"context"
	"os"
	"strings"
)

// The local native reader needs its VCS configuration, not provider credentials,
// loaders, editor/pager commands, network authentication, or tracing controls.
// Explicit Git config values can themselves be private; they remain local and
// never enter command arguments, remote payload metadata, or public diagnostics.
func jjSourceChildEnvironment(parent []string) []string {
	allowed := map[string]bool{}
	for _, name := range []string{
		"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "LANG", "TZ",
		"XDG_CONFIG_HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
		"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT",
		"LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
		"LC_PAPER", "LC_NAME", "LC_ADDRESS", "LC_TELEPHONE", "LC_MEASUREMENT", "LC_IDENTIFICATION",
		"JJ_CONFIG", "JJ_USER", "JJ_EMAIL", "JJ_TIMESTAMP", "JJ_RANDOMNESS_SEED",
		"JJ_OP_TIMESTAMP", "JJ_OP_HOSTNAME", "JJ_OP_USERNAME",
		"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_COUNT",
		"GIT_WORK_TREE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_SHALLOW_FILE", "GIT_NAMESPACE", "GIT_INDEX_FILE",
		"GIT_ALLOC_LIMIT",
		"GIX_OBJECT_CACHE_MEMORY", "GIX_PACK_CACHE_MEMORY",
	} {
		allowed[name] = true
	}
	result := []string{}
	for _, entry := range parent {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		name = strings.ToUpper(name)
		if allowed[name] || jjIndexedGitConfigName(name) {
			result = append(result, entry)
		}
	}
	return result
}

func jjIndexedGitConfigName(name string) bool {
	index, ok := strings.CutPrefix(name, "GIT_CONFIG_KEY_")
	if !ok {
		index, ok = strings.CutPrefix(name, "GIT_CONFIG_VALUE_")
	}
	if !ok || index == "" {
		return false
	}
	for _, digit := range index {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func newInstalledJJSourceProcess(ctx context.Context, directory string, limits jjSourceProcessLimits, runner CommandRunner) (*jjSourceProcess, error) {
	parent := os.Environ()
	helper, err := installedJJSourceHelper(ctx)
	if err != nil {
		return nil, err
	}
	return newJJSourceProcess(ctx, jjSourceProcessOptions{
		BinaryPath: helper, Directory: directory, Environment: jjSourceChildEnvironment(parent),
		ConditionEnvironment: parent, Limits: limits, Runner: runner,
	})
}
