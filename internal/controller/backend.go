// Package controller reconciles Bucket and BucketAccess claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// store, builds the RustFS client through the egress policy and hands the
// framework an External that drives the bucket and IAM calls of package
// s3client.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/types"

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
	OwnedPolicies(ctx context.Context, user string) ([]string, error)
	DeleteAccess(ctx context.Context, user string, keys []string) error
	// ServerVersion is the health probe: an authenticated read of the
	// server's version.
	ServerVersion(ctx context.Context) (string, error)
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
	// Cache keeps the Store of each object store between reconciles; nil
	// builds one for every reconcile.
	Cache *StoreCache
	// Now is the clock of the lifecycle (rotation and grace periods); nil is
	// time.Now. Tests advance it.
	Now func() time.Time
}

// backend is a resolved store.
type backend struct {
	store Store
	spec  s3v1alpha1.ObjectStoreConfig
	conn  server.Connection
	// host is the host of the store's endpoint, for the audit log.
	host string
}

// region is the signing region: the store's, us-east-1 when it names none.
func (b *backend) region() string { return regionOf(b.spec) }

func regionOf(spec s3v1alpha1.ObjectStoreConfig) string {
	if spec.Region == "" {
		return "us-east-1"
	}
	return spec.Region
}

// adminTarget is the connection to a store: its endpoint and signing region,
// the admin credentials of conn and the TLS settings. The claims' reconcilers
// and the health probe both build theirs here.
func adminTarget(spec s3v1alpha1.ObjectStoreConfig, conn server.Connection, dial egress.DialFunc) (s3client.Target, error) {
	access, secret := conn.Admin[AdminAccessKey], conn.Admin[AdminSecretKey]
	if len(access) == 0 || len(secret) == 0 {
		return s3client.Target{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrAdminCredentials)
	}
	return s3client.Target{
		Endpoint: conn.Endpoint, Region: regionOf(spec), AccessKey: string(access), SecretKey: string(secret),
		ServerName: conn.ServerName, CABundle: conn.CABundle, MinTLS: conn.MinTLSVersion, Dial: dial,
	}, nil
}

// backend resolves ref for a claim in ns: the connection settings through
// server.Resolve (which enforces allowedNamespaces), the store's S3 fields,
// and the client, whose connections dial through the egress policy and the
// store's proxy.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	cacheKey := storeKey(ns, ref)
	conn, err := server.Resolve(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	if err != nil {
		if errors.Is(err, server.ErrServerNotFound) {
			c.Cache.Forget(cacheKey)
		}
		return nil, err
	}
	obj, err := c.fetchStore(ctx, ns, ref)
	if err != nil {
		return nil, err
	}
	spec := obj.spec
	dial, err := c.Egress.HostDialer(conn.Proxy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	target, err := adminTarget(spec, conn, dial)
	if err != nil {
		return nil, err
	}
	target.KeepAlive = true
	b := &backend{spec: spec, conn: conn}
	b.store, err = c.Cache.Get(cacheKey, stamp(obj, target, conn.Proxy), func() (Store, error) { return c.NewStore(target) })
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	u, _ := url.Parse(conn.Endpoint) // the store accepted it, so it parses
	b.host = u.Host
	return b, nil
}

// storeObject is the store a claim names, as much of it as the controllers
// use.
type storeObject struct {
	uid        types.UID
	generation int64
	spec       s3v1alpha1.ObjectStoreConfig
}

// fetchStore reads the store, whose S3 fields server.Connection does not
// carry (ADR 0002, open issues), and its identity for the client cache.
func (c Config) fetchStore(ctx context.Context, ns string, ref common.ServerRef) (storeObject, error) {
	if ref.Kind == s3v1alpha1.KindClusterObjectStore {
		var s s3v1alpha1.ClusterObjectStore
		err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s)
		return storeObject{s.UID, s.Generation, s.Spec.ObjectStoreConfig}, err
	}
	var s s3v1alpha1.ObjectStore
	err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s)
	return storeObject{s.UID, s.Generation, s.Spec.ObjectStoreConfig}, err
}

// storeKey names a store in the client cache.
func storeKey(ns string, ref common.ServerRef) string {
	if ref.Kind == s3v1alpha1.KindClusterObjectStore {
		ns = ""
	}
	return ref.Kind + "/" + ns + "/" + ref.Name
}

// stamp identifies what a cached client was built from: the store object (UID
// and generation) and everything the connection takes from outside it, which
// the generation does not cover: the credentials, the CA bundle and the proxy.
// The credentials only go into a hash.
func stamp(o storeObject, t s3client.Target, proxy *url.URL) string {
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte(o.uid), []byte(strconv.FormatInt(o.generation, 10)), []byte(t.Endpoint), []byte(t.Region),
		[]byte(t.AccessKey), []byte(t.SecretKey), []byte(t.ServerName), t.CABundle,
		[]byte(strconv.FormatUint(uint64(t.MinTLS), 10)), []byte(proxyText(proxy)),
	} {
		// Length-prefixed, so ("ab","c") and ("a","bc") differ.
		_, _ = h.Write(strconv.AppendInt(nil, int64(len(part)), 10))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func proxyText(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// claimID is the claim part of the ownership marker.
func claimID(o client.Object) string {
	return fmt.Sprintf("%s/%s/%s", o.GetNamespace(), o.GetName(), o.GetUID())
}
