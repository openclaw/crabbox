package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	actionCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dockerDigestPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
)

type workflowChecker struct {
	root         string
	localActions map[string]bool
}

func main() {
	if err := checkWorkflowDir(filepath.Join(".github", "workflows")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkWorkflowDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	checker := workflowChecker{
		root:         filepath.Dir(filepath.Dir(absoluteDir)),
		localActions: make(map[string]bool),
	}
	var findings []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := checker.checkWorkflowFile(path); err != nil {
			findings = append(findings, err)
		}
	}
	return errors.Join(findings...)
}

func (checker *workflowChecker) checkWorkflowFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	var findings []error
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		findings = append(findings, checkWorkflowNode(path, &document)...)
		for _, action := range workflowActionNodes(&document) {
			action = resolvedNode(action)
			if action.Kind == yaml.ScalarNode && strings.HasPrefix(action.Value, "./") {
				findings = append(findings, checker.checkLocalAction(path, action)...)
			}
		}
	}
	return errors.Join(findings...)
}

func (checker *workflowChecker) checkLocalAction(source string, action *yaml.Node) []error {
	target := action.Value
	if strings.HasPrefix(target, "./.github/workflows/") {
		ext := filepath.Ext(target)
		if ext == ".yml" || ext == ".yaml" {
			return nil
		}
	}

	localPath := filepath.Clean(filepath.Join(checker.root, filepath.FromSlash(strings.TrimPrefix(target, "./"))))
	relative, err := filepath.Rel(checker.root, localPath)
	if err != nil {
		return []error{fmt.Errorf("%s:%d invalid local action path %s: %w", source, action.Line, target, err)}
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return []error{fmt.Errorf("%s:%d local action escapes the repository: %s", source, action.Line, target)}
	}

	var manifests []string
	for _, name := range []string{"action.yml", "action.yaml"} {
		manifest := filepath.Join(localPath, name)
		if info, err := os.Stat(manifest); err == nil && !info.IsDir() {
			manifests = append(manifests, manifest)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return []error{fmt.Errorf("%s:%d inspect local action %s: %w", source, action.Line, target, err)}
		}
	}
	if len(manifests) == 0 {
		return []error{fmt.Errorf("%s:%d local action has no action.yml or action.yaml: %s", source, action.Line, target)}
	}

	var findings []error
	for _, manifest := range manifests {
		if checker.localActions[manifest] {
			continue
		}
		checker.localActions[manifest] = true
		findings = append(findings, checker.checkLocalActionManifest(manifest)...)
	}
	return findings
}

func (checker *workflowChecker) checkLocalActionManifest(path string) []error {
	file, err := os.Open(path)
	if err != nil {
		return []error{err}
	}
	defer file.Close()

	var document yaml.Node
	if err := yaml.NewDecoder(file).Decode(&document); err != nil {
		return []error{fmt.Errorf("%s: %w", path, err)}
	}
	var findings []error
	for _, runs := range mappingValues(resolvedNode(&document), "runs", nil) {
		for _, steps := range mappingValues(resolvedNode(runs), "steps", nil) {
			steps = resolvedNode(steps)
			if steps.Kind != yaml.SequenceNode {
				continue
			}
			for _, step := range steps.Content {
				for _, action := range mappingValues(resolvedNode(step), "uses", nil) {
					action = resolvedNode(action)
					if action.Kind != yaml.ScalarNode {
						findings = append(findings, fmt.Errorf("%s:%d uses must be a scalar action reference", path, action.Line))
						continue
					}
					if err := validateActionTarget(action.Value); err != nil {
						findings = append(findings, fmt.Errorf("%s:%d: %w", path, action.Line, err))
						continue
					}
					if strings.HasPrefix(action.Value, "./") {
						findings = append(findings, checker.checkLocalAction(path, action)...)
					}
				}
			}
		}
	}
	return findings
}

func checkWorkflowNode(path string, node *yaml.Node) []error {
	var findings []error
	for _, value := range workflowActionNodes(node) {
		value = resolvedNode(value)
		if value.Kind != yaml.ScalarNode {
			findings = append(findings, fmt.Errorf("%s:%d uses must be a scalar action reference", path, value.Line))
		} else if err := validateActionTarget(value.Value); err != nil {
			findings = append(findings, fmt.Errorf("%s:%d: %w", path, value.Line, err))
		}
	}
	return findings
}

func workflowActionNodes(document *yaml.Node) []*yaml.Node {
	var actions []*yaml.Node
	for _, jobs := range mappingValues(resolvedNode(document), "jobs", nil) {
		jobs = resolvedNode(jobs)
		if jobs.Kind != yaml.MappingNode {
			continue
		}
		for index := 1; index < len(jobs.Content); index += 2 {
			job := resolvedNode(jobs.Content[index])
			actions = append(actions, mappingValues(job, "uses", nil)...)
			for _, steps := range mappingValues(job, "steps", nil) {
				steps = resolvedNode(steps)
				if steps.Kind != yaml.SequenceNode {
					continue
				}
				for _, step := range steps.Content {
					actions = append(actions, mappingValues(resolvedNode(step), "uses", nil)...)
				}
			}
		}
	}
	return actions
}

func mappingValues(node *yaml.Node, key string, visited map[*yaml.Node]bool) []*yaml.Node {
	node = resolvedNode(node)
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if visited == nil {
		visited = make(map[*yaml.Node]bool)
	}
	if visited[node] {
		return nil
	}
	visited[node] = true
	defer delete(visited, node)

	var values []*yaml.Node
	for index := 0; index+1 < len(node.Content); index += 2 {
		entryKey := scalarNodeValue(node.Content[index])
		entryValue := node.Content[index+1]
		if entryKey == key {
			values = append(values, entryValue)
		}
		if entryKey == "<<" {
			merged := resolvedNode(entryValue)
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					values = append(values, mappingValues(item, key, visited)...)
				}
			} else {
				values = append(values, mappingValues(merged, key, visited)...)
			}
		}
	}
	return values
}

func resolvedNode(node *yaml.Node) *yaml.Node {
	for node != nil && (node.Kind == yaml.DocumentNode || node.Kind == yaml.AliasNode) {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
			continue
		}
		if len(node.Content) == 0 {
			return node
		}
		node = node.Content[0]
	}
	return node
}

func scalarNodeValue(node *yaml.Node) string {
	node = resolvedNode(node)
	if node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}

func validateActionTarget(target string) error {
	if strings.HasPrefix(target, "./") {
		return nil
	}
	if strings.HasPrefix(target, "docker://") {
		if dockerDigestPattern.MatchString(target) {
			return nil
		}
		return fmt.Errorf("container action must use an immutable digest: %s", target)
	}
	separator := strings.LastIndex(target, "@")
	if separator <= 0 || !actionCommitPattern.MatchString(target[separator+1:]) {
		return fmt.Errorf("action must use a full commit SHA: %s", target)
	}
	return nil
}
