package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestOpenEditorZedTargetConstraints(t *testing.T) {
	zed := editorHandoffSpecs["zed"]
	tests := []struct {
		name    string
		cfg     Config
		target  SSHTarget
		wantErr string
	}{
		{name: "linux", target: SSHTarget{TargetOS: targetLinux}},
		{name: "macos", target