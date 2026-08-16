package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	localClaimsListVersion        = 1
	localClaimsListSource         = "local-claims"
	maxLocalClaimProblems         = 100
	maxLocalClaimProblemFileBytes = 160
)

type localClaimsListOutput struct {
	Version  int                 `json:"version"`
	Source   string              `json:"source"`
	Claims   []localClaimView    `json:"claims"`
	Problems []localClaimProblem `json:"problems"`
}

type localClaimView struct {
	LeaseID            string `json:"leaseId"`
	Slug               string `json:"slug"`
	Provider           string `json:"provider"`
	RepoRoot           string `json:"repoRoot"`
	Pond               string `json:"pond"`
	TargetOS           string `json:"targetOS"`
	WindowsMode        string `json:"windowsMode"`
	ClaimedAt          string `json:"claimedAt"`
	LastUsedAt         string `json:"lastUsedAt"`
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds"`
}

type localClaimProblem struct {
	File    string `json:"file"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a App) claimsList(args []string) error {
	fs := newFlagSet("claims list", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return exit(2, "claims list does not accept positional arguments")
	}

	snapshot, err := snapshotLeaseClaims()
	if err != nil {
		return err
	}
	output := projectLocalClaims(snapshot)
	if *jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(output); err != nil {
			return err
		}
	} else {
		if err := renderLocalClaims(a.Stdout, output); err != nil {
			return err
		}
	}
	if len(snapshot.invalid) > 0 {
		return ExitError{Code: 2}
	}
	return nil
}

func projectLocalClaims(snapshot leaseClaimsSnapshot) localClaimsListOutput {
	claims := make([]localClaimView, 0, len(snapshot.claims))
	for _, claim := range snapshot.claims {
		claims = append(claims, localClaimView{
			LeaseID:            claim.LeaseID,
			Slug:               claim.Slug,
			Provider:           claim.Provider,
			RepoRoot:           claim.RepoRoot,
			Pond:               claim.Pond,
			TargetOS:           claim.TargetOS,
			WindowsMode:        claim.WindowsMode,
			ClaimedAt:          claim.ClaimedAt,
			LastUsedAt:         claim.LastUsedAt,
			IdleTimeoutSeconds: claim.IdleTimeoutSeconds,
		})
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].LeaseID != claims[j].LeaseID {
			return claims[i].LeaseID < claims[j].LeaseID
		}
		if claims[i].Provider != claims[j].Provider {
			return claims[i].Provider < claims[j].Provider
		}
		return claims[i].Slug < claims[j].Slug
	})

	problems := make([]localClaimProblem, 0, len(snapshot.invalid))
	for leaseID, err := range snapshot.invalid {
		code := "invalid_claim"
		var fileErr *leaseClaimFileError
		if errors.As(err, &fileErr) {
			code = fileErr.code
		}
		problems = append(problems, localClaimProblem{
			File:    localClaimProblemFile(leaseID),
			Code:    code,
			Message: localClaimProblemMessage(code),
		})
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].File != problems[j].File {
			return problems[i].File < problems[j].File
		}
		return problems[i].Code < problems[j].Code
	})
	if len(problems) > maxLocalClaimProblems {
		omitted := len(problems) - (maxLocalClaimProblems - 1)
		problems = append(problems[:maxLocalClaimProblems-1], localClaimProblem{
			Code:    "problems_truncated",
			Message: fmt.Sprintf("%d additional malformed claim files omitted", omitted),
		})
	}

	return localClaimsListOutput{
		Version:  localClaimsListVersion,
		Source:   localClaimsListSource,
		Claims:   claims,
		Problems: problems,
	}
}

func localClaimProblemFile(leaseID string) string {
	name := leaseID + ".json"
	if validLeaseClaimID(leaseID) && len(name) <= maxLocalClaimProblemFileBytes {
		return RedactDiagnosticSecrets(name)
	}
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("sha256:%x", digest[:8])
}

func localClaimProblemMessage(code string) string {
	switch code {
	case "invalid_filename":
		return "claim filename is not a valid lease id; filename replaced by a fingerprint"
	case "invalid_json":
		return "claim file is not valid JSON"
	case "empty_lease_id":
		return "claim file has a missing or empty leaseId"
	case "lease_id_mismatch":
		return "claim filename and payload leaseId do not match"
	case "read_error":
		return "claim file could not be read"
	case "non_regular_file":
		return "claim path is not a regular file"
	default:
		return "claim file is invalid"
	}
}

func renderLocalClaims(stdout io.Writer, output localClaimsListOutput) error {
	var rendered strings.Builder
	fmt.Fprintln(&rendered, "Local claims (unverified local state; provider backends were not queried)")
	if len(output.Claims) == 0 {
		fmt.Fprintln(&rendered, "No valid local claims found.")
	} else {
		for _, claim := range output.Claims {
			fmt.Fprintf(&rendered, "- leaseId: %s\n", strconv.QuoteToGraphic(claim.LeaseID))
			fmt.Fprintf(&rendered, "  slug: %s\n", strconv.QuoteToGraphic(claim.Slug))
			fmt.Fprintf(&rendered, "  provider: %s\n", strconv.QuoteToGraphic(claim.Provider))
			fmt.Fprintf(&rendered, "  repoRoot: %s\n", strconv.QuoteToGraphic(claim.RepoRoot))
			fmt.Fprintf(&rendered, "  pond: %s\n", strconv.QuoteToGraphic(claim.Pond))
			fmt.Fprintf(&rendered, "  targetOS: %s\n", strconv.QuoteToGraphic(claim.TargetOS))
			fmt.Fprintf(&rendered, "  windowsMode: %s\n", strconv.QuoteToGraphic(claim.WindowsMode))
			fmt.Fprintf(&rendered, "  claimedAt: %s\n", strconv.QuoteToGraphic(claim.ClaimedAt))
			fmt.Fprintf(&rendered, "  lastUsedAt: %s\n", strconv.QuoteToGraphic(claim.LastUsedAt))
			fmt.Fprintf(&rendered, "  idleTimeoutSeconds: %d\n", claim.IdleTimeoutSeconds)
		}
	}
	if len(output.Problems) > 0 {
		fmt.Fprintln(&rendered, "Problems:")
		for _, problem := range output.Problems {
			if problem.File == "" {
				fmt.Fprintf(&rendered, "- %s (%s)\n", problem.Message, problem.Code)
				continue
			}
			fmt.Fprintf(&rendered, "- %s: %s (%s)\n", strconv.QuoteToGraphic(problem.File), problem.Message, problem.Code)
		}
	}
	_, err := io.WriteString(stdout, rendered.String())
	return err
}
