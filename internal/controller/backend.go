// Package controller reconciles Bucket and BucketAccess claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// store, builds the RustFS client through the egress policy and hands the
// framework an External that drives the bucket and IAM calls of package
// s3client.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/egress"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Keys of the admin Secret (docs/crd-drafts.md).
const (
	AdminAccessKey = "accessKey"
	AdminSecretKey = "secretKey"
)

// ErrAdminCredentials means the admin Secret lacks the access key or secret
// key.
var ErrAdminCredentials = errors.New("the admin Secret needs the keys accessKey and secretKey")

// Store is the RustFS client the controllers use; *s3client.Client is the
// implementation, and tests replace it.
type Store interface {
	Bucket(ctx context.Context, name string) (s3client.BucketState, error)
	CreateBucket(ctx context.Context, p s3client.BucketParams) error
	UpdateBucket(ctx context.Context, p s3client.BucketParams, cur s3client.BucketState) error
	DeleteBucket(ctx context.Context, name string) error
	User(ctx context.Context, name string) (s3client.UserState, error)
	ApplyAccess(ctx context.Context, p s3client.AccessParams, cur s3client.UserState) error
	KeyExists(ctx context.Context, key string) (bool, error)
	IssueKey(ctx context.Context, user, key, secret string) error
	DeleteKey(ctx context.Context, key string) error
	DeleteAccess(ctx context.Context, user string, policies []string) error
}

var _ Store = (*s3client.Client)(nil)

// NewStoreFunc builds the Store of a target; production uses NewClientStore.
type NewStoreFunc func(t s3client.Target) (Store, error)

// NewClientStore is the NewStoreFunc of the operator: the RustFS client.
func NewClientStore(t s3client.Target) (Store, error) {
	c, err := s3client.New(t)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Config is what both reconcilers share.
type Config struct {
	// Client reads and writes claims, stores and Secrets. Its Secret reads must
	// bypass the managed-by cache, which cannot see the admin Secret.
	Client client.Client
	// APIReader looks for binding Secrets (lifecycle.Options.APIReader).
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ManagedBy is the managed-by label of binding Secrets and the audit
	// controller name.
	ManagedBy             string
	OperatorNamespace     string
	DefaultDeletionPolicy common.DeletionPolicy
	Egress                egress.Policy
	NewStore              NewStoreFunc
	// Now is the clock of the lifecycle (rotation and grace periods); nil is
	// time.Now. Tests advance it.
	Now func() time.Time
}

// The kinds a serverRef may name.
const kindClusterObjectStore = "ClusterObjectStore"

// backend is a resolved store.
type backend struct {
	store Store
	spec  s3v1alpha1.ObjectStoreConfig
	conn  server.Connection
	// host is the host of the store's endpoint, for the audit log.
	host string
}

// region is the signing region: the store's, us-east-1 when it names none.
func (b *backend) region() string {
	if b.spec.Region == "" {
		return "us-east-1"
	}
	return b.spec.Region
}

// backend resolves ref for a claim in ns: the connection settings through
// server.Resolve (which enforces allowedNamespaces), the store's S3 fields,
// and the client, whose connections dial through the egress policy and the
// store's proxy.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	conn, err := server.Resolve(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	if err != nil {
		return nil, err
	}
	spec, err := c.storeSpec(ctx, ns, ref)
	if err != nil {
		return nil, err
	}
	dial, err := c.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	access, secret := conn.Admin[AdminAccessKey], conn.Admin[AdminSecretKey]
	if len(access) == 0 || len(secret) == 0 {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	b := &backend{spec: spec, conn: conn}
	b.store, err = c.NewStore(s3client.Target{
		Endpoint: conn.Endpoint, Region: b.region(), AccessKey: string(access), SecretKey: string(secret),
		ServerName: conn.ServerName, CABundle: conn.CABundle, MinTLS: conn.MinTLSVersion, Dial: dial,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	u, _ := url.Parse(conn.Endpoint) // the store accepted it, so it parses
	b.host = u.Host
	return b, nil
}

// storeSpec reads the S3 fields of the store, which server.Connection does not
// carry (ADR 0002, open issues).
func (c Config) storeSpec(ctx context.Context, ns string, ref common.ServerRef) (s3v1alpha1.ObjectStoreConfig, error) {
	if ref.Kind == kindClusterObjectStore {
		var s s3v1alpha1.ClusterObjectStore
		err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s)
		return s.Spec.ObjectStoreConfig, err
	}
	var s s3v1alpha1.ObjectStore
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	return s.Spec.ObjectStoreConfig, err
}

// claimID is the claim part of the ownership marker.
func claimID(o client.Object) string {
	return fmt.Sprintf("%s/%s/%s", o.GetNamespace(), o.GetName(), o.GetUID())
}
