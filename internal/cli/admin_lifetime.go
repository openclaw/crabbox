package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CoordinatorLifetimeReadback struct {
	LeaseID           string `json:"leaseId"`
	Provider          string `json:"provider"`
	Region            string `json:"region"`
	InstanceID        string `json:"instanceId"`
	InstanceState     string `json:"instanceState"`
	RootVolumeID      string `json:"rootVolumeId"`
	RootVolumePresent *bool  `json:"rootVolumePresent,omitempty"`
	ObservedAt        string `json:"observedAt"`
}

func (c *CoordinatorClient) AdminExtendLifetime(ctx context.Context, id, owner, reason, expiresAt string) (CoordinatorLease, error) {
	var res struct {
		Lease CoordinatorLease `json:"lease"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/admin/leases/"+url.PathEscape(id)+"/extend-lifetime", map[string]string{
		"owner": owner, "reason": reason, "expiresAt": expiresAt,
	}, &res)
	return res.Lease, err
}

func (c *CoordinatorClient) AdminLifetimeReadback(ctx context.Context, id string) (CoordinatorLifetimeReadback, error) {
	var res CoordinatorLifetimeReadback
	err := c.doRead(ctx, "/v1/admin/leases/"+url.PathEscape(id)+"/lifetime-readback", &res)
	return res, err
}

func (a App) adminExtendLifetime(ctx context.Context, args []string) error {
	fs := newFlagSet("admin extend-lifetime", a.Stderr)
	id := fs.String("id", "", "canonical lease ID")
	owner := fs.String("owner", "", "responsible owner for this extension")
	reason := fs.String("reason", "", "reason for the extension")
	expiresAt := fs.String("expires-at", "", "new RFC3339 compute-stop deadline (within seven days)")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !canonicalLeaseIDPattern.MatchString(*id) || strings.TrimSpace(*owner) == "" || strings.TrimSpace(*reason) == "" {
		return Exit(2, "usage: crabbox admin extend-lifetime --id <canonical-lease-id> --owner <owner> --reason <reason> --expires-at <RFC3339> [--json]")
	}
	deadline, err := time.Parse(time.RFC3339, *expiresAt)
	if err != nil {
		return Exit(2, "--expires-at must be an RFC3339 timestamp")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	lease, err := coord.AdminExtendLifetime(ctx, *id, strings.TrimSpace(*owner), strings.TrimSpace(*reason), deadline.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(lease)
	}
	fmt.Fprintf(a.Stdout, "extended %s%s\n", lease.ID, lifetimePolicySummary(lease.LifetimePolicy))
	return nil
}

func (a App) adminLifetimeReadback(ctx context.Context, args []string) error {
	fs := newFlagSet("admin lifetime-readback", a.Stderr)
	id := fs.String("id", "", "canonical lease ID")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !canonicalLeaseIDPattern.MatchString(*id) {
		return Exit(2, "usage: crabbox admin lifetime-readback --id <canonical-lease-id> [--json]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	readback, err := coord.AdminLifetimeReadback(ctx, *id)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(readback)
	}
	present := "unknown"
	if readback.RootVolumePresent != nil {
		present = fmt.Sprint(*readback.RootVolumePresent)
	}
	fmt.Fprintf(a.Stdout, "%s provider=%s region=%s instance=%s state=%s root_volume=%s root_volume_present=%s observed=%s\n",
		readback.LeaseID, readback.Provider, readback.Region, readback.InstanceID, readback.InstanceState, readback.RootVolumeID, present, readback.ObservedAt)
	return nil
}
