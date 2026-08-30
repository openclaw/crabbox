package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type coordinatorCheckpointRetention struct {
	Mode             string `json:"mode"`
	UnusedForSeconds int64  `json:"unusedForSeconds,omitempty"`
}

type coordinatorCheckpointScope struct {
	Region         string `json:"region"`
	AccountID      string `json:"accountID,omitempty"`
	SubscriptionID string `json:"subscriptionID,omitempty"`
	ResourceGroup  string `json:"resourceGroup,omitempty"`
	Project        string `json:"project,omitempty"`
}

type coordinatorCheckpointImage struct {
	ID           string   `json:"id"`
	ResourceID   string   `json:"resourceID"`
	Kind         string   `json:"kind"`
	ImmutableID  string   `json:"immutableID"`
	SnapshotIDs  []string `json:"snapshotIDs,omitempty"`
	State        string   `json:"state"`
	Architecture string   `json:"architecture,omitempty"`
}

type coordinatorCheckpoint struct {
	ID             string                         `json:"id"`
	Owner          string                         `json:"owner,omitempty"`
	LeaseID        string                         `json:"leaseID"`
	Provider       string                         `json:"provider"`
	Scope          coordinatorCheckpointScope     `json:"scope"`
	Name           string                         `json:"name"`
	Strategy       string                         `json:"strategy"`
	NoReboot       bool                           `json:"noReboot"`
	Image          *coordinatorCheckpointImage    `json:"image,omitempty"`
	State          string                         `json:"state"`
	Retention      coordinatorCheckpointRetention `json:"retention"`
	Generation     int64                          `json:"generation"`
	CreatedAt      string                         `json:"createdAt"`
	LastUsedAt     string                         `json:"lastUsedAt"`
	Target         string                         `json:"target"`
	WindowsMode    string                         `json:"windowsMode,omitempty"`
	Desktop        bool                           `json:"desktop,omitempty"`
	ServerType     string                         `json:"serverType,omitempty"`
	HostID         string                         `json:"hostID,omitempty"`
	Workdir        string                         `json:"workdir,omitempty"`
	Slug           string                         `json:"slug,omitempty"`
	ActiveUseCount int                            `json:"activeUseCount"`
	PinCount       int                            `json:"pinCount"`
	Repo           struct {
		Name      string `json:"name,omitempty"`
		Head      string `json:"head,omitempty"`
		BaseRef   string `json:"baseRef,omitempty"`
		RemoteURL string `json:"remoteURL,omitempty"`
	} `json:"repo,omitempty"`
}

type coordinatorCheckpointUseClaim struct {
	Checkpoint coordinatorCheckpoint `json:"checkpoint"`
	Claim      string                `json:"claim"`
	ExpiresAt  string                `json:"expiresAt"`
}

type checkpointLeaseClaimContextKey struct{}
type checkpointCreateContextKey struct{}
type checkpointAdminContextKey struct{}

type checkpointCreateContext struct {
	Record           checkpointRecord
	Retention        coordinatorCheckpointRetention
	PersistOwnership func(bool) error
}

type checkpointLeaseClaim struct {
	CheckpointID string
	Token        string
	LeaseCreated func()
}

func withCheckpointLeaseClaim(ctx context.Context, checkpointID, token string) context.Context {
	return context.WithValue(ctx, checkpointLeaseClaimContextKey{}, checkpointLeaseClaim{
		CheckpointID: checkpointID,
		Token:        token,
	})
}

func withCheckpointLeaseProvisioned(ctx context.Context, checkpointID, token string, leaseCreated func()) context.Context {
	return context.WithValue(ctx, checkpointLeaseClaimContextKey{}, checkpointLeaseClaim{
		CheckpointID: checkpointID,
		Token:        token,
		LeaseCreated: leaseCreated,
	})
}

func checkpointLeaseClaimFromContext(ctx context.Context) (checkpointLeaseClaim, bool) {
	claim, ok := ctx.Value(checkpointLeaseClaimContextKey{}).(checkpointLeaseClaim)
	return claim, ok && claim.CheckpointID != "" && claim.Token != ""
}

func withCheckpointCreateContext(ctx context.Context, record checkpointRecord, retention coordinatorCheckpointRetention, persistOwnership func(bool) error) context.Context {
	return context.WithValue(ctx, checkpointCreateContextKey{}, checkpointCreateContext{
		Record:           record,
		Retention:        retention,
		PersistOwnership: persistOwnership,
	})
}

func checkpointCreateContextFrom(ctx context.Context) (checkpointCreateContext, bool) {
	value, ok := ctx.Value(checkpointCreateContextKey{}).(checkpointCreateContext)
	return value, ok
}

func withCheckpointAdmin(ctx context.Context, admin bool) context.Context {
	if !admin {
		return ctx
	}
	return context.WithValue(ctx, checkpointAdminContextKey{}, true)
}

func configuredCheckpointCoordinatorFor(ctx context.Context) (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	admin, _ := ctx.Value(checkpointAdminContextKey{}).(bool)
	if admin {
		if cfg.CoordAdminToken == "" {
			return nil, exit(2, "checkpoint --admin requires a configured coordinator admin token")
		}
		cfg.CoordToken = cfg.CoordAdminToken
		cfg.CoordTokenCommand = nil
	} else if cfg.CoordToken == "" && len(cfg.CoordTokenCommand) == 0 && cfg.CoordAdminToken != "" {
		cfg.CoordToken = cfg.CoordAdminToken
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "checkpoint operation requires a configured coordinator")
	}
	return coord, nil
}

func bindCheckpointCoordinatorCredential(ctx context.Context, cfg *Config) error {
	coord, err := configuredCheckpointCoordinatorFor(ctx)
	if err != nil {
		return err
	}
	cfg.CoordToken = coord.Token
	cfg.CoordTokenCommand = append([]string(nil), coord.TokenCommand...)
	return nil
}

func (c *CoordinatorClient) checkpointOperationError(ctx context.Context, operationErr error) error {
	if !checkpointRouteUnsupported(operationErr) {
		return operationErr
	}
	c.checkpointSupportMu.Lock()
	known, supported := c.checkpointSupportKnown, c.checkpointSupported
	c.checkpointSupportMu.Unlock()
	if !known {
		_, probeErr := c.Checkpoints(ctx)
		if probeErr != nil && !checkpointRouteUnsupported(probeErr) {
			return fmt.Errorf("probe coordinator checkpoint support: %w", probeErr)
		}
		c.checkpointSupportMu.Lock()
		known, supported = c.checkpointSupportKnown, c.checkpointSupported
		c.checkpointSupportMu.Unlock()
	}
	if known && !supported {
		return checkpointUpgradeRequired(operationErr)
	}
	return operationErr
}

type checkpointUpgradeRequiredError struct{ cause error }

func (e checkpointUpgradeRequiredError) Error() string {
	return fmt.Sprintf("coordinator does not support coordinator-managed checkpoints; upgrade the coordinator (%v)", e.cause)
}
func (e checkpointUpgradeRequiredError) Unwrap() error { return e.cause }
func checkpointUpgradeRequired(err error) error        { return checkpointUpgradeRequiredError{err} }
func isCoordinatorCheckpointNotFound(err error) bool {
	var unsupported checkpointUpgradeRequiredError
	return !errors.As(err, &unsupported) && isCoordinatorNotFound(err)
}

func checkpointRouteUnsupported(err error) bool {
	var httpErr CoordinatorHTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusMethodNotAllowed)
}

func (c *CoordinatorClient) CreateCheckpoint(ctx context.Context, record checkpointRecord, name, strategy string, noReboot bool, retention coordinatorCheckpointRetention) (coordinatorCheckpoint, CoordinatorImage, error) {
	var response struct {
		Checkpoint coordinatorCheckpoint `json:"checkpoint"`
		Image      CoordinatorImage      `json:"image"`
	}
	request := map[string]any{
		"id":        record.ID,
		"leaseID":   record.LeaseID,
		"name":      name,
		"strategy":  strategy,
		"noReboot":  noReboot,
		"retention": retention,
		"workdir":   record.Workdir,
		"repo": map[string]string{
			"name":    record.Repo.Name,
			"head":    record.Repo.Head,
			"baseRef": record.Repo.BaseRef,
		},
	}
	err := c.do(ctx, http.MethodPost, "/v1/checkpoints", request, &response)
	if err == nil {
		err = validateCoordinatorCheckpointResponse(record.ID, response.Checkpoint)
	}
	return response.Checkpoint, response.Image, err
}

func (c *CoordinatorClient) Checkpoints(ctx context.Context) ([]coordinatorCheckpoint, error) {
	var response struct {
		Checkpoints []coordinatorCheckpoint `json:"checkpoints"`
	}
	err := c.doControl(ctx, http.MethodGet, "/v1/checkpoints", nil, &response)
	if err == nil && response.Checkpoints == nil {
		err = fmt.Errorf("coordinator checkpoint inventory must contain a checkpoint array")
	}
	c.checkpointSupportMu.Lock()
	if err == nil {
		c.checkpointSupportKnown, c.checkpointSupported = true, true
	} else if checkpointRouteUnsupported(err) {
		c.checkpointSupportKnown, c.checkpointSupported = true, false
	}
	c.checkpointSupportMu.Unlock()
	return response.Checkpoints, err
}

func (c *CoordinatorClient) Checkpoint(ctx context.Context, id string) (coordinatorCheckpoint, error) {
	var response struct {
		Checkpoint coordinatorCheckpoint `json:"checkpoint"`
	}
	err := c.do(ctx, http.MethodGet, checkpointCoordinatorPath(id, ""), nil, &response)
	if err == nil {
		err = validateCoordinatorCheckpointResponse(id, response.Checkpoint)
	}
	return response.Checkpoint, c.checkpointOperationError(ctx, err)
}

func (c *CoordinatorClient) CheckpointImage(ctx context.Context, id string) (CoordinatorImage, error) {
	var response struct {
		Image CoordinatorImage `json:"image"`
	}
	err := c.do(ctx, http.MethodGet, checkpointCoordinatorPath(id, "")+"?verify=true", nil, &response)
	return response.Image, c.checkpointOperationError(ctx, err)
}

func (c *CoordinatorClient) UpdateCheckpointRetention(ctx context.Context, id string, retention coordinatorCheckpointRetention) (coordinatorCheckpoint, error) {
	var response struct {
		Checkpoint coordinatorCheckpoint `json:"checkpoint"`
	}
	err := c.do(ctx, http.MethodPatch, checkpointCoordinatorPath(id, "retention"), map[string]any{"retention": retention}, &response)
	if err == nil {
		err = validateCoordinatorCheckpointResponse(id, response.Checkpoint)
	}
	return response.Checkpoint, c.checkpointOperationError(ctx, err)
}

func (c *CoordinatorClient) BeginCheckpointUse(ctx context.Context, id string) (coordinatorCheckpointUseClaim, error) {
	var response coordinatorCheckpointUseClaim
	err := c.do(ctx, http.MethodPost, checkpointCoordinatorPath(id, "use"), map[string]any{"action": "begin"}, &response)
	err = c.checkpointOperationError(ctx, err)
	if err == nil && response.Claim == "" {
		return coordinatorCheckpointUseClaim{}, fmt.Errorf("coordinator did not return a checkpoint use claim")
	}
	return response, err
}

func (c *CoordinatorClient) checkpointUseAction(ctx context.Context, id, token, action string) (coordinatorCheckpoint, string, error) {
	var response struct {
		Checkpoint coordinatorCheckpoint `json:"checkpoint"`
		ExpiresAt  string                `json:"expiresAt"`
	}
	err := c.do(ctx, http.MethodPost, checkpointCoordinatorPath(id, "use"), map[string]any{
		"action": action,
		"claim":  token,
	}, &response)
	return response.Checkpoint, response.ExpiresAt, c.checkpointOperationError(ctx, err)
}

func (c *CoordinatorClient) DeleteCheckpoint(ctx context.Context, id string) error {
	var response struct {
		CheckpointID string `json:"checkpointID"`
		Deleted      bool   `json:"deleted"`
	}
	err := c.do(ctx, http.MethodDelete, checkpointCoordinatorPath(id, ""), nil, &response)
	err = c.checkpointOperationError(ctx, err)
	if err == nil && (!response.Deleted || response.CheckpointID != id) {
		return fmt.Errorf("coordinator did not confirm deletion of checkpoint %s", id)
	}
	return err
}

func validateCoordinatorCheckpointResponse(id string, checkpoint coordinatorCheckpoint) error {
	if checkpoint.ID != id {
		return fmt.Errorf("coordinator returned checkpoint %q for requested checkpoint %q", checkpoint.ID, id)
	}
	return nil
}

func checkpointCoordinatorPath(id, action string) string {
	path := "/v1/checkpoints/" + url.PathEscape(id)
	if action != "" {
		path += "/" + action
	}
	return path
}

func checkpointCoordinatorOrigin(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.User = nil
	parsed.Path = path.Clean("/" + strings.TrimPrefix(parsed.Path, "/"))
	if parsed.Path == "/" {
		parsed.Path = ""
	}
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func checkpointRetentionFromDuration(duration time.Duration) coordinatorCheckpointRetention {
	if duration <= 0 {
		return coordinatorCheckpointRetention{Mode: "manual"}
	}
	return coordinatorCheckpointRetention{Mode: "expire-unused", UnusedForSeconds: int64(duration / time.Second)}
}

func checkpointRecordFromCoordinator(checkpoint coordinatorCheckpoint, origin string) (checkpointRecord, error) {
	if _, err := validateCheckpointID(checkpoint.ID); err != nil {
		return checkpointRecord{}, err
	}
	if checkpoint.Image == nil && checkpoint.State != "creating" && checkpoint.State != "failed" {
		return checkpointRecord{}, exit(2, "coordinator checkpoint %s has no verified provider resource", checkpoint.ID)
	}
	if checkpoint.Image != nil && (checkpoint.Image.ID == "" || checkpoint.Image.ResourceID == "" || checkpoint.Image.ImmutableID == "") {
		return checkpointRecord{}, exit(2, "coordinator checkpoint %s has no verified provider resource", checkpoint.ID)
	}
	kind := checkpoint.State
	if checkpoint.Image != nil {
		kind = checkpoint.Image.Kind
	}
	record := checkpointRecord{
		ID:               checkpoint.ID,
		Name:             checkpoint.Name,
		Kind:             kind,
		CreatedAt:        checkpoint.CreatedAt,
		LastUsedAt:       checkpoint.LastUsedAt,
		CrabboxVersion:   currentVersion(),
		Provider:         checkpoint.Provider,
		LeaseID:          checkpoint.LeaseID,
		Slug:             checkpoint.Slug,
		TargetOS:         checkpoint.Target,
		WindowsMode:      checkpoint.WindowsMode,
		Desktop:          checkpoint.Desktop,
		ServerType:       checkpoint.ServerType,
		HostID:           checkpoint.HostID,
		Workdir:          checkpoint.Workdir,
		Ownership:        &checkpointOwnership{Mode: "coordinator", Origin: origin},
		Retention:        &checkpoint.Retention,
		CoordinatorState: checkpoint.State,
	}
	if checkpoint.Image != nil {
		record.Native.Provider = checkpoint.Provider
		record.Native.ImageID = checkpoint.Image.ID
		record.Native.Kind = checkpoint.Image.Kind
		record.Native.Name = checkpoint.Name
		record.Native.State = checkpoint.Image.State
		record.Native.Region = checkpoint.Scope.Region
		record.Native.AccountID = checkpoint.Scope.AccountID
		record.Native.Project = checkpoint.Scope.Project
		record.Native.Resource = checkpoint.Image.ResourceID
		record.Native.Architecture = checkpoint.Image.Architecture
		record.Native.SnapshotIDs = checkpoint.Image.SnapshotIDs
		record.Native.Strategy = checkpoint.Strategy
		record.Native.NoReboot = checkpoint.NoReboot
	}
	record.Repo.Name = checkpoint.Repo.Name
	record.Repo.Head = checkpoint.Repo.Head
	record.Repo.BaseRef = checkpoint.Repo.BaseRef
	record.Repo.RemoteURL = checkpoint.Repo.RemoteURL
	if err := validateCheckpointRecordTimes(record); err != nil {
		return checkpointRecord{}, err
	}
	return record, nil
}
