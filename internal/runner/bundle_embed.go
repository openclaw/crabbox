//go:build runnerembed

package runner

import _ "embed"

//go:embed embedded/runners.bin
var runnerBundle []byte

func embeddedRunnerBundle() []byte { return runnerBundle }
