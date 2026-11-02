package utm

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
)

// Native UTM plists contain NSDate/NSData, which plutil cannot convert to JSON.
// Decode its XML representation with the standard library; unrequested fields
// remain observations and never get written back into the VM configuration.
type checkpointPlistNode struct {
	XMLName  xml.Name
	Text     string                `xml:",chardata"`
	Children []checkpointPlistNode `xml:",any"`
}

func checkpointPlistValue(node checkpointPlistNode) (any, error) {
	switch node.XMLName.Local {
	case "dict":
		if len(node.Children)%2 != 0 {
			return nil, fmt.Errorf("invalid plist dictionary")
		}
		result := map[string]any{}
		for i := 0; i < len(node.Children); i += 2 {
			key := node.Children[i]
			if key.XMLName.Local != "key" {
				return nil, fmt.Errorf("invalid plist dictionary key")
			}
			if _, exists := result[key.Text]; exists {
				return nil, fmt.Errorf("duplicate plist dictionary key")
			}
			value, err := checkpointPlistValue(node.Children[i+1])
			if err != nil {
				return nil, err
			}
			result[key.Text] = value
		}
		return result, nil
	case "array":
		result := make([]any, 0, len(node.Children))
		for _, child := range node.Children {
			value, err := checkpointPlistValue(child)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case "string", "date", "data":
		return node.Text, nil
	case "integer":
		value, err := strconv.ParseInt(strings.TrimSpace(node.Text), 10, 64)
		return value, err
	case "real":
		value, err := strconv.ParseFloat(strings.TrimSpace(node.Text), 64)
		return value, err
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return nil, fmt.Errorf("unsupported plist element %s", node.XMLName.Local)
	}
}
func decodeCheckpointPlist(data string, out any) error {
	var root checkpointPlistNode
	if err := xml.Unmarshal([]byte(data), &root); err != nil {
		return err
	}
	if root.XMLName.Local != "plist" || len(root.Children) != 1 {
		return fmt.Errorf("invalid UTM plist root")
	}
	value, err := checkpointPlistValue(root.Children[0])
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}
func (b *backend) readCheckpointPlist(ctx context.Context, path string, out any) error {
	result, err := b.command(ctx, "/usr/bin/plutil", []string{"-convert", "xml1", "-o", "-", path}, nil)
	if err != nil {
		return err
	}
	return decodeCheckpointPlist(result.Stdout, out)
}
