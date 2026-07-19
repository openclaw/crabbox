package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

func (a App) share(ctx context.Context, args []string) error {
	fs := newFlagSet("share", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	var users stringListFlag
	fs.Var(&users, "user", "owner identity to share with; repeatable")
	org := fs.Bool("org", false, "share with the lease org")
	role := fs.String("role", string(CoordinatorShareUse), "role: use or manage")
	list := fs.Bool("list", false, "list current sharing")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox share --id <lease-id-or-slug> [--user <owner>|--org|--list]")
	}
	shareRole, err := parseCoordinatorShareRole(*role)
	if err != nil {
		return err
	}
	coord, err := shareCoordinator()
	if err != nil {
		return err
	}
	current, err := coord.LeaseShare(ctx, *id)
	if err != nil {
		return err
	}
	if *list || (len(users) == 0 && !*org) {
		return printCoordinatorShare(a.Stdout, current, *jsonOut)
	}
	if current.Users == nil {
		current.Users = map[string]CoordinatorShareRole{}
	}
	for _, user := range users {
		normalized := normalizeShareOwner(user)
		if normalized == "" {
			return exit(2, "invalid empty --user")
		}
		current.Users[normalized] = shareRole
	}
	if *org {
		current.Org = shareRole
	}
	updated, err := coord.UpdateLeaseShare(ctx, *id, current)
	if err != nil {
		return err
	}
	return printCoordinatorShare(a.Stdout, updated, *jsonOut)
}

func (a App) unshare(ctx context.Context, args []string) error {
	fs := newFlagSet("unshare", a.Stderr)
	id := fs.String("id", "", "lease id or slug")
	var users stringListFlag
	fs.Var(&users, "user", "owner identity to remove; repeatable")
	org := fs.Bool("org", false, "remove org sharing")
	all := fs.Bool("all", false, "remove all sharing")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	setIDFromFirstArg(fs, id)
	if *id == "" {
		return exit(2, "usage: crabbox unshare --id <lease-id-or-slug> [--user <owner>|--org|--all]")
	}
	if len(users) == 0 && !*org && !*all {
		return exit(2, "usage: crabbox unshare --id <lease-id-or-slug> [--user <owner>|--org|--all]")
	}
	coord, err := shareCoordinator()
	if err != nil {
		return err
	}
	var updated CoordinatorShare
	if *all {
		updated, err = coord.DeleteLeaseShare(ctx, *id, "", false)
	} else {
		updated, err = coord.LeaseShare(ctx, *id)
		if err == nil {
			if updated.Users == nil {
				updated.Users = map[string]CoordinatorShareRole{}
			}
			for _, user := range users {
				delete(updated.Users, normalizeShareOwner(user))
			}
			if *org {
				updated.Org = ""
			}
			updated, err = coord.UpdateLeaseShare(ctx, *id, updated)
		}
	}
	if err != nil {
		return err
	}
	return printCoordinatorShare(a.Stdout, updated, *jsonOut)
}

func shareCoordinator() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "share requires a configured coordinator")
	}
	return coord, nil
}

func parseCoordinatorShareRole(value string) (CoordinatorShareRole, error) {
	switch CoordinatorShareRole(strings.TrimSpace(value)) {
	case CoordinatorShareUse:
		return CoordinatorShareUse, nil
	case CoordinatorShareManage:
		return CoordinatorShareManage, nil
	default:
		return "", exit(2, "share role must be use or manage")
	}
}

func normalizeShareOwner(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func printCoordinatorShare(out interface{ Write([]byte) (int, error) }, share CoordinatorShare, jsonOut bool) error {
	if jsonOut {
		return json.NewEncoder(out).Encode(map[string]any{"share": share})
	}
	if share.Org != "" {
		fmt.Fprintf(out, "org=%s\n", share.Org)
	} else {
		fmt.Fprintln(out, "org=off")
	}
	users := make([]string, 0, len(share.Users))
	for user := range share.Users {
		users = append(users, user)
	}
	sort.Strings(users)
	if len(users) == 0 {
		fmt.Fprintln(out, "users=none")
		return nil
	}
	for _, user := range users {
		fmt.Fprintf(out, "user=%s role=%s\n", user, share.Users[user])
	}
	return nil
}

func ensureOpenWebVNCPortalAccess(ctx context.Context, coord *CoordinatorClient, id string, openPortal bool, out interface{ Write([]byte) (int, error) }) error {
	if !openPortal {
		return nil
	}
	current, err := coord.LeaseShare(ctx, id)
	if err != nil {
		return err
	}
	if current.Org == CoordinatorShareUse || current.Org == CoordinatorShareManage {
		return nil
	}
	current.Org = CoordinatorShareUse
	if _, err := coord.UpdateLeaseShare(ctx, id, current); err != nil {
		var httpErr CoordinatorHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden {
			if out != nil {
				fmt.Fprintln(out, "portal share: skipped (lease manage access required)")
			}
			return nil
		}
		return err
	}
	if out != nil {
		fmt.Fprintln(out, "portal share: org=use")
	}
	return nil
}
