package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type CoordinatorCreateRetirement struct {
	LeaseID      string `json:"leaseId"`
	Revision     string `json:"revision"`
	Binding      string `json:"binding"`
	AttemptState string `json:"attemptState"`
	Owner        string `json:"owner,omitempty"`
	Org          string `json:"org,omitempty"`
	CreatedAt    string `json:"createdAt,omitempty"`
	Fenced       bool   `json:"fenced"`
	Cleanup      string `json:"cleanup"`
}

func (a App) adminRetireCreate(ctx context.Context, args []string) error {
	fs := newFlagSet("admin retire-create", a.Stderr)
	id := fs.String("id", "", "canonical requested lease ID")
	force := fs.Bool("force", false, "permanently retire the reviewed request ID")
	revision := fs.String("revision", "", "exact revision returned by the preview")
	owner := fs.String("owner", "", "expected original owner")
	org := fs.String("org", "", "expected canonical org key")
	before := fs.String("created-before", "", "latest permitted original creation time (RFC3339)")
	reason := fs.String("reason", "", "reason for retiring this request")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !canonicalLeaseIDPattern.MatchString(*id) {
		return Exit(2, "usage: crabbox admin retire-create --id <canonical-lease-id> [--json] [--force --revision <preview-revision> --owner <owner> --org <org-key> --created-before <RFC3339> --reason <reason>]")
	}
	var body map[string]string
	if *force {
		deadline, err := time.Parse(time.RFC3339, *before)
		if err != nil || deadline.After(time.Now()) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(*revision) || strings.TrimSpace(*owner) == "" || strings.TrimSpace(*org) == "" || strings.TrimSpace(*reason) == "" {
			return Exit(2, "--force requires the preview revision, original owner/org, a past --created-before timestamp, and --reason")
		}
		body = map[string]string{"revision": *revision, "owner": *owner, "org": *org, "createdBefore": deadline.UTC().Format(time.RFC3339Nano), "reason": *reason}
	} else if *revision != "" || *owner != "" || *org != "" || *before != "" || *reason != "" {
		return Exit(2, "retirement arguments require --force; omit them to preview the exact request")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	path := "/v1/admin/leases/" + url.PathEscape(*id) + "/retire-create"
	var result CoordinatorCreateRetirement
	if *force {
		err = coord.do(ctx, http.MethodPost, path, body, &result)
	} else {
		err = coord.doRead(ctx, path, &result)
	}
	if err != nil {
		return err
	}
	if result.LeaseID != *id || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(result.Revision) {
		return Exit(1, "coordinator returned an invalid create-retirement identity; recovery remains unresolved")
	}
	if *jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(result); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.Stdout, "%s binding=%s attempt=%s fenced=%t cleanup=%s revision=%s owner=%q org=%q created=%s\n", result.LeaseID, result.Binding, result.AttemptState, result.Fenced, result.Cleanup, result.Revision, result.Owner, result.Org, result.CreatedAt)
	}
	complete := result.Fenced && result.AttemptState == "canceled" &&
		((result.Binding == "unbound" && result.Cleanup == "not-allocated") ||
			(result.Binding == "bound" && result.Cleanup == "confirmed"))
	if *force && !complete {
		return Exit(1, "create retirement is not cleanup-complete; inspect with crabbox admin retire-create --id %s --json", *id)
	}
	return nil
}
