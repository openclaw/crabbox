package cli

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
	"github.com/aws/smithy-go"
)

const (
	testSSMTarget = "i-0123456789abcdef0"
	testSSMOwner  = "arn:aws:sts::123456789012:assumed-role/team/crabbox/session-a"
)

type fakeAWSSMSessionsAPI struct {
	pages        map[string]*ssm.DescribeSessionsOutput
	described    []*ssm.DescribeSessionsInput
	terminated   []string
	terminateErr map[string]error
	closed       map[string]bool
	keepActive   bool
}

func (f *fakeAWSSMSessionsAPI) DescribeSessions(_ context.Context, input *ssm.DescribeSessionsInput, _ ...func(*ssm.Options)) (*ssm.DescribeSessionsOutput, error) {
	copyInput := *input
	copyInput.Filters = append([]ssmtypes.SessionFilter(nil), input.Filters...)
	f.described = append(f.described, &copyInput)
	token := aws.ToString(input.NextToken)
	page := f.pages[token]
	if page == nil {
		return &ssm.DescribeSessionsOutput{}, nil
	}
	out := *page
	out.Sessions = nil
	for _, session := range page.Sessions {
		if !f.closed[aws.ToString(session.SessionId)] {
			out.Sessions = append(out.Sessions, session)
		}
	}
	return &out, nil
}

func (f *fakeAWSSMSessionsAPI) TerminateSession(_ context.Context, input *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	id := aws.ToString(input.SessionId)
	f.terminated = append(f.terminated, id)
	if f.closed == nil {
		f.closed = map[string]bool{}
	}
	if !f.keepActive {
		f.closed[id] = true
	}
	return &ssm.TerminateSessionOutput{SessionId: input.SessionId}, f.terminateErr[id]
}

func TestTerminateOwnedAWSSMSessionsScopesExactOwnerTargetAndPaginates(t *testing.T) {
	otherOwner := "arn:aws:sts::123456789012:assumed-role/team/crabbox/session-b"
	fake := &fakeAWSSMSessionsAPI{pages: map[string]*ssm.DescribeSessionsOutput{
		"": {
			Sessions: []ssmtypes.Session{
				ssmSession("owned-1", testSSMTarget, testSSMOwner),
				ssmSession("other-owner", testSSMTarget, otherOwner),
				ssmSession("other-target", "i-fedcba98765432100", testSSMOwner),
			},
			NextToken: aws.String("page-2"),
		},
		"page-2": {Sessions: []ssmtypes.Session{ssmSession("owned-2", testSSMTarget, testSSMOwner)}},
	}}
	if err := terminateOwnedAWSSMSessions(t.Context(), fake, testSSMTarget, testSSMOwner, []time.Duration{0}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.terminated, []string{"owned-1", "owned-2"}) {
		t.Fatalf("terminated=%v, want exact target and owner across all pages", fake.terminated)
	}
	if len(fake.described) != 4 {
		t.Fatalf("DescribeSessions calls=%d, want two initial pages and two verification pages", len(fake.described))
	}
	for _, input := range fake.described {
		if input.State != ssmtypes.SessionStateActive || !sessionFilterEquals(input.Filters, ssmtypes.SessionFilterKeyTargetId, testSSMTarget) || !sessionFilterEquals(input.Filters, ssmtypes.SessionFilterKeyOwner, testSSMOwner) {
			t.Fatalf("unsafe DescribeSessions scope: %+v", input)
		}
	}
}

func TestTerminateOwnedAWSSMSessionsTreatsAlreadyTerminalAsIdempotent(t *testing.T) {
	fake := &fakeAWSSMSessionsAPI{
		pages: map[string]*ssm.DescribeSessionsOutput{
			"": {Sessions: []ssmtypes.Session{ssmSession("already-terminal", testSSMTarget, testSSMOwner)}},
		},
		terminateErr: map[string]error{"already-terminal": &smithy.GenericAPIError{Code: "DoesNotExistException", Message: "gone"}},
	}
	if err := terminateOwnedAWSSMSessions(t.Context(), fake, testSSMTarget, testSSMOwner, []time.Duration{0}); err != nil {
		t.Fatal(err)
	}
}

func TestTerminateOwnedAWSSMSessionsFailsClosedForUnprovableOwner(t *testing.T) {
	fake := &fakeAWSSMSessionsAPI{pages: map[string]*ssm.DescribeSessionsOutput{
		"": {Sessions: []ssmtypes.Session{ssmSession("unknown-owner", testSSMTarget, "team/crabbox")}},
	}}
	if err := terminateOwnedAWSSMSessions(t.Context(), fake, testSSMTarget, testSSMOwner, []time.Duration{0}); err == nil {
		t.Fatal("unprovable owner scope should fail closed")
	}
	if len(fake.terminated) != 0 {
		t.Fatalf("unprovable owner session was terminated: %v", fake.terminated)
	}
}

func TestTerminateOwnedAWSSMSessionsRequiresVerifiedActiveZero(t *testing.T) {
	fake := &fakeAWSSMSessionsAPI{
		pages: map[string]*ssm.DescribeSessionsOutput{
			"": {Sessions: []ssmtypes.Session{ssmSession("still-active", testSSMTarget, testSSMOwner)}},
		},
		keepActive: true,
	}
	if err := terminateOwnedAWSSMSessions(t.Context(), fake, testSSMTarget, testSSMOwner, []time.Duration{0}); err == nil {
		t.Fatal("active session remaining after termination must fail verification")
	}
}

func ssmSession(id, target, owner string) ssmtypes.Session {
	return ssmtypes.Session{SessionId: aws.String(id), Target: aws.String(target), Owner: aws.String(owner)}
}

func sessionFilterEquals(filters []ssmtypes.SessionFilter, key ssmtypes.SessionFilterKey, value string) bool {
	for _, filter := range filters {
		if filter.Key == key && aws.ToString(filter.Value) == value {
			return true
		}
	}
	return false
}
