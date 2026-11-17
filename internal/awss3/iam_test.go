package awss3

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

const (
	iamUser = "team-a-orders-0123456789"
	iamArn  = "arn:aws:iam::123456789012:user/craftainer/" + iamUser
)

// accessParams is the access of the user to bucket b, read-only or not.
func accessParams(bucket string, write bool) s3client.AccessParams {
	doc := s3client.AccessPolicy(bucket, "", write)
	return s3client.AccessParams{User: iamUser, Policy: s3client.PolicyName(iamUser, doc), PolicyDoc: doc, Marker: marker}
}

func isPending(err error) bool {
	var p *lifecycle.PendingError
	return errors.As(err, &p) && p.After > 0 && !strings.Contains(err.Error(), "secret detail")
}

// A user is made with its tag, path, boundary and policy, read back, and its
// policy replaced; the policy it replaces goes, and a policy the operator did
// not name stays.
func TestUserAndPolicy(t *testing.T) {
	f := newFakeIAM()
	s := New(f.serve(t), WithIAM("/craftainer/", "arn:aws:iam::123456789012:policy/boundary"))
	ctx := context.Background()

	if st, err := s.User(ctx, iamUser); err != nil || st.Exists {
		t.Fatalf("absent user: %+v, %v", st, err)
	}
	read := accessParams("b", false)
	if err := s.ApplyAccess(ctx, read, s3client.UserState{}); err != nil {
		t.Fatal(err)
	}
	u := f.user(iamUser)
	if u.path != "/craftainer/" || u.boundary != "arn:aws:iam::123456789012:policy/boundary" || u.tags[s3client.MarkerTag] != marker || len(u.policies) != 1 {
		t.Fatalf("user = %+v", u)
	}
	u.tags["team"] = "x"
	u.policies["by-hand"] = "{}"

	st, err := s.User(ctx, iamUser)
	if err != nil || !st.Exists || !st.Enabled || st.ARN != iamArn || st.Marker != marker || !slices.Equal(st.Policies, []string{"by-hand", read.Policy}) {
		t.Fatalf("user state = %+v, %v", st, err)
	}

	// The policy changes: the new one is put, the old one (the operator's) is
	// deleted, and the one set by hand is kept.
	write := accessParams("b", true)
	if err := s.ApplyAccess(ctx, write, st); err != nil {
		t.Fatal(err)
	}
	if got := f.user(iamUser).policies; len(got) != 2 || got[write.Policy] == "" || got["by-hand"] == "" {
		t.Errorf("policies = %v", got)
	}
	// Nothing changed: no write.
	st, _ = s.User(ctx, iamUser)
	before := f.callList()
	if err := s.ApplyAccess(ctx, write, st); err != nil || strings.Count(f.callList(), "Put") != strings.Count(before, "Put") {
		t.Errorf("a second apply wrote: %v\n%s", err, f.callList())
	}

	owned, err := s.OwnedPolicies(ctx, iamUser)
	if err != nil || !slices.Equal(owned, []string{write.Policy}) {
		t.Errorf("owned = %v, %v", owned, err)
	}
}

// Without the options of the store, the user takes IAM's defaults.
func TestUserWithoutPathAndBoundary(t *testing.T) {
	f := newFakeIAM()
	s := New(f.serve(t))
	if err := s.ApplyAccess(context.Background(), accessParams("b", false), s3client.UserState{}); err != nil {
		t.Fatal(err)
	}
	if u := f.user(iamUser); u.path != "/" || u.boundary != "" {
		t.Errorf("user = %+v", u)
	}
}

// IAM that has not settled is a PendingError, not a failure: the user is made
// and not yet visible, or the policy call does not see the user yet.
func TestApplyAccessPendingWhileIAMSettles(t *testing.T) {
	for name, tc := range map[string]struct{ action, code string }{
		"the user exists already":     {"CreateUser", "EntityAlreadyExists"},
		"the user is not visible yet": {"PutUserPolicy", "NoSuchEntity"},
		"a change is in flight":       {"PutUserPolicy", "ConcurrentModification"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeIAM()
			f.refuse[tc.action] = refusal{http.StatusConflict, tc.code}
			err := New(f.serve(t)).ApplyAccess(context.Background(), accessParams("b", false), s3client.UserState{})
			if !isPending(err) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// Other failures are the categories of the S3 client, with no text of the answer.
func TestIAMFailures(t *testing.T) {
	ctx := context.Background()
	check := func(name string, status int, code string, call func(*Store) error) {
		t.Helper()
		f := newFakeIAM()
		f.put(iamUser, &fakeUser{path: "/", tags: map[string]string{}, policies: map[string]string{"old-01234567": "{}", "mine": "{}"}, keys: []fakeKey{{"K1", "s"}}})
		s := New(f.serve(t))
		f.refuse[name] = refusal{status, code}
		err := call(s)
		var apiErr *s3client.Error
		if err == nil || strings.Contains(err.Error(), "secret detail") || !errors.As(err, &apiErr) || apiErr.Status != status {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The user has one of the operator's policies, and the access wants another.
	own := accessParams("b", false)
	st := s3client.UserState{Exists: true, Policies: []string{iamUser + "-bbbbbbbb"}}

	check("GetUser", http.StatusForbidden, "AccessDenied", func(s *Store) error { _, err := s.User(ctx, iamUser); return err })
	check("ListUserPolicies", http.StatusForbidden, "AccessDenied", func(s *Store) error { _, err := s.User(ctx, iamUser); return err })
	check("ListUserPolicies", http.StatusForbidden, "AccessDenied", func(s *Store) error { _, err := s.OwnedPolicies(ctx, iamUser); return err })
	check("CreateUser", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.ApplyAccess(ctx, own, s3client.UserState{}) })
	check("PutUserPolicy", http.StatusForbidden, "AccessDenied", func(s *Store) error {
		return s.ApplyAccess(ctx, own, s3client.UserState{Exists: true})
	})
	check("DeleteUserPolicy", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.ApplyAccess(ctx, own, st) })
	check("ListAccessKeys", http.StatusForbidden, "AccessDenied", func(s *Store) error { _, err := s.Keys(ctx, iamUser); return err })
	check("CreateAccessKey", http.StatusConflict, "LimitExceeded", func(s *Store) error { _, _, err := s.NewKey(ctx, iamUser); return err })
	check("DeleteAccessKey", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.RemoveKey(ctx, iamUser, "K1") })
	check("ListAccessKeys", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.DeleteAccess(ctx, iamUser, nil) })
	check("DeleteAccessKey", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.DeleteAccess(ctx, iamUser, nil) })
	check("ListUserPolicies", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.DeleteAccess(ctx, iamUser, nil) })
	check("DeleteUserPolicy", http.StatusForbidden, "AccessDenied", func(s *Store) error { return s.DeleteAccess(ctx, iamUser, nil) })
	check("DeleteUser", http.StatusConflict, "DeleteConflict", func(s *Store) error { return s.DeleteAccess(ctx, iamUser, nil) })
}

// A user that is gone has no policies to own, and no keys; keys that IAM cannot
// list yet are pending.
func TestMissingUser(t *testing.T) {
	f := newFakeIAM()
	s := New(f.serve(t))
	ctx := context.Background()
	if owned, err := s.OwnedPolicies(ctx, iamUser); err != nil || len(owned) != 0 {
		t.Errorf("owned = %v, %v", owned, err)
	}
	if _, err := s.Keys(ctx, iamUser); !isPending(err) {
		t.Errorf("keys: %v", err)
	}
	if _, _, err := s.NewKey(ctx, iamUser); !isPending(err) {
		t.Errorf("new key: %v", err)
	}
	if err := s.RemoveKey(ctx, iamUser, "K"); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := s.DeleteAccess(ctx, iamUser, nil); err != nil {
		t.Errorf("delete: %v", err)
	}
}

// IAM generates the keys: two at most, each shown with its secret once.
func TestKeys(t *testing.T) {
	f := newFakeIAM()
	s := New(f.serve(t))
	ctx := context.Background()
	if err := s.ApplyAccess(ctx, accessParams("b", false), s3client.UserState{}); err != nil {
		t.Fatal(err)
	}
	id1, secret1, err := s.NewKey(ctx, iamUser)
	if err != nil || id1 == "" || secret1 == "" {
		t.Fatalf("%q, %q, %v", id1, secret1, err)
	}
	id2, _, err := s.NewKey(ctx, iamUser)
	if err != nil || id2 == id1 {
		t.Fatalf("%q, %v", id2, err)
	}
	var apiErr *s3client.Error
	if _, _, err := s.NewKey(ctx, iamUser); !errors.As(err, &apiErr) || apiErr.Code != "LimitExceeded" {
		t.Errorf("a third key: %v", err)
	}
	if ids, err := s.Keys(ctx, iamUser); err != nil || !slices.Equal(ids, []string{id1, id2}) {
		t.Errorf("keys = %v, %v", ids, err)
	}
	if err := s.RemoveKey(ctx, iamUser, id1); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKey(ctx, iamUser, id1); err != nil {
		t.Errorf("removing a removed key: %v", err)
	}
	if ids, _ := s.Keys(ctx, iamUser); !slices.Equal(ids, []string{id2}) {
		t.Errorf("keys = %v", ids)
	}
}

// A user goes with its keys first, then its policies; it cannot go before them.
func TestDeleteAccessOrder(t *testing.T) {
	f := newFakeIAM()
	s := New(f.serve(t))
	ctx := context.Background()
	if err := s.ApplyAccess(ctx, accessParams("b", false), s3client.UserState{}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, _, err := s.NewKey(ctx, iamUser); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAccess(ctx, iamUser, []string{"ignored"}); err != nil {
		t.Fatal(err)
	}
	if f.user(iamUser) != nil {
		t.Error("the user is still there")
	}
	calls := f.callList()
	lastKey, firstPolicy, deleteUser := strings.LastIndex(calls, "DeleteAccessKey"), strings.Index(calls, "DeleteUserPolicy"), strings.Index(calls, "DeleteUser ")
	if lastKey < 0 || firstPolicy < lastKey || deleteUser < firstPolicy {
		t.Errorf("order of calls:\n%s", calls)
	}
	if err := s.DeleteAccess(ctx, iamUser, nil); err != nil {
		t.Errorf("a second delete: %v", err)
	}
}
