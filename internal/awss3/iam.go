package awss3

import (
	"context"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsiam "github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// The IAM side of a BucketAccess (NKs3-1b): one IAM user per claim, tagged with
// the claim (s3client.MarkerTag), holding an inline policy that grants the
// claim's access to its bucket, and the access keys the binding hands out.
// IAM generates the keys, so a Store is a controller.GeneratedKeyStore, not a
// store of chosen keys like RustFS.
//
// IAM is eventually consistent: a user or key made a moment ago may not be
// visible to the next call. A call that finds an entity missing, or a change in
// flight, right after the Store made it returns a lifecycle.PendingError, so
// the claim is looked at again shortly and no call ever waits.

// pendingAfter is how soon a claim whose IAM change has not settled is looked
// at again.
const pendingAfter = 5 * time.Second

const (
	codeNoSuchEntity           = "NoSuchEntity"
	codeEntityAlreadyExists    = "EntityAlreadyExists"
	codeConcurrentModification = "ConcurrentModification"
	pendingMessage             = "waiting for IAM to settle"
)

// Option configures a Store.
type Option func(*Store)

// WithIAM sets the path and the permissions boundary (a policy ARN) of the IAM
// users the Store creates; an empty value leaves IAM's default (path "/", no
// boundary). They apply to a user when it is created.
func WithIAM(path, boundary string) Option {
	return func(s *Store) { s.iamPath, s.boundary = path, boundary }
}

// iamFail is fail for an IAM call: an entity that is not there yet, or a change
// still in flight, is IAM settling and a PendingError, not a failure.
func iamFail(ctx context.Context, op string, err error) error {
	if c := codeOf(err); c == codeNoSuchEntity || c == codeEntityAlreadyExists || c == codeConcurrentModification {
		log.FromContext(ctx).V(1).Info("IAM has not settled", "request", op, "code", c)
		return lifecycle.Pending(pendingAfter, "", pendingMessage)
	}
	return fail(ctx, op, err)
}

// User reads the IAM user: its ARN, the claim its tag names and its inline
// policies. A user that does not exist is the zero state.
func (s *Store) User(ctx context.Context, name string) (s3client.UserState, error) {
	out, err := s.idp.GetUser(ctx, &awsiam.GetUserInput{UserName: aws.String(name)})
	if codeOf(err) == codeNoSuchEntity {
		return s3client.UserState{}, nil
	}
	if err != nil {
		return s3client.UserState{}, fail(ctx, "IAM GetUser", err)
	}
	policies, err := s.policyNames(ctx, name)
	if err != nil {
		return s3client.UserState{}, err
	}
	st := s3client.UserState{Exists: true, Enabled: true, Policies: policies, ARN: aws.ToString(out.User.Arn)}
	for _, t := range out.User.Tags {
		if aws.ToString(t.Key) == s3client.MarkerTag {
			st.Marker = aws.ToString(t.Value)
		}
	}
	return st, nil
}

// policyNames lists the inline policies of a user; a user that is gone has none.
func (s *Store) policyNames(ctx context.Context, user string) ([]string, error) {
	out, err := s.idp.ListUserPolicies(ctx, &awsiam.ListUserPoliciesInput{UserName: aws.String(user)})
	if codeOf(err) == codeNoSuchEntity {
		return nil, nil
	}
	if err != nil {
		return nil, fail(ctx, "IAM ListUserPolicies", err)
	}
	return out.PolicyNames, nil
}

// ApplyAccess makes the user exist, tagged with the claim, with exactly the
// policy of p among the ones it owns, given what User found. The policy is put
// before the ones it replaces are deleted, so the user is never without access
// during a change. Only inline policies s3client.PolicyName gave to the user
// are removed. The path and the permissions boundary apply to a new user only.
func (s *Store) ApplyAccess(ctx context.Context, p s3client.AccessParams, cur s3client.UserState) error {
	if !cur.Exists {
		in := &awsiam.CreateUserInput{
			UserName: aws.String(p.User),
			Tags:     []iamtypes.Tag{{Key: aws.String(s3client.MarkerTag), Value: aws.String(p.Marker)}},
		}
		if s.iamPath != "" {
			in.Path = aws.String(s.iamPath)
		}
		if s.boundary != "" {
			in.PermissionsBoundary = aws.String(s.boundary)
		}
		if _, err := s.idp.CreateUser(ctx, in); err != nil {
			return iamFail(ctx, "IAM CreateUser", err)
		}
	}
	if !slices.Contains(cur.Policies, p.Policy) {
		_, err := s.idp.PutUserPolicy(ctx, &awsiam.PutUserPolicyInput{
			UserName: aws.String(p.User), PolicyName: aws.String(p.Policy), PolicyDocument: aws.String(string(p.PolicyDoc)),
		})
		if err != nil {
			return iamFail(ctx, "IAM PutUserPolicy", err)
		}
	}
	for _, old := range cur.Policies {
		if old != p.Policy && s3client.OwnsPolicy(p.User, old) {
			if err := s.dropPolicy(ctx, p.User, old); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropPolicy deletes an inline policy; one that is gone is success.
func (s *Store) dropPolicy(ctx context.Context, user, policy string) error {
	_, err := s.idp.DeleteUserPolicy(ctx, &awsiam.DeleteUserPolicyInput{UserName: aws.String(user), PolicyName: aws.String(policy)})
	if err != nil && codeOf(err) != codeNoSuchEntity {
		return fail(ctx, "IAM DeleteUserPolicy", err)
	}
	return nil
}

// OwnedPolicies returns the inline policies s3client.PolicyName gave to user.
// They die with the user, so a user that is gone has none.
func (s *Store) OwnedPolicies(ctx context.Context, user string) ([]string, error) {
	names, err := s.policyNames(ctx, user)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(names, func(n string) bool { return !s3client.OwnsPolicy(user, n) }), nil
}

// keyIDs lists the access key ids of a user; the error is the SDK's, so a
// caller can tell a user that is gone.
func (s *Store) keyIDs(ctx context.Context, user string) ([]string, error) {
	out, err := s.idp.ListAccessKeys(ctx, &awsiam.ListAccessKeysInput{UserName: aws.String(user)})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(out.AccessKeyMetadata))
	for i, k := range out.AccessKeyMetadata {
		ids[i] = aws.ToString(k.AccessKeyId)
	}
	return ids, nil
}

// Keys lists the access key ids of the user, active or not. IAM allows two.
func (s *Store) Keys(ctx context.Context, user string) ([]string, error) {
	ids, err := s.keyIDs(ctx, user)
	if err != nil {
		return nil, iamFail(ctx, "IAM ListAccessKeys", err)
	}
	return ids, nil
}

// NewKey creates an access key for the user. IAM shows the secret key this once.
func (s *Store) NewKey(ctx context.Context, user string) (id, secret string, err error) {
	out, err := s.idp.CreateAccessKey(ctx, &awsiam.CreateAccessKeyInput{UserName: aws.String(user)})
	if err != nil {
		return "", "", iamFail(ctx, "IAM CreateAccessKey", err)
	}
	return aws.ToString(out.AccessKey.AccessKeyId), aws.ToString(out.AccessKey.SecretAccessKey), nil
}

// RemoveKey deletes an access key of the user; one that is gone is success.
func (s *Store) RemoveKey(ctx context.Context, user, id string) error {
	_, err := s.idp.DeleteAccessKey(ctx, &awsiam.DeleteAccessKeyInput{UserName: aws.String(user), AccessKeyId: aws.String(id)})
	if err != nil && codeOf(err) != codeNoSuchEntity {
		return fail(ctx, "IAM DeleteAccessKey", err)
	}
	return nil
}

// DeleteAccess deletes the user: its access keys first, then its inline
// policies, then the user itself, which IAM refuses to delete while either is
// left. IAM generated the keys, so the ids the caller derives from the claim
// mean nothing here; every key of the user goes, and the caller has checked the
// user is its own (the claim's tag). Every step is safe to repeat, and a user
// that is gone is success.
func (s *Store) DeleteAccess(ctx context.Context, user string, _ []string) error {
	ids, err := s.keyIDs(ctx, user)
	if codeOf(err) == codeNoSuchEntity {
		return nil
	}
	if err != nil {
		return fail(ctx, "IAM ListAccessKeys", err)
	}
	for _, id := range ids {
		if err := s.RemoveKey(ctx, user, id); err != nil {
			return err
		}
	}
	names, err := s.policyNames(ctx, user)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := s.dropPolicy(ctx, user, name); err != nil {
			return err
		}
	}
	if _, err := s.idp.DeleteUser(ctx, &awsiam.DeleteUserInput{UserName: aws.String(user)}); err != nil && codeOf(err) != codeNoSuchEntity {
		return fail(ctx, "IAM DeleteUser", err)
	}
	return nil
}
