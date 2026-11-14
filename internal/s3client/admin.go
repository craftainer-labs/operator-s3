package s3client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// adminPrefix is the RustFS admin API. Its payloads are plain JSON; only the
// /minio/admin compatibility paths are encrypted, and the operator never
// uses them.
const adminPrefix = "/rustfs/admin/v3"

// UserState is what the admin API reports about an IAM user.
type UserState struct {
	Exists, Enabled bool
	// Policies are the policies attached to the user.
	Policies []string
}

func (c *Client) admin(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	rq := call{method: method, path: adminPrefix + path, query: query}
	if body != nil {
		rq.body, _ = json.Marshal(body) // maps and structs of strings always marshal
		rq.contentType = "application/json"
	}
	return c.do(ctx, rq)
}

// adminOp is the path as the log names it: the one path that carries a
// bucket name, a quota path, loses it. do applies it to every request.
func adminOp(path string) string {
	if strings.HasPrefix(path, "/quota/") {
		return "/quota"
	}
	return path
}

func decodeJSON(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return ErrBadResponse
	}
	return nil
}

// ignoreNotFound turns a 404 into success.
func ignoreNotFound(err error) error {
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// User reads the IAM user name.
func (c *Client) User(ctx context.Context, name string) (UserState, error) {
	data, err := c.admin(ctx, http.MethodGet, "/user-info", url.Values{"accessKey": {name}}, nil)
	if errors.Is(err, ErrNotFound) {
		return UserState{}, nil
	}
	if err != nil {
		return UserState{}, err
	}
	var info struct {
		Status     string `json:"status"`
		PolicyName string `json:"policyName"`
	}
	if err := decodeJSON(data, &info); err != nil {
		return UserState{}, err
	}
	policies := strings.FieldsFunc(info.PolicyName, func(r rune) bool { return r == ',' })
	return UserState{Exists: true, Enabled: info.Status == "enabled", Policies: policies}, nil
}

// AccessParams is the IAM side of a BucketAccess.
type AccessParams struct {
	// User is the IAM user; Secret is its secret key when it has to be
	// created. Nobody holds that key: the access keys of the binding are
	// service accounts of the user.
	User, Secret string
	// Policy is the name of the policy document PolicyDoc.
	Policy    string
	PolicyDoc []byte
}

// run is admin without the answer.
func (c *Client) run(ctx context.Context, method, path string, query url.Values, body any) error {
	_, err := c.admin(ctx, method, path, query, body)
	return err
}

// ApplyAccess makes the user exist, enabled, with exactly the policy
// attached, given what User found. The policy is added before it is attached
// and the ones it replaces are detached and removed last, so the user is
// never without access during a change. Only policies PolicyName gave to the
// user are removed.
func (c *Client) ApplyAccess(ctx context.Context, p AccessParams, cur UserState) error {
	user := url.Values{"accessKey": {p.User}}
	var steps []func() error
	attached := slices.Contains(cur.Policies, p.Policy)
	if !attached {
		steps = append(steps, func() error {
			return c.run(ctx, http.MethodPut, "/add-canned-policy", url.Values{"name": {p.Policy}}, json.RawMessage(p.PolicyDoc))
		})
	}
	switch {
	case !cur.Exists:
		steps = append(steps, func() error {
			return c.run(ctx, http.MethodPut, "/add-user", user, map[string]string{"secretKey": p.Secret, "status": "enabled"})
		})
	case !cur.Enabled:
		steps = append(steps, func() error {
			return c.run(ctx, http.MethodPut, "/set-user-status", url.Values{"accessKey": {p.User}, "status": {"enabled"}}, nil)
		})
	}
	if !attached {
		steps = append(steps, func() error { return c.bindPolicy(ctx, "attach", p.User, p.Policy) })
	}
	for _, old := range cur.Policies {
		if old == p.Policy {
			continue
		}
		steps = append(steps, func() error { return c.bindPolicy(ctx, "detach", p.User, old) })
		if OwnsPolicy(p.User, old) {
			steps = append(steps, func() error { return c.removePolicy(ctx, old) })
		}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) bindPolicy(ctx context.Context, verb, user, policy string) error {
	return c.run(ctx, http.MethodPost, "/idp/builtin/policy/"+verb, nil,
		map[string]any{"policies": []string{policy}, "user": user})
}

func (c *Client) removePolicy(ctx context.Context, policy string) error {
	return ignoreNotFound(c.run(ctx, http.MethodDelete, "/remove-canned-policy", url.Values{"name": {policy}}, nil))
}

// KeyExists reports whether the service account with that access key exists.
func (c *Client) KeyExists(ctx context.Context, key string) (bool, error) {
	err := c.run(ctx, http.MethodGet, "/info-service-account", url.Values{"accessKey": {key}}, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// IssueKey makes the service account key of user exist with that secret key.
// It is idempotent: an existing account gets the secret key set again, so a
// retry after a crash neither fails nor leaks a second account. The account
// inherits the policy of the user.
func (c *Client) IssueKey(ctx context.Context, user, key, secret string) error {
	exists, err := c.KeyExists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return c.run(ctx, http.MethodPost, "/update-service-account", url.Values{"accessKey": {key}},
			map[string]string{"newSecretKey": secret})
	}
	return c.run(ctx, http.MethodPut, "/add-service-accounts", nil,
		map[string]string{"targetUser": user, "accessKey": key, "secretKey": secret})
}

// OwnedPolicies returns the policies of the store that PolicyName gave to
// user, whether or not they are still attached to it: the user may be gone
// while its policy is not.
func (c *Client) OwnedPolicies(ctx context.Context, user string) ([]string, error) {
	data, err := c.admin(ctx, http.MethodGet, "/list-canned-policies", nil, nil)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := decodeJSON(data, &all); err != nil {
		return nil, err
	}
	var owned []string
	for name := range all {
		if OwnsPolicy(user, name) {
			owned = append(owned, name)
		}
	}
	slices.Sort(owned)
	return owned, nil
}

// DeleteKey deletes a service account; one that is gone is success.
func (c *Client) DeleteKey(ctx context.Context, key string) error {
	exists, err := c.KeyExists(ctx, key)
	if err != nil || !exists {
		return err
	}
	return ignoreNotFound(c.run(ctx, http.MethodDelete, "/delete-service-account", url.Values{"accessKey": {key}}, nil))
}

// DeleteAccess deletes the given service accounts (the caller derives them
// from the claim, so no other account of the user is touched) and the user,
// then every policy PolicyName gave to the user, found in the store so that a
// policy outlives neither the user nor an earlier failed attempt. Every step
// is safe to repeat.
func (c *Client) DeleteAccess(ctx context.Context, user string, keys []string) error {
	for _, key := range keys {
		if err := c.DeleteKey(ctx, key); err != nil {
			return err
		}
	}
	// RustFS answers remove-user for a user that does not exist with HTTP 500
	// ("does not exist" in the message), not 404, so the user is looked up
	// first rather than the answer's text relied on.
	st, err := c.User(ctx, user)
	if err != nil {
		return err
	}
	if st.Exists {
		if err := ignoreNotFound(c.run(ctx, http.MethodDelete, "/remove-user", url.Values{"accessKey": {user}}, nil)); err != nil {
			return err
		}
	}
	policies, err := c.OwnedPolicies(ctx, user)
	if err != nil {
		return err
	}
	for _, policy := range policies {
		if err := c.removePolicy(ctx, policy); err != nil {
			return err
		}
	}
	return nil
}

// quota reads the hard quota of a bucket in bytes; 0 is none. RustFS answers
// 200 with "quota":0 for a bucket without a quota (the get-bucket-quota
// handler, admin/handlers/quota.rs); a 404 is NoSuchBucket, so it is an error
// here, not "no quota".
func (c *Client) quota(ctx context.Context, bucket string) (int64, error) {
	data, err := c.admin(ctx, http.MethodGet, "/get-bucket-quota", url.Values{"bucket": {bucket}}, nil)
	if err != nil {
		return 0, err
	}
	var q struct {
		Quota int64 `json:"quota"`
	}
	if err := decodeJSON(data, &q); err != nil {
		return 0, err
	}
	return q.Quota, nil
}

// setQuota sets the hard quota of a bucket, or clears it for 0.
func (c *Client) setQuota(ctx context.Context, bucket string, bytes int64) error {
	if bytes == 0 {
		return ignoreNotFound(c.run(ctx, http.MethodDelete, "/quota/"+bucket, nil, nil))
	}
	return c.run(ctx, http.MethodPut, "/set-bucket-quota", url.Values{"bucket": {bucket}},
		map[string]any{"quota": bytes, "quotatype": "HARD"})
}
