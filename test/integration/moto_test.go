//go:build integration && moto

package integration

// The AWS store of NKs3-1a and NKs3-1b (internal/awss3, with its IAM users,
// policies and access keys, and the AWS probe of internal/controller) against
// moto server, reached at its container address
// (a private range, never loopback: pkg/egress blocks loopback in both
// modes) with EGRESS_MODE=allow-private in this test's own wiring only.
//
// What moto cannot check, and these tests therefore do not: it does not
// enforce IAM, so a role's permissions, the eventual consistency of IAM
// changes and the 403 of a denied call are not exercised; and it does not
// validate a web identity token with STS, so the workload identity credentials
// of a real cluster (IRSA, Pod Identity) are not exercised either. They are
// checked by a real AWS account, which no CI lane has. For the IAM calls that
// means the eventual consistency of a new user or key, a permissions boundary
// that actually limits what a key can do, and the policy that really grants the
// bucket access; moto stores them and answers for them, nothing more.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsiam "github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	libserver "github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/awss3"
	"github.com/craftainer-labs/operator-s3/internal/controller"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

var motoCreds = map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test"), cloud.KeyAWSSecretAccessKey: []byte("test")}

// motoEndpoint is the moto server, from MOTO_ENDPOINT (http://<private ip>:5000).
func motoEndpoint(t *testing.T) string {
	t.Helper()
	v := os.Getenv("MOTO_ENDPOINT")
	if v == "" {
		t.Fatal("MOTO_ENDPOINT is not set; see README.md here")
	}
	return v
}

// motoConfig is the AWS configuration of an AWS store pointed at moto, built
// the way the operator builds it, through cloud.Egress.
func motoConfig(t *testing.T, region string) aws.Config {
	t.Helper()
	cfg, err := cloud.NewEgress(egress.ModeAllowPrivate, nil).AWS(context.Background(), cloud.ScopeCluster,
		cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: region}, EndpointOverride: motoEndpoint(t)},
		&cloud.Credentials{Source: cloud.SourceSecret}, motoCreds)
	if err != nil {
		t.Fatalf("AWS configuration: %v", err)
	}
	return cfg
}

func motoStore(t *testing.T, region string) *awss3.Store {
	t.Helper()
	s := awss3.New(motoConfig(t, region))
	t.Cleanup(s.CloseIdleConnections)
	return s
}

func TestMotoBucketLifecycle(t *testing.T) {
	for _, region := range []string{"us-east-1", "eu-west-1"} {
		t.Run(region, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s := motoStore(t, region)
			name := unique("moto")
			t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

			if st, err := s.Bucket(ctx, name); err != nil || st.Exists {
				t.Fatalf("absent bucket: %+v, %v", st, err)
			}

			p := s3client.BucketParams{
				Name: name, Marker: "ns/claim/uid", Versioning: true,
				Rules: []s3client.Rule{{ID: "tmp", Prefix: "tmp/", Days: 7}, {ID: "all", Prefix: "", Days: 365}},
			}
			if err := s.CreateBucket(ctx, p); err != nil {
				t.Fatalf("create: %v", err)
			}
			st, err := s.Bucket(ctx, name)
			if err != nil || !st.Exists || st.Marker() != p.Marker || !st.Versioning || st.Quota != 0 || !s3client.RulesEqual(st.Rules, p.Rules) {
				t.Fatalf("after create: %+v, %v", st, err)
			}
			if err := s.CreateBucket(ctx, p); !errors.Is(err, s3client.ErrBucketExists) {
				t.Errorf("second create: %v", err)
			}

			// Update: the rules change and versioning is suspended.
			p.Rules = []s3client.Rule{{ID: "tmp", Prefix: "tmp/", Days: 30}}
			p.Versioning = false
			if err := s.UpdateBucket(ctx, p, st); err != nil {
				t.Fatalf("update: %v", err)
			}
			st, err = s.Bucket(ctx, name)
			if err != nil || st.Versioning || !s3client.RulesEqual(st.Rules, p.Rules) {
				t.Errorf("after update: %+v, %v", st, err)
			}
			p.Rules = nil
			if err := s.UpdateBucket(ctx, p, st); err != nil {
				t.Fatalf("update without rules: %v", err)
			}
			if st, err = s.Bucket(ctx, name); err != nil || len(st.Rules) != 0 {
				t.Errorf("rules were not removed: %+v, %v", st, err)
			}

			if err := s.DeleteBucket(ctx, name); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if st, err = s.Bucket(ctx, name); err != nil || st.Exists {
				t.Errorf("after delete: %+v, %v", st, err)
			}
			if err := s.DeleteBucket(ctx, name); err != nil {
				t.Errorf("delete of a deleted bucket: %v", err)
			}
		})
	}
}

// The operator never deletes objects, so a bucket that holds one stays.
func TestMotoDeleteKeepsObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := motoConfig(t, "us-east-1")
	s := awss3.New(cfg)
	name := unique("moto-full")
	if err := s.CreateBucket(ctx, s3client.BucketParams{Name: name, Marker: "m"}); err != nil {
		t.Fatal(err)
	}
	api := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
	key := aws.String("object")
	if _, err := api.PutObject(ctx, &s3.PutObjectInput{Bucket: &name, Key: key, Body: strings.NewReader("data")}); err != nil {
		t.Fatalf("put object: %v", err)
	}
	t.Cleanup(func() {
		_, _ = api.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: &name, Key: key})
		_ = s.DeleteBucket(context.Background(), name)
	})
	if err := s.DeleteBucket(ctx, name); !errors.Is(err, s3client.ErrBucketNotEmpty) {
		t.Errorf("delete: %v", err)
	}
	if st, err := s.Bucket(ctx, name); err != nil || !st.Exists {
		t.Errorf("the bucket is gone: %+v, %v", st, err)
	}
}

// A quota is refused, for a new bucket and for an existing one, before any
// request: S3 has none to enforce, and a quota that is never reported back would
// update the bucket forever.
func TestMotoQuotaIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := motoStore(t, "us-east-1")
	name := unique("moto-quota")
	t.Cleanup(func() { _ = s.DeleteBucket(context.Background(), name) })

	p := s3client.BucketParams{Name: name, Marker: "m", Quota: 1 << 30}
	if err := s.CreateBucket(ctx, p); !errors.Is(err, awss3.ErrQuotaUnsupported) {
		t.Errorf("create: %v", err)
	}
	if st, err := s.Bucket(ctx, name); err != nil || st.Exists {
		t.Errorf("a bucket was made: %+v, %v", st, err)
	}
	p.Quota = 0
	if err := s.CreateBucket(ctx, p); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Bucket(ctx, name)
	p.Quota = 1 << 30
	if err := s.UpdateBucket(ctx, p, st); !errors.Is(err, awss3.ErrQuotaUnsupported) {
		t.Errorf("update: %v", err)
	}
}

// motoStoreObject is a ClusterObjectStore on AWS pointed at moto.
func motoStoreObject(t *testing.T) *s3v1alpha1.ClusterObjectStore {
	t.Helper()
	s := &s3v1alpha1.ClusterObjectStore{}
	s.Spec.Provider = &s3v1alpha1.ClusterStoreProvider{
		Type: cloud.ProviderAWS,
		AWS:  &s3v1alpha1.ClusterAWSBlock{AWSClusterBlock: cloud.AWSClusterBlock{AWSBlock: cloud.AWSBlock{Region: "us-west-2"}, EndpointOverride: motoEndpoint(t)}},
	}
	s.Spec.Credentials = &cloud.Credentials{Source: cloud.SourceSecret}
	return s
}

func motoProber(mode egress.Mode) controller.Prober {
	return controller.Prober{Config: controller.Config{Egress: egress.NewPolicy(mode), EgressMode: mode, ProbeAWS: awss3.Probe}}
}

// The probe asks STS and S3, and discovers the region, the endpoint and the
// account.
func TestMotoProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := motoProber(egress.ModeAllowPrivate).ProbeCloud(ctx, motoStoreObject(t), libserver.CloudConnection{Credentials: motoCreds})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	want := map[string]string{
		controller.DiscoveredRegion: "us-west-2", controller.DiscoveredEndpoint: motoEndpoint(t),
		controller.DiscoveredAccountID: "123456789012", // moto's default account
	}
	if res.Version != "" || len(res.Discovered) != len(want) {
		t.Fatalf("result = %+v", res)
	}
	for k, v := range want {
		if res.Discovered[k] != v {
			t.Errorf("discovered[%s] = %q, want %q", k, res.Discovered[k], v)
		}
	}
}

// An override is refused in strict mode, and the credentials of a Secret that
// lacks a key are refused, each before a request is made.
func TestMotoProbeRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := motoProber(egress.ModeStrict).ProbeCloud(ctx, motoStoreObject(t), libserver.CloudConnection{Credentials: motoCreds})
	if !errors.Is(err, cloud.ErrEndpointOverrideForbidden) {
		t.Errorf("strict: %v", err)
	}
	_, err = motoProber(egress.ModeAllowPrivate).ProbeCloud(ctx, motoStoreObject(t),
		libserver.CloudConnection{Credentials: map[string][]byte{cloud.KeyAWSAccessKeyID: []byte("test")}})
	if !errors.Is(err, cloud.ErrCredentialsUnavailable) {
		t.Errorf("credentials: %v", err)
	}
}

// motoAccess is the IAM side of a BucketAccess for a fresh user.
func motoAccess(user, bucket string, write bool) s3client.AccessParams {
	doc := s3client.AccessPolicy(bucket, "app/", write)
	return s3client.AccessParams{User: user, Policy: s3client.PolicyName(user, doc), PolicyDoc: doc, Marker: "ns/claim/uid"}
}

// A BucketAccess's IAM user: made with its tag, path and policy, its policy
// replaced by a change of access, two keys through a rotation, and the user
// deleted with its keys and policy; deleting again succeeds.
func TestMotoAccessFlow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s := awss3.New(motoConfig(t, "us-east-1"), awss3.WithIAM("/craftainer/", ""))
	t.Cleanup(s.CloseIdleConnections)
	user := unique("moto-user")
	t.Cleanup(func() { _ = s.DeleteAccess(context.Background(), user, nil) })

	if st, err := s.User(ctx, user); err != nil || st.Exists {
		t.Fatalf("absent user: %+v, %v", st, err)
	}
	read := motoAccess(user, "bkt", false)
	if err := s.ApplyAccess(ctx, read, s3client.UserState{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	st, err := s.User(ctx, user)
	if err != nil || !st.Exists || st.Marker != read.Marker || !strings.HasSuffix(st.ARN, ":user/craftainer/"+user) || len(st.Policies) != 1 || st.Policies[0] != read.Policy {
		t.Fatalf("user = %+v, %v", st, err)
	}

	// The access changes: the new policy replaces the old one.
	write := motoAccess(user, "bkt", true)
	if err := s.ApplyAccess(ctx, write, st); err != nil {
		t.Fatalf("update: %v", err)
	}
	if owned, err := s.OwnedPolicies(ctx, user); err != nil || len(owned) != 1 || owned[0] != write.Policy {
		t.Fatalf("owned = %v, %v", owned, err)
	}

	// A rotation: the second key, then the first one is deleted.
	first, secret, err := s.NewKey(ctx, user)
	if err != nil || first == "" || secret == "" {
		t.Fatalf("first key: %q, %v", first, err)
	}
	second, _, err := s.NewKey(ctx, user)
	if err != nil || second == first {
		t.Fatalf("second key: %q, %v", second, err)
	}
	if ids, err := s.Keys(ctx, user); err != nil || len(ids) != 2 {
		t.Fatalf("keys = %v, %v", ids, err)
	}
	if err := s.RemoveKey(ctx, user, first); err != nil {
		t.Fatalf("remove key: %v", err)
	}
	if ids, err := s.Keys(ctx, user); err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatalf("keys after rotation = %v, %v", ids, err)
	}

	// The user goes with its key and policy.
	if err := s.DeleteAccess(ctx, user, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if st, err = s.User(ctx, user); err != nil || st.Exists {
		t.Errorf("after delete: %+v, %v", st, err)
	}
	if err := s.DeleteAccess(ctx, user, nil); err != nil {
		t.Errorf("delete of a deleted user: %v", err)
	}
}

// A user made by someone else carries no claim tag, so the operator sees it as
// not its own.
func TestMotoUserWithoutTheTagIsNotOurs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := motoConfig(t, "us-east-1")
	user := unique("moto-foreign")
	api := awsiam.NewFromConfig(cfg)
	if _, err := api.CreateUser(ctx, &awsiam.CreateUserInput{UserName: &user}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = api.DeleteUser(context.Background(), &awsiam.DeleteUserInput{UserName: &user}) })
	s := awss3.New(cfg)
	t.Cleanup(s.CloseIdleConnections)
	if st, err := s.User(ctx, user); err != nil || !st.Exists || st.Marker != "" {
		t.Errorf("user = %+v, %v", st, err)
	}
}
