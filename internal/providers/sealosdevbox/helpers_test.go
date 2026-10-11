package sealosdevbox

import (
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

func commandString(req core.LocalCommandRequest) string {
	return strings.TrimSpace(req.Name + " " + strings.Join(req.Args, " "))
}
