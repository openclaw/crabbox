package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
)

type jjSourceContext struct {
	jjProtocolHeader
	WorkspaceRoot        string   `json:"workspace_root"`
	RepositoryPath       string   `json:"repository_path"`
	Workspace            string   `json:"workspace"`
	WorkingCopyOperation string   `json:"working_copy_operation"`
	OperationHeads       []string `json:"operation_heads"`
	InputUsedBytes       uint64   `json:"input_used_bytes"`
}

type jjSourceRead struct {
	Context        jjSourceContext
	Inventory      jjSourceInventory
	MetadataDigest [sha256.Size]byte
}

func decodeJJSourceContext(data []byte) (jjSourceContext, error) {
	var result jjSourceContext
	if err := decodeJJProtocolMessage(data, "context", &result); err != nil {
		return result, err
	}
	if !filepath.IsAbs(result.WorkspaceRoot) || !filepath.IsAbs(result.RepositoryPath) ||
		result.Workspace == "" || !nativeHexID(result.WorkingCopyOperation) ||
		len(result.OperationHeads) != 1 || !nativeHexID(result.OperationHeads[0]) || result.InputUsedBytes != 0 {
		return result, fmt.Errorf("native source context requires one explicit operation and an identified workspace without file input")
	}
	return result, nil
}

// The digest retains all native policy/state facts, including fields the Go
// scope adapter does not interpret. It is not a content fingerprint.
func decodeJJSourceRead(contextData, inventoryData []byte) (jjSourceRead, error) {
	var read jjSourceRead
	var err error
	read.Context, err = decodeJJSourceContext(contextData)
	if err != nil {
		return read, err
	}
	if err := decodeJJProtocolMessage(inventoryData, "live_inventory", &read.Inventory); err != nil {
		return jjSourceRead{}, err
	}
	inventory := read.Inventory
	if err := validateJJSourceIdentity(inventory.Identity); err != nil {
		return jjSourceRead{}, err
	}
	if err := validateJJContextIdentity(read.Context, inventory.Identity); err != nil {
		return jjSourceRead{}, err
	}
	if inventory.WorkingCopyFreshness != "fresh" || inventory.InputUsedBytes != 0 || inventory.WorkingCopyOperation != read.Context.WorkingCopyOperation {
		return jjSourceRead{}, fmt.Errorf("native source inventory does not match its captured context")
	}
	if !validJJTreeLabels(inventory.WorkingCopyTreeIDs, inventory.WorkingCopyTreeLabels) {
		return jjSourceRead{}, fmt.Errorf("native working-copy tree terms and labels are inconsistent")
	}
	for _, id := range inventory.WorkingCopyTreeIDs {
		if !nativeHexID(id) {
			return jjSourceRead{}, fmt.Errorf("invalid native working-copy tree identity")
		}
	}
	if inventory.Pending == nil || inventory.SparsePrefixes == nil || inventory.Tracked == nil ||
		inventory.Admitted == nil || inventory.Observations == nil || inventory.Untracked == nil {
		return jjSourceRead{}, fmt.Errorf("native live inventory is missing required metadata lists")
	}
	h := sha256.New()
	for _, data := range [][]byte{contextData, inventoryData} {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return jjSourceRead{}, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return jjSourceRead{}, fmt.Errorf("native source metadata has trailing data")
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			return jjSourceRead{}, err
		}
		h.Write(canonical)
		h.Write([]byte{0})
	}
	copy(read.MetadataDigest[:], h.Sum(nil))
	return read, nil
}

func validateJJContextIdentity(sourceContext jjSourceContext, identity jjSourceIdentity) error {
	if len(sourceContext.OperationHeads) != 1 || identity.WorkspaceRoot != sourceContext.WorkspaceRoot || identity.RepositoryPath != sourceContext.RepositoryPath || identity.Workspace != sourceContext.Workspace || identity.Operation != sourceContext.OperationHeads[0] {
		return fmt.Errorf("native source inventory does not match its captured context")
	}
	return nil
}
