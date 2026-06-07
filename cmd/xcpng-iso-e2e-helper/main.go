package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/all"
	"github.com/openclaw/crabbox/internal/providers/xcpng"
)

func main() {
	ctx := context.Background()
	fs := flag.NewFlagSet("xcpng-iso-e2e-helper", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	mode := fs.String("mode", "read-only", "read-only or mutate")
	osName := fs.String("os", "", "linux or windows")
	iso := fs.String("iso", "", "installer ISO path, VDI name, UUID, or OpaqueRef")
	answerISO := fs.String("answer-iso", "", "optional answer ISO path, VDI name, UUID, or OpaqueRef")
	namePrefix := fs.String("name-prefix", "crabbox-xcpng-iso-e2e", "VM name prefix")
	timeout := fs.Duration("timeout", 20*time.Minute, "overall timeout")
	evidenceDir := fs.String("evidence-dir", ".crabbox/xcpng-iso-e2e", "evidence directory")
	summaryPath := fs.String("summary", "", "summary JSON path")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *summaryPath == "" {
		*summaryPath = filepath.Join(*evidenceDir, "summary.json")
	}
	cfg, err := core.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	summary, runErr := xcpng.RunISOE2E(ctx, xcpng.ISOE2EOptions{
		Config:      cfg,
		Mode:        *mode,
		OS:          *osName,
		ISO:         *iso,
		AnswerISO:   *answerISO,
		NamePrefix:  *namePrefix,
		Timeout:     *timeout,
		EvidenceDir: *evidenceDir,
		MutateGate:  os.Getenv("CRABBOX_XCP_NG_ISO_E2E_MUTATE") == "1",
	})
	summary.Evidence["summary"] = *summaryPath
	if writeErr := xcpng.WriteISOE2ESummary(*summaryPath, summary); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(summary); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if runErr != nil {
		var exitErr core.ExitError
		if core.AsExitError(runErr, &exitErr) {
			os.Exit(exitErr.Code)
		}
		os.Exit(1)
	}
}
