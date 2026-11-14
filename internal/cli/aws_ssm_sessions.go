package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

const awsSSMSessionCleanupTimeout = 20 * time.Second

var awsSSMSessionVerifyDelays = []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, time.Second}

type awsSSMSessionsAPI interface {
	DescribeSessions(context.Context, *ssm.DescribeSessionsInput, ...func(*ssm.Options)) (*ssm.DescribeSessionsOutput, error)
	TerminateSession(context.Context, *ssm.TerminateSessionInput, ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error)
}

func (c *AWSClient) TerminateOwnedSSMSessions(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("AWS SSM cleanup target is empty")
	}
	if c.ssm == nil || c.sts == nil {
		return errors.New("AWS SSM cleanup clients are unavailable")
	}
	identity, err := c.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("resolve AWS SSM session owner: %w", err)
	}
	owner, err := canonicalAWSSessionOwnerARN(aws.ToString(identity.Arn))
	if err != nil {
		return fmt.Errorf("resolve AWS SSM session owner: %w", err)
	}
	if account := strings.TrimSpace(aws.ToString(identity.Account)); account == "" || account != arnAccount(owner) {
		return errors.New("AWS SSM session owner account does not match caller identity")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, awsSSMSessionCleanupTimeout)
	defer cancel()
	return terminateOwnedAWSSMSessions(cleanupCtx, c.ssm, target, owner, awsSSMSessionVerifyDelays)
}

func canonicalAWSSessionOwnerARN(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", errors.New("AWS caller ARN is empty or malformed")
	}
	parsed, err := arn.Parse(value)
	if err != nil || parsed.Partition == "" || parsed.AccountID == "" || parsed.Resource == "" {
		return "", errors.New("AWS caller ARN is malformed")
	}
	if len(parsed.AccountID) != 12 || strings.Trim(parsed.AccountID, "0123456789") != "" {
		return "", errors.New("AWS caller ARN account is malformed")
	}
	switch parsed.Service {
	case "iam":
		if parsed.Resource != "root" && !strings.HasPrefix(parsed.Resource, "user/") && !strings.HasPrefix(parsed.Resource, "role/") {
			return "", errors.New("AWS caller ARN is not an IAM principal")
		}
	case "sts":
		if !strings.HasPrefix(parsed.Resource, "assumed-role/") && !strings.HasPrefix(parsed.Resource, "federated-user/") {
			return "", errors.New("AWS caller ARN is not an STS session principal")
		}
	default:
		return "", errors.New("AWS caller ARN is not an IAM or STS principal")
	}
	return parsed.String(), nil
}

func arnAccount(value string) string {
	parsed, err := arn.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.AccountID
}

func terminateOwnedAWSSMSessions(ctx context.Context, client awsSSMSessionsAPI, target, owner string, verifyDelays []time.Duration) error {
	canonicalOwner, err := canonicalAWSSessionOwnerARN(owner)
	if err != nil {
		return err
	}
	sessions, err := listOwnedActiveAWSSessions(ctx, client, target, canonicalOwner)
	if err != nil {
		return err
	}
	var terminateErr error
	for _, session := range sessions {
		_, err := client.TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: aws.String(aws.ToString(session.SessionId))})
		if err != nil && awsAPIErrorCode(err) != "DoesNotExistException" {
			terminateErr = errors.Join(terminateErr, fmt.Errorf("terminate AWS SSM session %s: %w", aws.ToString(session.SessionId), err))
		}
	}
	var active []ssmtypes.Session
	for _, delay := range verifyDelays {
		if delay > 0 {
			if err := sleepContext(ctx, delay); err != nil {
				return errors.Join(terminateErr, context.Cause(ctx))
			}
		}
		active, err = listOwnedActiveAWSSessions(ctx, client, target, canonicalOwner)
		if err != nil {
			return errors.Join(terminateErr, err)
		}
		if len(active) == 0 {
			return terminateErr
		}
	}
	return errors.Join(terminateErr, fmt.Errorf("AWS SSM cleanup still has %d active session(s) for the exact instance and caller", len(active)))
}

func listOwnedActiveAWSSessions(ctx context.Context, client awsSSMSessionsAPI, target, owner string) ([]ssmtypes.Session, error) {
	input := &ssm.DescribeSessionsInput{
		State: ssmtypes.SessionStateActive,
		Filters: []ssmtypes.SessionFilter{
			{Key: ssmtypes.SessionFilterKeyTargetId, Value: aws.String(target)},
			{Key: ssmtypes.SessionFilterKeyOwner, Value: aws.String(owner)},
		},
	}
	paginator := ssm.NewDescribeSessionsPaginator(client, input, func(options *ssm.DescribeSessionsPaginatorOptions) {
		options.StopOnDuplicateToken = true
	})
	var sessions []ssmtypes.Session
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list active AWS SSM sessions: %w", err)
		}
		for _, session := range page.Sessions {
			if aws.ToString(session.Target) != target {
				continue
			}
			sessionOwner, ownerErr := canonicalAWSSessionOwnerARN(aws.ToString(session.Owner))
			if ownerErr != nil {
				return nil, fmt.Errorf("cannot prove AWS SSM session owner scope: %w", ownerErr)
			}
			if sessionOwner != owner {
				continue
			}
			if strings.TrimSpace(aws.ToString(session.SessionId)) == "" {
				return nil, errors.New("cannot terminate owned AWS SSM session without a session id")
			}
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}
