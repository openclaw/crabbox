package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	capsuleVersion          = 1
	repoBuildReplayClass    = "repo-build-replay"
	repoBuildReplayVersion  = "0.1.0"
	capsuleManifestFileName = "capsule.yaml"
	privateCapsuleDirMode   = 0o700
	privateCapsuleFileMode  = 0o600

	capsuleOutcomePass           = "pass"
	capsuleOutcomeFailReproduced = "fail_reproduced"
	capsuleOutcomeFailNew        = "fail_new"
	capsuleOutcomeEnvError       = "inconclusive_env_error"

	capsuleReplayOutputMaxBytes = 256 * 1024
)

type capsuleManifest struct {
	CapsuleVersion int                   `yaml:"capsule_version" json:"capsule_version"`
	CapsuleID      string                `yaml:"capsule_id" json:"capsule_id"`
	Class          string                `yaml:"class" json:"class"`
	ClassVersion   string                `yaml:"class_version" json:"class_version"`
	Scenario       string                `yaml:"scenario" json:"scenario"`
	TenantScope    string                `yaml:"tenant_scope" json:"tenant_scope"`
	Source         capsuleSource         `yaml:"source" json:"source"`
	Inputs         capsuleInputs         `yaml:"inputs" json:"inputs"`
	Oracle         capsuleOracle         `yaml:"oracle" json:"oracle"`
	Replay         capsuleReplayContract `yaml:"replay" json:"replay"`
	Cost           capsuleCost           `yaml:"cost" json:"cost"`
	Safety         capsuleSafety         `yaml:"safety" json:"safety"`
	Artifacts      capsuleArtifacts      `yaml:"artifacts" json:"artifacts"`
	Extensions     map[string]any        `yaml:"extensions,omitempty" json:"extensions,omitempty"`
	Replays        []capsuleReplayRecord `yaml:"replays,omitempty" json:"replays,omitempty"`
	Promotion      *capsulePromotion     `yaml:"promotion,omitempty" json:"promotion,omitempty"`
}

type capsuleSource struct {
	Kind         string `yaml:"kind" json:"kind"`
	Repo         string `yaml:"repo" json:"repo"`
	RunID        string `yaml:"run_id" json:"run_id"`
	RunURL       string `yaml:"run_url" json:"run_url"`
	Attempt      int    `yaml:"attempt,omitempty" json:"attempt,omitempty"`
	WorkflowName string `yaml:"workflow_name,omitempty" json:"workflow_name,omitempty"`
	WorkflowPath string `yaml:"workflow_path,omitempty" json:"workflow_path,omitempty"`
	JobName      string `yaml:"job_name,omitempty" json:"job_name,omitempty"`
	JobURL       string `yaml:"job_url,omitempty" json:"job_url,omitempty"`
	FailedStep   string `yaml:"failed_step,omitempty" json:"failed_step,omitempty"`
	HeadSHA      string `yaml:"head_sha,omitempty" json:"head_sha,omitempty"`
	HeadBranch   string `yaml:"head_branch,omitempty" json:"head_branch,omitempty"`
	Event        string `yaml:"event,omitempty" json:"event,omitempty"`
	Status       string `yaml:"status,omitempty" json:"status,omitempty"`
	Conclusion   string `yaml:"conclusion,omitempty" json:"conclusion,omitempty"`
	StartedAt    string `yaml:"started_at,omitempty" json:"started_at,omitempty"`
	CompletedAt  string `yaml:"completed_at,omitempty" json:"completed_at,omitempty"`
	CapturedAt   string `yaml:"captured_at" json:"captured_at"`
}

type capsuleInputs struct {
	SourceSnapshotDigest string `yaml:"source_snapshot_digest,omitempty" json:"source_snapshot_digest,omitempty"`
	ActionsRunDigest     string `yaml:"actions_run_digest,omitempty" json:"actions_run_digest,omitempty"`
	EnvManifestDigest    string `yaml:"env_manifest_digest,omitempty" json:"env_manifest_digest,omitempty"`
}

type capsuleOracle struct {
	Type                  string   `yaml:"type" json:"type"`
	SuccessCondition      string   `yaml:"success_condition" json:"success_condition"`
	FailureSignature      string   `yaml:"failure_signature,omitempty" json:"failure_signature,omitempty"`
	ForbiddenSuccessModes []string `yaml:"forbidden_success_modes" json:"forbidden_success_modes"`
}

type capsuleReplayContract struct {
	Command              string `yaml:"command" json:"command"`
	CommandMode          string `yaml:"command_mode" json:"command_mode"`
	RequiredQuality      string `yaml:"required_quality" json:"required_quality"`
	NondeterminismBudget string `yaml:"nondeterminism_budget,omitempty" json:"nondeterminism_budget,omitempty"`
}

type capsuleCost struct {
	MaxWallTimeSec         int  `yaml:"max_wall_time_sec" json:"max_wall_time_sec"`
	MaxSpendUnits          int  `yaml:"max_spend_units" json:"max_spend_units"`
	RequiresExclusiveLease bool `yaml:"requires_exclusive_lease" json:"requires_exclusive_lease"`
}

type capsuleSafety struct {
	ActionProfile string `yaml:"action_profile" json:"action_profile"`
	Network       string `yaml:"network" json:"network"`
	Secrets       string `yaml:"secrets" json:"secrets"`
}

type capsuleArtifacts struct {
	Logs      []capsuleArtifactRef `yaml:"logs,omitempty" json:"logs,omitempty"`
	GitHub    []capsuleArtifactRef `yaml:"github,omitempty" json:"github,omitempty"`
	Manifests []capsuleArtifactRef `yaml:"manifests,omitempty" json:"manifests,omitempty"`
}

type capsuleArtifactRef struct {
	Name   string `yaml:"name" json:"name"`
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	URL    string `yaml:"url,omitempty" json:"url,omitempty"`
	Size   int64  `yaml:"size,omitempty" json:"size,omitempty"`
	Digest string `yaml:"digest,omitempty" json:"digest,omitempty"`
}

type capsuleReplayRecord struct {
	At            string `yaml:"at" json:"at"`
	Outcome       string `yaml:"outcome" json:"outcome"`
	ReplayQuality string `yaml:"replay_quality" json:"replay_quality"`
	Command       string `yaml:"command" json:"command"`
	ExitCode      int    `yaml:"exit_code" json:"exit_code"`
	DurationMs    int64  `yaml:"duration_ms" json:"duration_ms"`
	KeptLease     bool   `yaml:"kept_lease" json:"kept_lease"`
	Note          string `yaml:"note,omitempty" json:"note,omitempty"`
}

type capsulePromotion struct {
	Regression bool   `yaml:"regression" json:"regression"`
	PromotedAt string `yaml:"promoted_at" json:"promoted_at"`
	Note       string `yaml:"note,omitempty" json:"note,omitempty"`
}

type capsuleRunView struct {
	Attempt      int              `json:"attempt"`
	Conclusion   string           `json:"conclusion"`
	CreatedAt    string           `json:"createdAt"`
	DisplayTitle string           `json:"displayTitle"`
	Event        string           `json:"event"`
	HeadBranch   string           `json:"headBranch"`
	HeadSHA      string           `json:"headSha"`
	Jobs         []capsuleJobView `json:"jobs"`
	Name         string           `json:"name"`
	StartedAt    string           `json:"startedAt"`
	Status       string           `json:"status"`
	UpdatedAt    string           `json:"updatedAt"`
	URL          string           `json:"url"`
	WorkflowName string           `json:"workflowName"`
}

type capsuleJobView struct {
	Conclusion string            `json:"conclusion"`
	DatabaseID int64             `json:"databaseId"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Steps      []capsuleStepView `json:"steps"`
	URL        string            `json:"url"`
}

type capsuleStepView struct {
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
}

type actionsRunAPIResponse struct {
	Path string `json:"path"`
}

type actionsArtifactsResponse struct {
	TotalCount int               `json:"total_count"`
	Artifacts  []actionsArtifact `json:"artifacts"`
}

type actionsArtifact struct {
	Name               string `json:"name"`
	SizeInBytes        int64  `json:"size_in_bytes"`
	Expired            bool   `json:"expired"`
	ArchiveDownloadURL string `json:"archive_download_url"`
}

func (a App) capsuleFromActions(ctx context.Context, args []string) error {
	args = moveKnownFlagsBeforePositionals(args,
		[]string{"repo", "replay", "output", "scenario", "job", "required-quality", "max-log-bytes"},
		[]string{"no-logs"},
	)
	fs := newFlagSet("capsule from-actions", a.Stderr)
	repoFlag := fs.String("repo", "", "GitHub repository owner/name; optional when the argument is a run URL")
	replayCommand := fs.String("replay", "", "replay command to run inside Crabbox")
	outputDir := fs.String("output", "", "capsule output directory")
	scenario := fs.String("scenario", "", "human-readable scenario")
	jobName := fs.String("job", "", "preferred failed job name when a run has multiple failures")
	requiredQuality := fs.String("required-quality", "semantically_identical", "required replay quality")
	maxLogBytes := fs.Int("max-log-bytes", 256*1024, "maximum failed log bytes to keep locally")
	noLogs := fs.Bool("no-logs", false, "skip fetching failed Actions logs")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox capsule from-actions <run-url> --replay '<command>'")
	}
	if strings.TrimSpace(*replayCommand) == "" {
		return exit(2, "capsule from-actions requires --replay")
	}
	if *maxLogBytes <= 0 {
		return exit(2, "--max-log-bytes must be greater than 0")
	}
	runRef, err := parseActionsRunRef(fs.Arg(0), *repoFlag)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := ValidateProviderCredentialDestination(cfg); err != nil {
		return err
	}
	childEnvDenylist := externalDesktopChildEnvDenylist(cfg, cfg.TargetOS)
	view, err := fetchActionsRunView(ctx, runRef, childEnvDenylist)
	if err != nil {
		return err
	}
	runRef.Attempt = firstNonZero(runRef.Attempt, view.Attempt)
	workflowPath, err := fetchActionsWorkflowPath(ctx, runRef, childEnvDenylist)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: workflow path unavailable: %v\n", err)
	}
	artifacts, err := fetchActionsArtifacts(ctx, runRef, childEnvDenylist)
	if err != nil {
		fmt.Fprintf(a.Stderr, "warning: artifact metadata unavailable: %v\n", err)
	}
	job, step, jobMatched := selectCapsuleFailure(view.Jobs, *jobName)
	if !jobMatched {
		return exit(2, "capsule from-actions --job %q did not match any job in the run", *jobName)
	}
	if !isFailureConclusion(job.Conclusion) {
		if job.Name != "" {
			return exit(2, "capsule from-actions selected job %q but its conclusion is %q, not a failure", job.Name, blank(job.Conclusion, "-"))
		}
		return exit(2, "capsule from-actions requires a failed GitHub Actions job")
	}
	if *scenario == "" {
		*scenario = defaultCapsuleScenario(view, job, step)
	}
	dir := *outputDir
	if dir == "" {
		dir = filepath.Join("capsules", defaultCapsuleOutputName(runRef, job, step, *jobName != "" || capsuleRunHasMultipleFailures(view.Jobs)))
	}
	if err := ensurePrivateCapsuleDir(dir); err != nil {
		return err
	}
	if err := ensurePrivateCapsuleDir(filepath.Join(dir, "logs")); err != nil {
		return err
	}
	logRef, failureSignature := capsuleArtifactRef{}, ""
	if !*noLogs {
		logText, logErr := fetchActionsFailedLog(ctx, runRef, childEnvDenylist)
		if logErr != nil {
			fmt.Fprintf(a.Stderr, "warning: failed log unavailable: %v\n", logErr)
		} else if logText != "" {
			logText = boundString(logText, *maxLogBytes)
			logPath := filepath.Join(dir, "logs", "failed.log")
			if err := writePrivateCapsuleFile(logPath, []byte(logText)); err != nil {
				return err
			}
			logDigest := sha256String(logText)
			logRef = capsuleArtifactRef{Name: "failed-actions-log", Path: filepath.ToSlash(filepath.Join("logs", "failed.log")), Size: int64(len(logText)), Digest: "sha256:" + logDigest}
			failureSignature = capsuleFailureSignatureForSelection(logText, job.Name, step.Name)
		}
	}
	manifest := buildActionsCapsuleManifest(runRef, view, workflowPath, job, step, *scenario, *replayCommand, *requiredQuality, failureSignature, logRef, artifacts)
	if err := writeCapsuleManifest(filepath.Join(dir, capsuleManifestFileName), manifest); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "capsule written %s id=%s class=%s replay=%q\n", filepath.Join(dir, capsuleManifestFileName), manifest.CapsuleID, manifest.Class, manifest.Replay.Command)
	if manifest.Source.JobName != "" {
		fmt.Fprintf(a.Stdout, "source repo=%s run=%s job=%q step=%q conclusion=%s\n", manifest.Source.Repo, manifest.Source.RunURL, manifest.Source.JobName, blank(manifest.Source.FailedStep, "-"), blank(manifest.Source.Conclusion, "-"))
	}
	return nil
}

func (a App) capsuleReplay(ctx context.Context, args []string) error {
	args = moveKnownFlagsBeforePositionals(args,
		[]string{"id", "junit"},
		[]string{"keep", "no-sync", "reclaim"},
	)
	fs := newFlagSet("capsule replay", a.Stderr)
	leaseID := fs.String("id", "", "existing lease id or slug")
	keep := fs.Bool("keep", false, "keep the lease after replay for SSH debugging")
	junit := fs.String("junit", "", "comma-separated remote JUnit XML paths to record")
	noSync := fs.Bool("no-sync", false, "skip rsync")
	reclaim := fs.Bool("reclaim", false, "claim this lease for the current repo")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox capsule replay <capsule.yaml> [--keep]")
	}
	path := capsuleManifestPath(fs.Arg(0))
	manifest, err := readCapsuleManifest(path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(manifest.Replay.Command) == "" {
		return exit(2, "capsule %s has no replay.command", path)
	}
	runArgs := []string{"--shell"}
	if *leaseID != "" {
		runArgs = append(runArgs, "--id", *leaseID)
	}
	if *keep {
		runArgs = append(runArgs, "--keep")
	}
	if *junit != "" {
		runArgs = append(runArgs, "--junit", *junit)
	}
	if *noSync {
		runArgs = append(runArgs, "--no-sync")
	}
	if *reclaim {
		runArgs = append(runArgs, "--reclaim")
	}
	runArgs = append(runArgs, "--", manifest.Replay.Command)
	started := time.Now()
	runApp := a
	var replayOutput runLogBuffer
	if strings.TrimSpace(manifest.Oracle.FailureSignature) != "" {
		runApp.Stdout = io.MultiWriter(a.Stdout, &replayOutput)
		runApp.Stderr = io.MultiWriter(a.Stderr, &replayOutput)
	}
	err = runApp.runCommand(ctx, runArgs)
	record := capsuleReplayRecord{
		At:            time.Now().UTC().Format(time.RFC3339),
		Command:       manifest.Replay.Command,
		KeptLease:     *keep,
		ReplayQuality: manifest.Replay.RequiredQuality,
	}
	if err == nil {
		record.DurationMs = time.Since(started).Milliseconds()
		record.Outcome = capsuleOutcomePass
		record.ExitCode = 0
		record.Note = "replay command exited 0; original failure was not reproduced"
		manifest.Replays = append(manifest.Replays, record)
		_ = writeCapsuleManifest(path, manifest)
		return exit(1, "capsule replay did not reproduce the failure; command exited 0 after %s", time.Since(started).Round(time.Millisecond))
	}
	if code, ok := remoteReplayExitCode(err); ok {
		record.DurationMs = time.Since(started).Milliseconds()
		record.ExitCode = code
		outcome, note, reproduced := capsuleReplayFailureOutcome(manifest.Oracle.FailureSignature, replayOutput.String(), code)
		record.Outcome = outcome
		record.Note = note
		manifest.Replays = append(manifest.Replays, record)
		if writeErr := writeCapsuleManifest(path, manifest); writeErr != nil {
			return writeErr
		}
		if !reproduced {
			return exit(1, "capsule replay found a new failure; exit=%d did not contain failure_signature %q in the last %d bytes of replay output", code, strings.TrimSpace(manifest.Oracle.FailureSignature), capsuleReplayOutputMaxBytes)
		}
		fmt.Fprintf(a.Stdout, "capsule replay outcome=%s exit=%d quality=%s total=%s\n", record.Outcome, code, record.ReplayQuality, time.Since(started).Round(time.Millisecond))
		if *keep {
			fmt.Fprintln(a.Stdout, "lease kept for debugging; use the printed lease id or slug with: crabbox ssh --id <id-or-slug>")
		}
		return nil
	}
	record.Outcome = capsuleOutcomeEnvError
	record.DurationMs = time.Since(started).Milliseconds()
	record.ExitCode = 1
	record.Note = err.Error()
	manifest.Replays = append(manifest.Replays, record)
	_ = writeCapsuleManifest(path, manifest)
	return err
}

func (a App) capsuleInspect(ctx context.Context, args []string) error {
	args = moveKnownFlagsBeforePositionals(args, nil, []string{"json"})
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("capsule inspect", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox capsule inspect <capsule.yaml>")
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	manifest, err := readCapsuleManifest(capsuleManifestPath(fs.Arg(0)))
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(manifest)
	}
	fmt.Fprintf(a.Stdout, "capsule %s class=%s@%s scenario=%q\n", manifest.CapsuleID, manifest.Class, manifest.ClassVersion, manifest.Scenario)
	fmt.Fprintf(a.Stdout, "source kind=%s repo=%s run=%s job=%q step=%q\n", manifest.Source.Kind, manifest.Source.Repo, manifest.Source.RunURL, blank(manifest.Source.JobName, "-"), blank(manifest.Source.FailedStep, "-"))
	fmt.Fprintf(a.Stdout, "replay command=%q required_quality=%s\n", manifest.Replay.Command, manifest.Replay.RequiredQuality)
	fmt.Fprintf(a.Stdout, "oracle type=%s failure_signature=%q\n", manifest.Oracle.Type, blank(manifest.Oracle.FailureSignature, "-"))
	if len(manifest.Replays) > 0 {
		last := manifest.Replays[len(manifest.Replays)-1]
		fmt.Fprintf(a.Stdout, "last_replay outcome=%s exit=%d duration_ms=%d kept=%t at=%s\n", last.Outcome, last.ExitCode, last.DurationMs, last.KeptLease, last.At)
	}
	if manifest.Promotion != nil && manifest.Promotion.Regression {
		fmt.Fprintf(a.Stdout, "promotion regression=true at=%s\n", manifest.Promotion.PromotedAt)
	}
	return nil
}

func (a App) capsulePromote(ctx context.Context, args []string) error {
	args = moveKnownFlagsBeforePositionals(args, []string{"note"}, []string{"regression"})
	fs := newFlagSet("capsule promote", a.Stderr)
	regression := fs.Bool("regression", false, "promote this capsule as a regression replay")
	note := fs.String("note", "", "promotion note")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox capsule promote <capsule.yaml> --regression")
	}
	if !*regression {
		return exit(2, "capsule promote currently requires --regression")
	}
	path := capsuleManifestPath(fs.Arg(0))
	manifest, err := readCapsuleManifest(path)
	if err != nil {
		return err
	}
	manifest.Promotion = &capsulePromotion{
		Regression: true,
		PromotedAt: time.Now().UTC().Format(time.RFC3339),
		Note:       *note,
	}
	if err := writeCapsuleManifest(path, manifest); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "capsule promoted regression=true path=%s id=%s\n", path, manifest.CapsuleID)
	return nil
}

type actionsRunRef struct {
	Repo    GitHubRepo
	RunID   string
	Attempt int
}

func parseActionsRunRef(value, repoOverride string) (actionsRunRef, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return actionsRunRef{}, exit(2, "empty GitHub Actions run URL")
	}
	if _, err := strconv.ParseInt(value, 10, 64); err == nil || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
		if _, err := parsePositiveActionsInt(value, "run id"); err != nil {
			return actionsRunRef{}, err
		}
		if repoOverride == "" {
			return actionsRunRef{}, exit(2, "run id requires --repo owner/name")
		}
		repo, err := parseGitHubRepo(repoOverride)
		if err != nil {
			return actionsRunRef{}, err
		}
		return actionsRunRef{Repo: repo, RunID: value}, nil
	}
	u, err := url.Parse(value)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return actionsRunRef{}, exit(2, "expected GitHub Actions run URL or numeric run id with --repo")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 5 || parts[2] != "actions" || parts[3] != "runs" {
		return actionsRunRef{}, exit(2, "expected GitHub Actions run URL like https://github.com/owner/repo/actions/runs/123")
	}
	repo, err := cleanGitHubRepo(parts[0], parts[1])
	if err != nil {
		return actionsRunRef{}, err
	}
	if _, err := parsePositiveActionsInt(parts[4], "run id"); err != nil {
		return actionsRunRef{}, err
	}
	ref := actionsRunRef{Repo: repo, RunID: parts[4]}
	if len(parts) >= 6 && parts[5] == "attempts" {
		if len(parts) < 7 {
			return actionsRunRef{}, exit(2, "expected GitHub Actions attempt URL like https://github.com/owner/repo/actions/runs/123/attempts/2")
		}
		attempt, err := parsePositiveActionsInt(parts[6], "attempt")
		if err != nil {
			return actionsRunRef{}, err
		}
		ref.Attempt = attempt
	}
	if repoOverride != "" {
		repo, err := parseGitHubRepo(repoOverride)
		if err != nil {
			return actionsRunRef{}, err
		}
		ref.Repo = repo
	}
	return ref, nil
}

func parsePositiveActionsInt(value, label string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, exit(2, "invalid GitHub Actions %s %q", label, value)
	}
	return n, nil
}

func fetchActionsRunView(ctx context.Context, ref actionsRunRef, childEnvDenylist []string) (capsuleRunView, error) {
	args := []string{"run", "view", ref.RunID, "--repo", ref.Repo.Slug(), "--json", "attempt,conclusion,createdAt,displayTitle,event,headBranch,headSha,jobs,name,startedAt,status,updatedAt,url,workflowName"}
	if ref.Attempt > 0 {
		args = append(args, "--attempt", strconv.Itoa(ref.Attempt))
	}
	out, err := ghOutputWithChildEnvironment(ctx, "", childEnvDenylist, args...)
	if err != nil {
		return capsuleRunView{}, err
	}
	var view capsuleRunView
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return capsuleRunView{}, err
	}
	return view, nil
}

func fetchActionsWorkflowPath(ctx context.Context, ref actionsRunRef, childEnvDenylist []string) (string, error) {
	out, err := ghOutputWithChildEnvironment(ctx, "", childEnvDenylist, "api", "repos/"+ref.Repo.Slug()+"/actions/runs/"+ref.RunID)
	if err != nil {
		return "", err
	}
	var res actionsRunAPIResponse
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return "", err
	}
	return res.Path, nil
}

func fetchActionsArtifacts(ctx context.Context, ref actionsRunRef, childEnvDenylist []string) ([]capsuleArtifactRef, error) {
	const pageSize = 100
	artifacts := []capsuleArtifactRef{}
	seen := 0
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("repos/%s/actions/runs/%s/artifacts?per_page=%d&page=%d", ref.Repo.Slug(), ref.RunID, pageSize, page)
		out, err := ghOutputWithChildEnvironment(ctx, "", childEnvDenylist, "api", endpoint)
		if err != nil {
			return nil, err
		}
		var res actionsArtifactsResponse
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			return nil, err
		}
		artifacts = appendActionsArtifactRefs(artifacts, res.Artifacts)
		seen += len(res.Artifacts)
		if len(res.Artifacts) == 0 || len(res.Artifacts) < pageSize || (res.TotalCount > 0 && seen >= res.TotalCount) {
			break
		}
	}
	return artifacts, nil
}

func appendActionsArtifactRefs(dst []capsuleArtifactRef, src []actionsArtifact) []capsuleArtifactRef {
	for _, artifact := range src {
		if artifact.Expired {
			continue
		}
		dst = append(dst, capsuleArtifactRef{
			Name: artifact.Name,
			URL:  artifact.ArchiveDownloadURL,
			Size: artifact.SizeInBytes,
		})
	}
	return dst
}

func fetchActionsFailedLog(ctx context.Context, ref actionsRunRef, childEnvDenylist []string) (string, error) {
	args := []string{"run", "view", ref.RunID, "--repo", ref.Repo.Slug(), "--log-failed"}
	if ref.Attempt > 0 {
		args = append(args, "--attempt", strconv.Itoa(ref.Attempt))
	}
	return ghOutputAllowErrorWithChildEnvironment(ctx, "", childEnvDenylist, args...)
}

func ghOutputAllowErrorWithChildEnvironment(ctx context.Context, dir string, childEnvDenylist []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if len(childEnvDenylist) > 0 {
		cmd.Env = childEnvironmentWithout(os.Environ(), childEnvDenylist...)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func selectCapsuleFailure(jobs []capsuleJobView, preferredJob string) (capsuleJobView, capsuleStepView, bool) {
	preferredJob = strings.TrimSpace(preferredJob)
	matchedPreferred := preferredJob == ""
	var selected capsuleJobView
	for _, job := range jobs {
		if preferredJob != "" && job.Name != preferredJob {
			continue
		}
		matchedPreferred = true
		if selected.Name == "" || isFailureConclusion(job.Conclusion) {
			selected = job
		}
		if isFailureConclusion(job.Conclusion) {
			break
		}
	}
	if selected.Name == "" && len(jobs) > 0 {
		selected = jobs[0]
	}
	for _, step := range selected.Steps {
		if isFailureConclusion(step.Conclusion) {
			return selected, step, matchedPreferred
		}
	}
	return selected, capsuleStepView{}, matchedPreferred
}

func isFailureConclusion(conclusion string) bool {
	switch strings.ToLower(conclusion) {
	case "failure", "timed_out", "cancelled", "action_required":
		return true
	default:
		return false
	}
}

func capsuleRunHasMultipleFailures(jobs []capsuleJobView) bool {
	failures := 0
	for _, job := range jobs {
		if isFailureConclusion(job.Conclusion) {
			failures++
		}
	}
	return failures > 1
}

func buildActionsCapsuleManifest(ref actionsRunRef, view capsuleRunView, workflowPath string, job capsuleJobView, step capsuleStepView, scenario, replayCommand, requiredQuality, failureSignature string, logRef capsuleArtifactRef, artifacts []capsuleArtifactRef) capsuleManifest {
	capturedAt := time.Now().UTC().Format(time.RFC3339)
	ref.Attempt = firstNonZero(ref.Attempt, view.Attempt)
	failureSignature = strings.TrimSpace(failureSignature)
	capsuleID := "sha256:" + capsuleIDDigest(ref, view.HeadSHA, replayCommand, capsuleFailureIdentity(job, step, failureSignature))
	successCondition := "The replay command exits non-zero."
	nondeterminismBudget := "exit code must remain non-zero"
	if failureSignature != "" {
		successCondition = "The replay command exits non-zero with the same failure signature."
		nondeterminismBudget = "exit code and failure signature must match"
	}
	logs := []capsuleArtifactRef{}
	if logRef.Name != "" {
		logs = append(logs, logRef)
	}
	manifest := capsuleManifest{
		CapsuleVersion: capsuleVersion,
		CapsuleID:      capsuleID,
		Class:          repoBuildReplayClass,
		ClassVersion:   repoBuildReplayVersion,
		Scenario:       scenario,
		TenantScope:    "external_sanitized",
		Source: capsuleSource{
			Kind:         "github_actions",
			Repo:         ref.Repo.Slug(),
			RunID:        ref.RunID,
			RunURL:       blank(view.URL, "https://github.com/"+ref.Repo.Slug()+"/actions/runs/"+ref.RunID),
			Attempt:      ref.Attempt,
			WorkflowName: blank(view.WorkflowName, view.Name),
			WorkflowPath: workflowPath,
			JobName:      job.Name,
			JobURL:       job.URL,
			FailedStep:   step.Name,
			HeadSHA:      view.HeadSHA,
			HeadBranch:   view.HeadBranch,
			Event:        view.Event,
			Status:       view.Status,
			Conclusion:   view.Conclusion,
			StartedAt:    view.StartedAt,
			CompletedAt:  view.UpdatedAt,
			CapturedAt:   capturedAt,
		},
		Inputs: capsuleInputs{
			SourceSnapshotDigest: digestLabel("git", view.HeadSHA),
			ActionsRunDigest:     digestLabel("github_actions_run", actionsRunIdentity(ref)),
		},
		Oracle: capsuleOracle{
			Type:             "deterministic_rerun",
			SuccessCondition: successCondition,
			FailureSignature: failureSignature,
			ForbiddenSuccessModes: []string{
				"passing by deleting or skipping the failing test",
				"passing by removing the failing build target",
				"passing by ignoring the replay command exit code",
			},
		},
		Replay: capsuleReplayContract{
			Command:              replayCommand,
			CommandMode:          "shell",
			RequiredQuality:      blank(requiredQuality, "semantically_identical"),
			NondeterminismBudget: nondeterminismBudget,
		},
		Cost: capsuleCost{
			MaxWallTimeSec:         3600,
			MaxSpendUnits:          1,
			RequiresExclusiveLease: false,
		},
		Safety: capsuleSafety{
			ActionProfile: "build_debug_v1",
			Network:       "repo_default",
			Secrets:       "denied",
		},
		Artifacts: capsuleArtifacts{
			Logs:   logs,
			GitHub: artifacts,
		},
		Extensions: map[string]any{
			repoBuildReplayClass: map[string]any{
				"schema_version": 1,
				"source":         "github_actions",
				"replay_mode":    "explicit_command",
			},
		},
	}
	return manifest
}

func capsuleIDDigest(ref actionsRunRef, headSHA, replayCommand, failureIdentity string) string {
	sum := sha256.Sum256([]byte(actionsRunIdentity(ref) + "\n" + headSHA + "\n" + replayCommand + "\n" + failureIdentity))
	return hex.EncodeToString(sum[:])
}

func actionsRunIdentity(ref actionsRunRef) string {
	identity := ref.Repo.Slug() + "#" + ref.RunID
	if ref.Attempt > 0 {
		identity += "#attempt-" + strconv.Itoa(ref.Attempt)
	}
	return identity
}

func defaultCapsuleOutputName(ref actionsRunRef, job capsuleJobView, step capsuleStepView, disambiguateFailure bool) string {
	name := ref.Repo.Owner + "-" + ref.Repo.Name + "-actions-" + ref.RunID
	if ref.Attempt > 1 {
		name += "-attempt-" + strconv.Itoa(ref.Attempt)
	}
	if disambiguateFailure {
		name += "-" + capsuleFailurePathSuffix(job, step)
	}
	return safePathComponent(name)
}

func capsuleFailureIdentity(job capsuleJobView, step capsuleStepView, failureSignature string) string {
	return strings.TrimSpace(job.Name) + "\n" + strings.TrimSpace(step.Name) + "\n" + strings.TrimSpace(failureSignature)
}

func capsuleFailurePathSuffix(job capsuleJobView, step capsuleStepView) string {
	parts := []string{}
	if name := strings.TrimSpace(job.Name); name != "" {
		parts = append(parts, name)
	}
	if name := strings.TrimSpace(step.Name); name != "" {
		parts = append(parts, name)
	}
	if len(parts) == 0 {
		return "failure"
	}
	return strings.Join(parts, "-")
}

func digestLabel(kind, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return kind + ":" + strings.TrimSpace(value)
}

func defaultCapsuleScenario(view capsuleRunView, job capsuleJobView, step capsuleStepView) string {
	parts := []string{"Replay GitHub Actions"}
	if view.WorkflowName != "" {
		parts = append(parts, view.WorkflowName)
	}
	if job.Name != "" {
		parts = append(parts, "job "+job.Name)
	}
	if step.Name != "" {
		parts = append(parts, "step "+step.Name)
	}
	return strings.Join(parts, " ")
}

func capsuleFailureSignature(logText string) string {
	lines := strings.Split(logText, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(stripGitHubLogPrefix(lines[i]))
		if line == "" || isLowSignalActionsLogLine(line) {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "fail") ||
			strings.Contains(lower, "error") ||
			strings.Contains(lower, "panic") ||
			strings.Contains(lower, "exit status") {
			if len(line) > 240 {
				line = line[:240]
			}
			return line
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(stripGitHubLogPrefix(lines[i]))
		if line == "" || isLowSignalActionsLogLine(line) {
			continue
		}
		if len(line) > 240 {
			line = line[:240]
		}
		return line
	}
	return ""
}

func capsuleFailureSignatureForSelection(logText, jobName, stepName string) string {
	filtered := filterActionsLogForSelection(logText, jobName, stepName)
	if strings.TrimSpace(filtered) == "" && strings.TrimSpace(stepName) != "" {
		filtered = filterActionsLogForSelection(logText, jobName, "")
	}
	if strings.TrimSpace(filtered) == "" {
		return ""
	}
	return capsuleFailureSignature(filtered)
}

func filterActionsLogForSelection(logText, jobName, stepName string) string {
	jobName = strings.TrimSpace(jobName)
	stepName = strings.TrimSpace(stepName)
	if jobName == "" {
		return ""
	}
	var b strings.Builder
	for _, line := range strings.Split(logText, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) < 3 {
			continue
		}
		if strings.TrimSpace(fields[0]) != jobName {
			continue
		}
		if stepName != "" && strings.TrimSpace(fields[1]) != stepName {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func stripGitHubLogPrefix(line string) string {
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) >= 3 {
		line = fields[2]
	}
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "\ufeff")
	if lineHasGitHubTimestampPrefix(line) {
		if space := strings.IndexByte(line, ' '); space > 0 {
			line = strings.TrimSpace(line[space+1:])
		}
	}
	line = strings.TrimPrefix(line, "##[error]")
	return strings.TrimSpace(line)
}

func lineHasGitHubTimestampPrefix(line string) bool {
	if len(line) < 20 || line[4] != '-' || line[7] != '-' || line[10] != 'T' {
		return false
	}
	space := strings.IndexByte(line, ' ')
	if space < 0 {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, line[:space])
	return err == nil
}

func isLowSignalActionsLogLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	return lower == "post job cleanup." ||
		lower == "fail" ||
		strings.Contains(lower, "cleaning up orphan processes") ||
		strings.HasPrefix(lower, "process completed with exit code ") ||
		strings.HasPrefix(lower, "fail\t") ||
		strings.HasPrefix(lower, "[command]/usr/bin/git ") ||
		strings.HasPrefix(lower, "removing ") ||
		strings.HasPrefix(lower, "temporarily overriding home=") ||
		strings.HasPrefix(lower, "adding repository directory ")
}

func boundString(value string, maxBytes int) string {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value
	}
	return value[len(value)-maxBytes:]
}

func writeCapsuleManifest(path string, manifest capsuleManifest) error {
	data, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	return writePrivateCapsuleFile(path, data)
}

func ensurePrivateCapsuleDir(path string) error {
	if err := os.MkdirAll(path, privateCapsuleDirMode); err != nil {
		return err
	}
	return os.Chmod(path, privateCapsuleDirMode)
}

func writePrivateCapsuleFile(path string, data []byte) error {
	// Create missing capsule parents privately without changing permissions on
	// an existing repository, shared directory, or the current working directory.
	if err := os.MkdirAll(filepath.Dir(path), privateCapsuleDirMode); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, privateCapsuleFileMode)
	if err != nil {
		return err
	}
	// OpenFile preserves an existing file's mode. Tighten it before writing so
	// reused capsule paths never expose newly captured logs through old modes.
	if err := file.Chmod(privateCapsuleFileMode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func readCapsuleManifest(path string) (capsuleManifest, error) {
	path = capsuleManifestPath(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return capsuleManifest{}, err
	}
	var manifest capsuleManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return capsuleManifest{}, err
	}
	if manifest.CapsuleVersion != capsuleVersion {
		return capsuleManifest{}, exit(2, "unsupported capsule_version=%d in %s", manifest.CapsuleVersion, path)
	}
	if manifest.Class == "" {
		return capsuleManifest{}, exit(2, "capsule %s is missing class", path)
	}
	return manifest, nil
}

func capsuleManifestPath(path string) string {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return filepath.Join(path, capsuleManifestFileName)
	}
	return path
}

func remoteReplayExitCode(err error) (int, bool) {
	var exitErr ExitError
	if !AsExitError(err, &exitErr) {
		return 0, false
	}
	message := strings.TrimSpace(exitErr.Message)
	if strings.Contains(strings.ToLower(message), " failed:") {
		return 0, false
	}
	fields := strings.Fields(message)
	if len(fields) < 3 || fields[len(fields)-2] != "exited" {
		return 0, false
	}
	last := fields[len(fields)-1]
	code, err := strconv.Atoi(last)
	if err != nil || code < 0 {
		return 0, false
	}
	switch fields[len(fields)-3] {
	case "command", "run":
		return code, true
	default:
		return 0, false
	}
}

func capsuleReplayFailureOutcome(failureSignature, replayOutput string, code int) (string, string, bool) {
	signature := strings.TrimSpace(failureSignature)
	if signature == "" {
		return capsuleOutcomeFailReproduced, fmt.Sprintf("replay command exited %d", code), true
	}
	boundedOutput := boundString(replayOutput, capsuleReplayOutputMaxBytes)
	if strings.Contains(boundedOutput, signature) {
		return capsuleOutcomeFailReproduced, fmt.Sprintf("replay command exited %d and matched failure_signature", code), true
	}
	return capsuleOutcomeFailNew, fmt.Sprintf("replay command exited %d but failure_signature was not present in bounded replay output", code), false
}

func safePathComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "capsule"
	}
	return out
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func moveKnownFlagsBeforePositionals(args []string, valueFlags, boolFlags []string) []string {
	valueSet := map[string]bool{}
	for _, name := range valueFlags {
		valueSet[name] = true
	}
	boolSet := map[string]bool{}
	for _, name := range boolFlags {
		boolSet[name] = true
	}
	flags := []string{}
	positionals := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}
		name := strings.TrimPrefix(arg, "--")
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name = name[:eq]
		}
		if strings.HasPrefix(arg, "--") && valueSet[name] {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(arg, "--") && boolSet[name] {
			flags = append(flags, arg)
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}
