package s3client

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"
)

var bg = context.Background()

func TestUserStates(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()

	st, err := c.User(bg, "nobody")
	if err != nil || st.Exists {
		t.Errorf("missing user: %+v, %v", st, err)
	}
	f.user["alice"] = &fakeUser{status: "enabled", policies: []string{"p1", "p2"}}
	f.user["bob"] = &fakeUser{status: "disabled"}
	st, err = c.User(bg, "alice")
	if err != nil || !st.Exists || !st.Enabled || !slices.Equal(st.Policies, []string{"p1", "p2"}) {
		t.Errorf("alice: %+v, %v", st, err)
	}
	st, _ = c.User(bg, "bob")
	if !st.Exists || st.Enabled || len(st.Policies) != 0 {
		t.Errorf("bob: %+v", st)
	}

	f.setBody("GET /rustfs/admin/v3/user-info", "not json")
	if _, err := c.User(bg, "alice"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("bad body: %v", err)
	}
	f.body = map[string]string{}
	f.failOn = "user-info"
	if _, err := c.User(bg, "alice"); err == nil {
		t.Error("a failing store was not an error")
	}
}

func accessParams(user string, write bool) AccessParams {
	doc := AccessPolicy("bkt", "app/", write)
	return AccessParams{User: user, Secret: "user-secret", Policy: PolicyName(user, doc), PolicyDoc: doc}
}

func TestApplyAccessCreatesAndConverges(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	p := accessParams("tenant-user", true)

	// Nothing exists: policy, user, attachment, in that order.
	if err := c.ApplyAccess(bg, p, UserState{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /rustfs/admin/v3/add-canned-policy?name=" + p.Policy,
		"PUT /rustfs/admin/v3/add-user?accessKey=tenant-user",
		"POST /rustfs/admin/v3/idp/builtin/policy/attach",
	}
	if got := f.requests(); !slices.Equal(got, want) {
		t.Errorf("requests:\n%v\nwant:\n%v", got, want)
	}
	if f.policy[p.Policy] != string(p.PolicyDoc) || f.user["tenant-user"].status != "enabled" ||
		!slices.Equal(f.user["tenant-user"].policies, []string{p.Policy}) {
		t.Errorf("state: %+v, %v", f.user["tenant-user"], f.policy)
	}

	// Converged: nothing to do.
	cur, err := c.User(bg, "tenant-user")
	if err != nil {
		t.Fatal(err)
	}
	f.reset()
	if err := c.ApplyAccess(bg, p, cur); err != nil {
		t.Fatal(err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("a converged user was written: %v", f.requests())
	}

	// A disabled user is enabled again.
	f.user["tenant-user"].status = "disabled"
	cur, _ = c.User(bg, "tenant-user")
	if err := c.ApplyAccess(bg, p, cur); err != nil {
		t.Fatal(err)
	}
	if f.user["tenant-user"].status != "enabled" {
		t.Error("the user is still disabled")
	}

	// The spec changed: the new policy is attached before the one it replaces
	// is detached and removed; a policy somebody else attached is detached but
	// kept.
	f.policy["foreign"] = "{}"
	f.user["tenant-user"].policies = append(f.user["tenant-user"].policies, "foreign")
	next := accessParams("tenant-user", false)
	cur, _ = c.User(bg, "tenant-user")
	f.reset()
	if err := c.ApplyAccess(bg, next, cur); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) != 5 || !strings.Contains(reqs[0], "add-canned-policy?name="+next.Policy) ||
		!strings.Contains(reqs[1], "policy/attach") || !strings.Contains(reqs[2], "policy/detach") {
		t.Errorf("replace: %v", reqs)
	}
	if !slices.Equal(f.user["tenant-user"].policies, []string{next.Policy}) {
		t.Errorf("policies = %v", f.user["tenant-user"].policies)
	}
	if _, ok := f.policy[p.Policy]; ok {
		t.Error("the replaced policy was kept")
	}
	if _, ok := f.policy["foreign"]; !ok {
		t.Error("a policy the operator did not name was removed")
	}
}

func TestApplyAccessReportsEveryFailure(t *testing.T) {
	p := accessParams("tenant-user", true)
	old := accessParams("tenant-user", false)
	cases := []struct {
		failOn string
		user   *fakeUser
		cur    UserState
	}{
		{"add-canned-policy", nil, UserState{}},
		{"add-user", nil, UserState{}},
		{"policy/attach", &fakeUser{status: "enabled"}, UserState{Exists: true, Enabled: true}},
		{"set-user-status", &fakeUser{status: "disabled", policies: []string{p.Policy}}, UserState{Exists: true, Policies: []string{p.Policy}}},
		{"policy/detach", &fakeUser{status: "enabled", policies: []string{p.Policy, old.Policy}},
			UserState{Exists: true, Enabled: true, Policies: []string{p.Policy, old.Policy}}},
		{"remove-canned-policy", &fakeUser{status: "enabled", policies: []string{p.Policy, old.Policy}},
			UserState{Exists: true, Enabled: true, Policies: []string{p.Policy, old.Policy}}},
	}
	for _, tc := range cases {
		f := newFakeStore(t)
		if tc.user != nil {
			f.user["tenant-user"] = tc.user
		}
		f.policy[old.Policy] = "{}"
		f.failOn = tc.failOn
		if err := f.client().ApplyAccess(bg, p, tc.cur); err == nil {
			t.Errorf("%s: no error", tc.failOn)
		}
	}
}

func TestServiceAccountKeys(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()

	if ok, err := c.KeyExists(bg, "KEY1"); ok || err != nil {
		t.Errorf("missing key: %v, %v", ok, err)
	}
	// A new key is added for the user with the key and secret asked for; an
	// existing one gets its secret set again.
	if err := c.IssueKey(bg, "u1", "KEY1", "secret-1"); err != nil {
		t.Fatal(err)
	}
	if k := f.key["KEY1"]; k == nil || k.parent != "u1" || k.secret != "secret-1" {
		t.Errorf("key = %+v", k)
	}
	f.reset()
	if err := c.IssueKey(bg, "u1", "KEY1", "secret-2"); err != nil {
		t.Fatal(err)
	}
	if f.key["KEY1"].secret != "secret-2" || f.asked("add-service-accounts") || !f.asked("update-service-account?accessKey=KEY1") {
		t.Errorf("reissue: %v, %+v", f.requests(), f.key["KEY1"])
	}
	if err := c.IssueKey(bg, "u1", "KEY2", "secret-3"); err != nil {
		t.Fatal(err)
	}
	if len(f.key) != 2 || f.key["KEY1"] == nil || f.key["KEY2"] == nil {
		t.Errorf("keys = %v", f.key)
	}

	// Deleting a key that is gone succeeds.
	if err := c.DeleteKey(bg, "KEY1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteKey(bg, "KEY1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.key["KEY1"]; ok {
		t.Error("KEY1 is still there")
	}

	for _, failOn := range []string{"info-service-account", "update-service-account", "add-service-accounts", "delete-service-account"} {
		f.failOn = failOn
		var err error
		switch failOn {
		case "info-service-account":
			_, err = c.KeyExists(bg, "KEY2")
		case "update-service-account":
			err = c.IssueKey(bg, "u1", "KEY2", "s")
		case "add-service-accounts":
			err = c.IssueKey(bg, "u1", "KEY9", "s")
		default:
			err = c.DeleteKey(bg, "KEY2")
		}
		if err == nil {
			t.Errorf("%s: no error", failOn)
		}
	}
	f.failOn = "info-service-account"
	if err := c.DeleteKey(bg, "KEY2"); err == nil {
		t.Error("delete: the existence check failed silently")
	}
	if err := c.IssueKey(bg, "u1", "KEY2", "s"); err == nil {
		t.Error("issue: the existence check failed silently")
	}
}

func TestDeleteAccess(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	p := accessParams("tenant-user", true)
	if err := c.ApplyAccess(bg, p, UserState{}); err != nil {
		t.Fatal(err)
	}
	f.policy["foreign"] = "{}"
	f.policy["tenant-user-extra"] = "{}"
	for _, k := range []string{"K1", "K2"} {
		if err := c.IssueKey(bg, "tenant-user", k, "s"); err != nil {
			t.Fatal(err)
		}
	}
	// A service account of the user that the operator did not derive stays.
	f.key["MANUAL"] = &fakeKey{parent: "tenant-user"}
	f.key["OTHER"] = &fakeKey{parent: "someone-else"}

	if err := c.DeleteAccess(bg, "tenant-user", []string{"K1", "K2", "K3"}); err != nil {
		t.Fatal(err)
	}
	if len(f.user) != 0 || len(f.key) != 2 || f.key["OTHER"] == nil || f.key["MANUAL"] == nil {
		t.Errorf("users = %v, keys = %v", f.user, f.key)
	}
	if _, ok := f.policy[p.Policy]; ok {
		t.Error("the policy of the user is still there")
	}
	for _, kept := range []string{"foreign", "tenant-user-extra"} {
		if _, ok := f.policy[kept]; !ok {
			t.Errorf("the policy %s, which the operator did not name, was removed", kept)
		}
	}
	// Again: everything is gone and that is success.
	if err := c.DeleteAccess(bg, "tenant-user", []string{"K1", "K2", "K3"}); err != nil {
		t.Errorf("repeat: %v", err)
	}

	// A policy outlives its user: it is found in the store, not through the
	// user.
	f2 := newFakeStore(t)
	c2 := f2.client()
	f2.policy[p.Policy] = "{}"
	if err := c2.DeleteAccess(bg, "tenant-user", nil); err != nil {
		t.Fatal(err)
	}
	if len(f2.policy) != 0 {
		t.Errorf("policies = %v", f2.policy)
	}

	for _, failOn := range []string{"info-service-account", "delete-service-account", "remove-user", "list-canned-policies", "remove-canned-policy"} {
		f3 := newFakeStore(t)
		c3 := f3.client()
		_ = c3.ApplyAccess(bg, p, UserState{})
		_ = c3.IssueKey(bg, "tenant-user", "K1", "s")
		f3.failOn = failOn
		if err := c3.DeleteAccess(bg, "tenant-user", []string{"K1"}); err == nil {
			t.Errorf("%s: no error", failOn)
		}
	}
}

func TestOwnedPolicies(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	f.policy["u-0123abcd"] = "{}"
	f.policy["u-4567abcd"] = `{"Version":"2012-10-17"}`
	f.policy["u-extra"] = "{}"
	f.policy["u-v-0123abcd"] = "not json"
	f.policy["readwrite"] = "{}"
	got, err := c.OwnedPolicies(bg, "u")
	if err != nil || !slices.Equal(got, []string{"u-0123abcd", "u-4567abcd"}) {
		t.Errorf("owned = %v, %v", got, err)
	}
	f.setBody("GET /rustfs/admin/v3/list-canned-policies", "[")
	if _, err := c.OwnedPolicies(bg, "u"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("bad list: %v", err)
	}
	f.failOn = "list-canned-policies"
	if _, err := c.OwnedPolicies(bg, "u"); err == nil {
		t.Error("a failed list is not an error")
	}
}

func TestQuota(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	if q, err := c.quota(bg, "b"); err != nil || q != 0 {
		t.Errorf("quota = %d, %v", q, err)
	}
	if err := c.setQuota(bg, "b", 1<<30); err != nil {
		t.Fatal(err)
	}
	if q, _ := c.quota(bg, "b"); q != 1<<30 || f.quotaType != "HARD" {
		t.Errorf("quota = %d, type %q", q, f.quotaType)
	}
	if err := c.setQuota(bg, "b", 0); err != nil {
		t.Fatal(err)
	}
	if q, _ := c.quota(bg, "b"); q != 0 {
		t.Errorf("quota after clear = %d", q)
	}
	// Clearing a quota that is not set is not an error.
	f.statusOn["DELETE /rustfs/admin/v3/quota/b"] = 404
	if err := c.setQuota(bg, "b", 0); err != nil {
		t.Errorf("clear of a missing quota: %v", err)
	}
	// No quota set may be a 404.
	f.statusOn["GET /rustfs/admin/v3/get-bucket-quota"] = 404
	if q, err := c.quota(bg, "b"); err != nil || q != 0 {
		t.Errorf("quota of a 404 = %d, %v", q, err)
	}
	delete(f.statusOn, "GET /rustfs/admin/v3/get-bucket-quota")
	f.setBody("GET /rustfs/admin/v3/get-bucket-quota", "not json")
	if _, err := c.quota(bg, "b"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("bad body: %v", err)
	}
	f.failOn = "quota"
	if _, err := c.quota(bg, "b"); err == nil {
		t.Error("get: no error")
	}
	if err := c.setQuota(bg, "b", 5); err == nil {
		t.Error("set: no error")
	}
}

func TestAnUnreachableStoreIsUnreachable(t *testing.T) {
	f := newFakeStore(t)
	c := f.client()
	f.statusOn["rustfs/admin"] = 403
	if _, err := c.User(bg, "u"); !errors.Is(err, lifecycle.ErrUnreachable) {
		t.Errorf("403: %v", err)
	}
}
