package cli

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type initPackageJSON struct {
	PackageManager string            `json:"packageManager"`
	Scripts        map[string]string `json:"scripts"`
}

type detectedPackageManager struct {
	Name        string
	Version     string
	HasLockfile bool
}

func detectInitProject(root string) initProjectDetection {
	d := initProjectDetection{}
	if root == "" {
		return d
	}
	for _, mod := range detectFiles(root, "go.mod") {
		rel := filepath.Dir(mod)
		d.Commands = append(d.Commands, subshellCommand(rel, "go test ./..."))
		d.PreflightTools = appendUniqueStrings(d.PreflightTools, "go")
	}
	for _, manifest := range detectFiles(root, "package.json") {
		rel := filepath.Dir(manifest)
		command, tools, excludes := detectPackageCommand(root, rel)
		if command == "" {
			continue
		}
		d.Commands = append(d.Commands, command)
		d.PreflightTools = appendUniqueStrings(d.PreflightTools, tools...)
		d.SyncExcludes = appendUniqueStrings(d.SyncExcludes, excludes...)
	}
	for _, manifest := range detectFiles(root, "Cargo.toml") {
		rel := filepath.Dir(manifest)
		d.Commands = append(d.Commands, subshellCommand(rel, "cargo test"))
		d.PreflightTools = appendUniqueStrings(d.PreflightTools, "cargo")
		d.SyncExcludes = appendUniqueStrings(d.SyncExcludes, "target")
	}
	if hasMakeTestTarget(filepath.Join(root, "Makefile")) || hasMakeTestTarget(filepath.Join(root, "makefile")) {
		d.Commands = append(d.Commands, "make test")
		d.PreflightTools = appendUniqueStrings(d.PreflightTools, "make")
	}
	d.Commands = appendUniqueStrings(nil, d.Commands...)
	return d
}

func detectFiles(root, name string) []string {
	var matches []string
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "dist", "dist-runtime", "bin", ".cache", ".turbo", ".venv", "target":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != name {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		matches = append(matches, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(matches)
	return matches
}

func detectPackageCommand(root, rel string) (string, []string, []string) {
	if shouldSkipNestedPackage(root, rel) {
		return "", nil, nil
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel), "package.json"))
	if err != nil {
		return "", nil, nil
	}
	var pkg initPackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", nil, nil
	}
	script := choosePackageScript(pkg.Scripts)
	if script == "" || packageScriptIsPlaceholder(pkg.Scripts[script]) {
		return "", nil, nil
	}
	pm := detectPackageManager(root, rel, pkg.PackageManager)
	tools := []string{"node", pm.Name}
	excludes := []string{"node_modules", ".npm", ".yarn/cache"}
	if pm.Name == "pnpm" || pm.Name == "yarn" {
		tools = append([]string{"corepack"}, tools...)
	}
	if pm.Name == "pnpm" {
		excludes = append(excludes, ".pnpm-store")
	}
	return packageManagerCommand(rel, pm, script), tools, excludes
}

func shouldSkipNestedPackage(root, rel string) bool {
	if rel == "." {
		return false
	}
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if packageDirHasLockfile(dir) {
		return false
	}
	parent := filepath.Dir(filepath.FromSlash(rel))
	for parent != "." && parent != "/" {
		if fileExists(filepath.Join(root, parent, "package.json")) {
			return true
		}
		parent = filepath.Dir(parent)
	}
	return fileExists(filepath.Join(root, "package.json"))
}

func choosePackageScript(scripts map[string]string) string {
	for _, name := range []string{"test:ci", "test", "check", "build"} {
		if _, ok := scripts[name]; ok {
			return name
		}
	}
	return ""
}

func packageScriptIsPlaceholder(script string) bool {
	normalized := strings.ToLower(strings.TrimSpace(script))
	return strings.Contains(normalized, "error: no test specified")
}

func detectPackageManager(root, rel, packageManager string) detectedPackageManager {
	pm, version := splitPackageManager(packageManager)
	dir := filepath.Join(root, filepath.FromSlash(rel))
	switch pm {
	case "pnpm", "yarn", "bun", "npm":
		return detectedPackageManager{Name: pm, Version: version, HasLockfile: packageManagerHasLockfile(dir, pm)}
	}
	switch {
	case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
		return detectedPackageManager{Name: "pnpm", HasLockfile: true}
	case fileExists(filepath.Join(dir, "yarn.lock")):
		return detectedPackageManager{Name: "yarn", HasLockfile: true}
	case fileExists(filepath.Join(dir, "bun.lock")) || fileExists(filepath.Join(dir, "bun.lockb")):
		return detectedPackageManager{Name: "bun", HasLockfile: true}
	default:
		return detectedPackageManager{Name: "npm", HasLockfile: fileExists(filepath.Join(dir, "package-lock.json"))}
	}
}

func splitPackageManager(packageManager string) (string, string) {
	value := strings.ToLower(strings.TrimSpace(packageManager))
	if value == "" {
		return "", ""
	}
	name, version, _ := strings.Cut(value, "@")
	return name, version
}

func packageManagerHasLockfile(dir, pm string) bool {
	switch pm {
	case "pnpm":
		return fileExists(filepath.Join(dir, "pnpm-lock.yaml"))
	case "yarn":
		return fileExists(filepath.Join(dir, "yarn.lock"))
	case "bun":
		return fileExists(filepath.Join(dir, "bun.lock")) || fileExists(filepath.Join(dir, "bun.lockb"))
	default:
		return fileExists(filepath.Join(dir, "package-lock.json"))
	}
}

func packageDirHasLockfile(dir string) bool {
	for _, file := range []string{"package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"} {
		if fileExists(filepath.Join(dir, file)) {
			return true
		}
	}
	return false
}

func packageManagerCommand(rel string, pm detectedPackageManager, script string) string {
	runScript := packageRunArgs(pm.Name, script)
	var command string
	switch pm.Name {
	case "pnpm":
		install := "pnpm install"
		if pm.HasLockfile {
			install += " --frozen-lockfile"
		}
		run := "pnpm " + runScript
		command = "corepack enable && " + install + " && " + run
	case "yarn":
		install := "yarn install"
		if pm.HasLockfile {
			install += yarnFrozenInstallFlag(pm.Version)
		}
		run := "yarn " + packageDirectScriptName(script)
		command = "corepack enable && " + install + " && " + run
	case "bun":
		install := "bun install"
		if pm.HasLockfile {
			install += " --frozen-lockfile"
		}
		run := "bun " + runScript
		command = install + " && " + run
	default:
		install := "npm install"
		if pm.HasLockfile {
			install = "npm ci"
		}
		run := "npm " + runScript
		command = install + " && " + run
	}
	if rel != "." {
		return subshellCommand(rel, command)
	}
	return command
}

func yarnFrozenInstallFlag(version string) string {
	major, _, _ := strings.Cut(version, ".")
	if parsed, err := strconv.Atoi(major); err == nil && parsed >= 2 {
		return " --immutable"
	}
	return " --frozen-lockfile"
}

func packageRunArgs(pm, script string) string {
	if (pm == "npm" || pm == "pnpm") && script == "test" {
		return script
	}
	return "run " + packageDirectScriptName(script)
}

func packageDirectScriptName(script string) string {
	return shellQuote(script)
}

func subshellCommand(rel, command string) string {
	if rel == "." {
		return command
	}
	return "(cd " + shellQuote(rel) + " && " + command + ")"
}

func hasMakeTestTarget(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if makefileLineDefinesTarget(line, "test") {
			return true
		}
	}
	return false
}

func makefileLineDefinesTarget(line, target string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ".") {
		return false
	}
	name, _, ok := strings.Cut(trimmed, ":")
	if !ok {
		return false
	}
	return strings.TrimSpace(name) == target
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
