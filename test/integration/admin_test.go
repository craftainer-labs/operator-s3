//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/craftainer-labs/lib-operator-go/pkg/lifecycle"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// What the Client signs with minio-go's SigV4 signer (service s3, the
// region, the payload hash in X-Amz-Content-Sha256) is accepted by the admin
// API, with and without a body, and a wrong secret or an unknown access key is
// an authentication failure.
func TestSigV4IsAccepted(t *testing.T) {
	sv := load(t)

	if status, body := sv.raw(t, http.MethodGet, "/list-canned-policies", nil, nil); status != http.StatusOK {
		t.Fatalf("signed GET: HTTP %d: %s", status, body)
	}
	name := unique("it-sig")
	doc := s3client.AccessPolicy("it-sig-bucket", "", false)
	status, body := sv.raw(t, http.MethodPut, "/add-canned-policy", url.Values{"name": {name}}, doc)
	if status != http.StatusOK {
		t.Fatalf("signed PUT with a body: HTTP %d: %s", status, body)
	}
	t.Cleanup(func() { sv.raw(t, http.MethodDelete, "/remove-canned-policy", url.Values{"name": {name}}, nil) })

	c := sv.client(t)
	if _, err := c.OwnedPolicies(bg, "it-sig"); err != nil {
		t.Fatalf("the Client's signature: %v", err)
	}
	// An empty region is the default one.
	d := sv.client(t, func(tg *s3client.Target) { tg.Region = "" })
	if _, err := d.OwnedPolicies(bg, "it-sig"); err != nil {
		t.Fatalf("the default region: %v", err)
	}

	for what, mutate := range map[string]func(*s3client.Target){
		"wrong secret":   func(tg *s3client.Target) { tg.SecretKey += "x" },
		"unknown access": func(tg *s3client.Target) { tg.AccessKey = "nobody-" + unique("ak") },
	} {
		_, err := sv.client(t, mutate).OwnedPolicies(bg, "it-sig")
		if !errors.Is(err, lifecycle.ErrUnreachable) || !errors.Is(err, s3client.ErrConnAuth) {
			t.Errorf("%s: %v, want an authentication failure", what, err)
		}
	}
}

// list-canned-policies answers with the policy documents by name, the
// built-in policies among them, and OwnedPolicies finds the ones PolicyName
// gave to the user and no others.
func TestListCannedPolicies(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	user := unique("it-list")
	p := grant(t, c, user, "it-list-bucket", "p/", true)

	status, body := sv.raw(t, http.MethodGet, "/list-canned-policies", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("HTTP %d: %s", status, body)
	}
	var all map[string]struct {
		Version   string
		Statement []json.RawMessage
	}
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("not a map of policies: %v: %s", err, body)
	}
	got, ok := all[p.Policy]
	if !ok {
		t.Fatalf("policy %s is not listed", p.Policy)
	}
	if got.Version != "2012-10-17" || len(got.Statement) == 0 {
		t.Errorf("policy = %+v", got)
	}
	foreign := 0
	for name := range all {
		if !s3client.OwnsPolicy(user, name) {
			foreign++
		}
	}
	t.Logf("%d policies are listed, %d of them not ours", len(all), foreign)

	owned, err := c.OwnedPolicies(bg, user)
	if err != nil || !slices.Equal(owned, []string{p.Policy}) {
		t.Errorf("owned = %v, %v", owned, err)
	}
	// A user whose name only starts like ours owns nothing of ours.
	if owned, err := c.OwnedPolicies(bg, user+"-more"); err != nil || len(owned) != 0 {
		t.Errorf("owned by a longer name = %v, %v", owned, err)
	}
}

// user-info answers with the status and the attached policies comma-joined in
// policyName; a disabled user reports it.
func TestUserInfo(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	user := unique("it-info")
	p := grant(t, c, user, "it-info-bucket", "", false)

	info := func() (string, []string) {
		t.Helper()
		status, body := sv.raw(t, http.MethodGet, "/user-info", url.Values{"accessKey": {user}}, nil)
		if status != http.StatusOK {
			t.Fatalf("user-info: HTTP %d: %s", status, body)
		}
		var in struct {
			Status     string `json:"status"`
			PolicyName string `json:"policyName"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatalf("user-info: %v: %s", err, body)
		}
		names := strings.Split(in.PolicyName, ",")
		slices.Sort(names)
		return in.Status, names
	}
	if status, names := info(); status != "enabled" || !slices.Equal(names, []string{p.Policy}) {
		t.Errorf("user-info = %s, %v", status, names)
	}

	// A second policy, attached by hand, makes the list.
	hand := unique("it-hand")
	handDoc := s3client.AccessPolicy("it-info-bucket", "", true)
	if status, body := sv.raw(t, http.MethodPut, "/add-canned-policy", url.Values{"name": {hand}}, handDoc); status != http.StatusOK {
		t.Fatalf("add-canned-policy: HTTP %d: %s", status, body)
	}
	t.Cleanup(func() { sv.raw(t, http.MethodDelete, "/remove-canned-policy", url.Values{"name": {hand}}, nil) })
	attach := []byte(`{"policies":["` + hand + `"],"user":"` + user + `"}`)
	if status, body := sv.raw(t, http.MethodPost, "/idp/builtin/policy/attach", nil, attach); status != http.StatusOK {
		t.Fatalf("attach: HTTP %d: %s", status, body)
	}
	want := []string{p.Policy, hand}
	slices.Sort(want)
	if _, names := info(); !slices.Equal(names, want) {
		t.Errorf("policyName = %v, want %v", names, want)
	}
	st, err := c.User(bg, user)
	got := slices.Clone(st.Policies)
	slices.Sort(got)
	if err != nil || !st.Exists || !st.Enabled || !slices.Equal(got, want) {
		t.Errorf("User = %+v, %v", st, err)
	}

	// A disabled user is reported as such, and ApplyAccess enables it again and
	// detaches the policy it does not own without removing it.
	status, body := sv.raw(t, http.MethodPut, "/set-user-status", url.Values{"accessKey": {user}, "status": {"disabled"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("set-user-status: HTTP %d: %s", status, body)
	}
	if status, _ := info(); status != "disabled" {
		t.Errorf("status = %q", status)
	}
	st, err = c.User(bg, user)
	if err != nil || !st.Exists || st.Enabled {
		t.Errorf("User of a disabled user = %+v, %v", st, err)
	}
	if err := c.ApplyAccess(bg, p, st); err != nil {
		t.Fatalf("ApplyAccess: %v", err)
	}
	st, err = c.User(bg, user)
	if err != nil || !st.Enabled || !slices.Equal(st.Policies, []string{p.Policy}) {
		t.Errorf("User after ApplyAccess = %+v, %v", st, err)
	}
	if status, body := sv.raw(t, http.MethodGet, "/list-canned-policies", nil, nil); status != http.StatusOK || !strings.Contains(string(body), `"`+hand+`"`) {
		t.Errorf("the policy the operator does not own was removed: HTTP %d", status)
	}
}

// The answers for something that is not there, and for the requests that
// would leave a policy or key orphaned, as the client has to read them: a user
// or service account is a 404 on read, the others are 400, and removing what is
// gone is a success.
func TestAnswersForMissingThings(t *testing.T) {
	sv := load(t)
	missing := unique("it-missing")
	for _, tc := range []struct {
		name, method, path string
		query              url.Values
		body               string
		want               int
	}{
		{"user-info", http.MethodGet, "/user-info", url.Values{"accessKey": {missing}}, "", http.StatusNotFound},
		{"info-service-account", http.MethodGet, "/info-service-account", url.Values{"accessKey": {missing}}, "", http.StatusNotFound},
		{"get-bucket-quota of a missing bucket", http.MethodGet, "/get-bucket-quota", url.Values{"bucket": {missing}}, "", http.StatusNotFound},
		{"attach to a missing user", http.MethodPost, "/idp/builtin/policy/attach", nil,
			`{"policies":["readonly"],"user":"` + missing + `"}`, http.StatusBadRequest},
		{"add-service-accounts for a missing user", http.MethodPut, "/add-service-accounts", nil,
			`{"targetUser":"` + missing + `","accessKey":"` + strings.ToUpper(missing) + `","secretKey":"` + newSecret() + `"}`, http.StatusBadRequest},
		{"delete-service-account", http.MethodDelete, "/delete-service-account", url.Values{"accessKey": {missing}}, "", http.StatusBadRequest},
		{"remove-user", http.MethodDelete, "/remove-user", url.Values{"accessKey": {missing}}, "", http.StatusInternalServerError},
		{"remove-canned-policy", http.MethodDelete, "/remove-canned-policy", url.Values{"name": {missing}}, "", http.StatusOK},
	} {
		status, body := sv.raw(t, tc.method, tc.path, tc.query, []byte(tc.body))
		if status != tc.want {
			t.Errorf("%s: HTTP %d, want %d: %s", tc.name, status, tc.want, body)
		}
	}

	// The Client's reading of them.
	c := sv.client(t)
	if st, err := c.User(bg, missing); err != nil || st.Exists {
		t.Errorf("User of a missing user = %+v, %v", st, err)
	}
	if ok, err := c.KeyExists(bg, strings.ToUpper(missing)); err != nil || ok {
		t.Errorf("KeyExists of a missing key = %v, %v", ok, err)
	}
	if err := c.DeleteKey(bg, strings.ToUpper(missing)); err != nil {
		t.Errorf("DeleteKey of a missing key: %v", err)
	}
	if err := c.DeleteAccess(bg, missing, []string{strings.ToUpper(missing)}); err != nil {
		t.Errorf("DeleteAccess of a missing user: %v", err)
	}
}

// A policy that is still attached cannot be removed, which is why ApplyAccess
// detaches before it removes; a policy that does not exist cannot be attached.
func TestPolicyOrderMatters(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	user := unique("it-order")
	p := grant(t, c, user, "it-order-bucket", "", true)

	status, _ := sv.raw(t, http.MethodDelete, "/remove-canned-policy", url.Values{"name": {p.Policy}}, nil)
	if status < 400 {
		t.Errorf("removing an attached policy: HTTP %d, want a failure", status)
	}
	if st, err := c.User(bg, user); err != nil || !slices.Equal(st.Policies, []string{p.Policy}) {
		t.Errorf("User = %+v, %v", st, err)
	}
	ghost := []byte(`{"policies":["` + unique("it-ghost") + `"],"user":"` + user + `"}`)
	if status, _ := sv.raw(t, http.MethodPost, "/idp/builtin/policy/attach", nil, ghost); status < 400 {
		t.Errorf("attaching a policy that does not exist: HTTP %d, want a failure", status)
	}
}

// A change of the policy replaces it: the new document is added and attached
// first, the old one detached and removed last, and the user keeps its
// service accounts.
func TestApplyAccessReplacesThePolicy(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	bucket := unique("it-repl")
	sv.makeBucket(t, bucket, false)
	user := unique("it-repl-user")
	first := grant(t, c, user, bucket, "a/", false)

	key := s3client.AccessKey(unique("uid"), 0)
	if err := c.IssueKey(bg, user, key, newSecret()); err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	st, err := c.User(bg, user)
	if err != nil {
		t.Fatal(err)
	}
	second := access(user, bucket, "b/", true)
	if second.Policy == first.Policy {
		t.Fatal("two documents, one name")
	}
	if err := c.ApplyAccess(bg, second, st); err != nil {
		t.Fatalf("ApplyAccess: %v", err)
	}
	st, err = c.User(bg, user)
	if err != nil || !st.Exists || !st.Enabled || !slices.Equal(st.Policies, []string{second.Policy}) {
		t.Errorf("User = %+v, %v", st, err)
	}
	if owned, err := c.OwnedPolicies(bg, user); err != nil || !slices.Equal(owned, []string{second.Policy}) {
		t.Errorf("owned = %v, %v (the replaced policy must be gone)", owned, err)
	}
	if ok, err := c.KeyExists(bg, key); err != nil || !ok {
		t.Errorf("the service account did not survive the policy change: %v, %v", ok, err)
	}
	// Repeating it changes nothing.
	if err := c.ApplyAccess(bg, second, st); err != nil {
		t.Errorf("repeat: %v", err)
	}
}

// DeleteAccess removes the claim's user, the service accounts it names and the
// policies PolicyName gave to the user, also when the user is gone already;
// another user's policies, a policy attached by hand and another user's service
// accounts stay. The one thing it cannot spare: RustFS removes every service
// account of a user together with the user.
func TestDeleteAccessTakesOnlyItsOwn(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	bucket := "it-del-bucket"
	user := unique("it-del")
	other := user + "-x" // its policies start with the name of user
	pa := grant(t, c, user, bucket, "", true)
	pb := grant(t, c, other, bucket, "x/", false)
	uid := unique("uid")
	ka, kb, stray := s3client.AccessKey(uid, 0), s3client.AccessKey(uid, 1), s3client.AccessKey(uid, 2)
	for key, owner := range map[string]string{ka: user, kb: other, stray: user} {
		if err := c.IssueKey(bg, owner, key, newSecret()); err != nil {
			t.Fatalf("IssueKey %s: %v", key, err)
		}
	}
	hand := unique("it-hand")
	if status, body := sv.raw(t, http.MethodPut, "/add-canned-policy", url.Values{"name": {hand}}, access(user, bucket, "", false).PolicyDoc); status != http.StatusOK {
		t.Fatalf("add-canned-policy: HTTP %d: %s", status, body)
	}
	t.Cleanup(func() { sv.raw(t, http.MethodDelete, "/remove-canned-policy", url.Values{"name": {hand}}, nil) })
	attach := []byte(`{"policies":["` + hand + `"],"user":"` + user + `"}`)
	if status, body := sv.raw(t, http.MethodPost, "/idp/builtin/policy/attach", nil, attach); status != http.StatusOK {
		t.Fatalf("attach: HTTP %d: %s", status, body)
	}

	if err := c.DeleteAccess(bg, user, []string{ka}); err != nil {
		t.Fatalf("DeleteAccess: %v", err)
	}
	if st, err := c.User(bg, user); err != nil || st.Exists {
		t.Errorf("the user is still there: %+v, %v", st, err)
	}
	if ok, _ := c.KeyExists(bg, ka); ok {
		t.Error("the named service account is still there")
	}
	if owned, err := c.OwnedPolicies(bg, user); err != nil || len(owned) != 0 {
		t.Errorf("owned policies = %v, %v", owned, err)
	}
	// Removing the user took the service account the claim did not name along.
	if ok, _ := c.KeyExists(bg, stray); ok {
		t.Error("RustFS kept a service account of a removed user")
	}
	// What is not the claim's is still there.
	if st, err := c.User(bg, other); err != nil || !st.Exists || !slices.Equal(st.Policies, []string{pb.Policy}) {
		t.Errorf("the other user = %+v, %v", st, err)
	}
	if ok, err := c.KeyExists(bg, kb); err != nil || !ok {
		t.Errorf("the other user's service account: %v, %v", ok, err)
	}
	status, body := sv.raw(t, http.MethodGet, "/list-canned-policies", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list: HTTP %d", status)
	}
	for _, name := range []string{pb.Policy, hand} {
		if !strings.Contains(string(body), `"`+name+`"`) {
			t.Errorf("policy %s is gone", name)
		}
	}
	if strings.Contains(string(body), `"`+pa.Policy+`"`) {
		t.Errorf("policy %s is still there", pa.Policy)
	}

	// A user that is gone while its policy is not: the policy is found by its
	// name and removed.
	p2 := access(other, bucket, "y/", true)
	st, err := c.User(bg, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyAccess(bg, p2, st); err != nil {
		t.Fatal(err)
	}
	if status, body := sv.raw(t, http.MethodDelete, "/remove-user", url.Values{"accessKey": {other}}, nil); status != http.StatusOK {
		t.Fatalf("remove-user: HTTP %d: %s", status, body)
	}
	if owned, err := c.OwnedPolicies(bg, other); err != nil || !slices.Equal(owned, []string{p2.Policy}) {
		t.Errorf("owned policies of a removed user = %v, %v", owned, err)
	}
	if err := c.DeleteAccess(bg, other, nil); err != nil {
		t.Fatalf("DeleteAccess of a removed user: %v", err)
	}
	if owned, err := c.OwnedPolicies(bg, other); err != nil || len(owned) != 0 {
		t.Errorf("owned policies after = %v, %v", owned, err)
	}
}

// A service account is issued with the key and secret given, again with a new
// secret (a retry), two generations work at once (the rotation overlap), and a
// deleted one stops working.
func TestServiceAccountKeys(t *testing.T) {
	sv := load(t)
	c := sv.client(t)
	bucket := unique("it-sa")
	sv.makeBucket(t, bucket, false)
	user := unique("it-sa-user")
	grant(t, c, user, bucket, "", true)
	uid := unique("uid")
	k0, k1 := s3client.AccessKey(uid, 0), s3client.AccessKey(uid, 1)

	// can tells whether the key authenticates and may use the bucket.
	can := func(key, secret string) error {
		_, err := sv.minio(t, key, secret).BucketExists(bg, bucket)
		return err
	}
	cannot := func(what, key, secret string) {
		t.Helper()
		eventually(t, 15*time.Second, what, func() error {
			if can(key, secret) == nil {
				return errors.New("still accepted")
			}
			return nil
		})
	}
	works := func(what, key, secret string) {
		t.Helper()
		eventually(t, 15*time.Second, what, func() error { return can(key, secret) })
	}

	if ok, err := c.KeyExists(bg, k0); err != nil || ok {
		t.Fatalf("KeyExists before = %v, %v", ok, err)
	}
	s0 := newSecret()
	if err := c.IssueKey(bg, user, k0, s0); err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if ok, err := c.KeyExists(bg, k0); err != nil || !ok {
		t.Fatalf("KeyExists after = %v, %v", ok, err)
	}
	works("the new key", k0, s0)

	// The retry after a crash: same key, a new secret, no second account.
	s0b := newSecret()
	if err := c.IssueKey(bg, user, k0, s0b); err != nil {
		t.Fatalf("IssueKey again: %v", err)
	}
	works("the key with its new secret", k0, s0b)
	cannot("the old secret", k0, s0)

	// The overlap, then the end of the grace period.
	s1 := newSecret()
	if err := c.IssueKey(bg, user, k1, s1); err != nil {
		t.Fatalf("IssueKey of the next generation: %v", err)
	}
	works("the next generation", k1, s1)
	works("the previous generation", k0, s0b)
	if err := c.DeleteKey(bg, k0); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if ok, err := c.KeyExists(bg, k0); err != nil || ok {
		t.Errorf("KeyExists after delete = %v, %v", ok, err)
	}
	cannot("the deleted key", k0, s0b)
	works("the remaining generation", k1, s1)
	if err := c.DeleteKey(bg, k0); err != nil {
		t.Errorf("DeleteKey again: %v", err)
	}

	// A key gone from the store is issued again.
	if err := c.DeleteKey(bg, k1); err != nil {
		t.Fatal(err)
	}
	s1b := newSecret()
	if err := c.IssueKey(bg, user, k1, s1b); err != nil {
		t.Fatalf("IssueKey after a delete: %v", err)
	}
	works("the reissued key", k1, s1b)
}

// The limits of names and keys the operator relies on: a user name of the
// longest the operator derives, service account keys of 3 to 128 characters,
// secret keys of 8 to 40.
func TestNameAndKeyLimits(t *testing.T) {
	sv := load(t)
	c := sv.client(t)

	long := unique("it-long") + "-"
	long += strings.Repeat("u", 54-len(long))
	if len(long) != 54 {
		t.Fatalf("test bug: %d characters", len(long))
	}
	grant(t, c, long, "it-long-bucket", "", false)
	if st, err := c.User(bg, long); err != nil || !st.Exists {
		t.Errorf("a 54-character user = %+v, %v", st, err)
	}
	// Policy names are the user's name, a dash and 8 hex characters.
	if owned, err := c.OwnedPolicies(bg, long); err != nil || len(owned) != 1 || len(owned[0]) != 63 {
		t.Errorf("owned = %v, %v", owned, err)
	}
	body := []byte(`{"secretKey":"` + newSecret() + `","status":"enabled"}`)
	if status, _ := sv.raw(t, http.MethodPut, "/add-user", url.Values{"accessKey": {"ab"}}, body); status != http.StatusInternalServerError {
		// RustFS rejects it with 500, not 400.
		t.Errorf("a 2-character user: HTTP %d, want 500", status)
	}

	for _, tc := range []struct {
		name, key, secret string
		ok                bool
	}{
		{"key of 3", "KEY", newSecret(), true},
		{"key of 128", strings.Repeat("K", 128), newSecret(), true},
		{"key of 129", strings.Repeat("K", 129), newSecret(), false},
		{"key of 2", "KE", newSecret(), false},
		{"secret of 8", "SECRETKEYEIGHT", strings.Repeat("s", 8), true},
		{"secret of 40", "SECRETKEYFORTY", strings.Repeat("s", 40), true},
		{"secret of 41", "SECRETKEYTOOLONG", strings.Repeat("s", 41), false},
		{"secret of 7", "SECRETKEYSEVEN", strings.Repeat("s", 7), false},
	} {
		err := c.IssueKey(bg, long, tc.key, tc.secret)
		var apiErr *s3client.Error
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case !tc.ok && (!errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest):
			t.Errorf("%s: %v, want HTTP 400", tc.name, err)
		}
	}
}
