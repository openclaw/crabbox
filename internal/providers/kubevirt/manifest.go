package kubevirt

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	leaseIDLabel   = "crabbox.dev/lease-id"
	slugLabel      = "crabbox.dev/slug"
	annotationBase = "crabbox.dev/"
)

func renderManifest(templatePath, name, namespace, leaseID, slug, publicKey string, leaseLabels map[string]string) ([]byte, error) {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("read KubeVirt template: %w", err)
	}
	placeholderValues := map[string]string{
		"{{NAME}}":           name,
		"{{NAMESPACE}}":      namespace,
		"{{LEASE_ID}}":       leaseID,
		"{{SLUG}}":           slug,
		"{{SSH_PUBLIC_KEY}}": publicKey,
	}
	protected := string(data)
	replacements := make(map[string]string, len(placeholderValues))
	for placeholder, value := range placeholderValues {
		sentinel := "__CRABBOX_" + strings.Trim(placeholder, "{}") + "__"
		protected = strings.ReplaceAll(protected, placeholder, sentinel)
		replacements[sentinel] = value
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(protected), &doc); err != nil {
		return nil, fmt.Errorf("parse KubeVirt template: %w", err)
	}
	root := documentMapping(&doc)
	if root == nil {
		return nil, fmt.Errorf("KubeVirt template must contain one YAML mapping")
	}
	if kind := mappingScalar(root, "kind"); kind != "VirtualMachine" {
		return nil, fmt.Errorf("KubeVirt template kind must be VirtualMachine, got %q", kind)
	}
	spec := mappingNode(root, "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("KubeVirt template spec must be a YAML mapping")
	}
	if runStrategy := mappingScalar(spec, "runStrategy"); runStrategy != "Manual" {
		return nil, fmt.Errorf("KubeVirt template spec.runStrategy must be Manual, got %q", runStrategy)
	}
	replaceManifestPlaceholders(&doc, replacements)
	metadata := ensureMapping(root, "metadata")
	setMappingScalar(metadata, "name", name)
	setMappingScalar(metadata, "namespace", namespace)
	labels := ensureMapping(metadata, "labels")
	setMappingScalar(labels, managedByLabel, "crabbox")
	setMappingScalar(labels, leaseIDLabel, leaseID)
	setMappingScalar(labels, slugLabel, slug)
	annotations := ensureMapping(metadata, "annotations")
	for key, value := range leaseLabels {
		setMappingScalar(annotations, annotationBase+key, value)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("encode KubeVirt manifest: %w", err)
	}
	return out, nil
}

func replaceManifestPlaceholders(node *yaml.Node, values map[string]string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.ScalarNode && (node.Tag == "!!str" || node.Tag == "") {
		for placeholder, value := range values {
			node.Value = strings.ReplaceAll(node.Value, placeholder, value)
		}
	}
	for _, child := range node.Content {
		replaceManifestPlaceholders(child, values)
	}
}

func documentMapping(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func mappingScalar(node *yaml.Node, key string) string {
	value := mappingNode(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}

func mappingNode(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func ensureMapping(node *yaml.Node, key string) *yaml.Node {
	if existing := mappingNode(node, key); existing != nil {
		if existing.Kind != yaml.MappingNode {
			existing.Kind = yaml.MappingNode
			existing.Tag = "!!map"
			existing.Value = ""
			existing.Content = nil
		}
		return existing
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valueNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content, keyNode, valueNode)
	return valueNode
}

func setMappingScalar(node *yaml.Node, key, value string) {
	if existing := mappingNode(node, key); existing != nil {
		existing.Kind = yaml.ScalarNode
		existing.Tag = "!!str"
		existing.Value = value
		existing.Content = nil
		return
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}
