package hyperv

import core "github.com/openclaw/crabbox/internal/cli"

func commandScript(req core.LocalCommandRequest) string {
	if len(req.Args) == 0 {
		return ""
	}
	return req.Args[len(req.Args)-1]
}
