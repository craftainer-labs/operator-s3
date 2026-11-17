// Package controller reconciles Bucket and BucketAccess claims with
// lib-operator-go's lifecycle.Reconcile (ADR 0002): it resolves the claim's
// store, builds the RustFS client through the egress policy and hands the
// framework an External that drives the bucket and IAM calls of package
// s3client.
package controller

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"k8s.io/apimachinery/pkg/types"

	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/cloud"
	"github.com/craftainer-labs/lib-operator-go/pkg/egress"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
	"github.com/craftainer-labs/operator-s3/internal/awss3"
	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// Keys of the admin Secret (docs/crd-drafts.md).
const (
	AdminAccessKey = "accessKey"
	AdminSecretKey = "secretKey"
)

// ErrCloudStore means the store's provider block is not one this operator
// serves (only AWS, with its aws block; the CRD's rules admit nothing else, so
// this is a guard against a store that skipped them). It is wrapped in
// server.ErrInvalidServer, so a claim and the probe report it as a store that
// cannot be used.
var ErrCloudStore = errors.New("the store's cloud provider is not supported")

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

var (
	_ Store = (*s3client.Client)(nil)
	_ Store = (*awss3.Store)(nil)
)

// NewAWSStoreFunc builds the Store of an AWS configuration; production uses
// NewAWSStore.
type NewAWSStoreFunc func(cfg aws.Config) Store

// NewAWSStore is the NewAWSStoreFunc of the operator: the S3 client of the
// account.
func NewAWSStore(cfg aws.Config) Store { return awss3.New(cfg) }

// ProbeAWSFunc checks that an AWS account is usable; production uses
// awss3.Probe.
type ProbeAWSFunc func(ctx context.Context, cfg aws.Config) error

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
	// EgressMode is the mode Egress was built from: pkg/cloud needs the mode
	// itself (an endpointOverride is honoured only in allow-private) as well as
	// the policy.
	EgressMode egress.Mode
	NewStore   NewStoreFunc
	// NewAWSStore and ProbeAWS serve the stores whose provider is AWS.
	NewAWSStore NewAWSStoreFunc
	ProbeAWS    ProbeAWSFunc
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

// cloudStore is the provider of a store that runs on a cloud; the zero value
// (set false) is a self-hosted store.
type cloudStore struct {
	set   bool
	scope cloud.Scope
	aws   cloud.AWSClusterBlock
	creds *cloud.Credentials
}

// clusterCloud reads the provider of a ClusterObjectStore.
func clusterCloud(s *s3v1alpha1.ClusterObjectStore) (cloudStore, error) {
	p := s.Spec.Provider
	if p == nil {
		return cloudStore{}, nil
	}
	if p.Type != cloud.ProviderAWS || p.AWS == nil {
		return cloudStore{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrCloudStore)
	}
	return cloudStore{set: true, scope: cloud.ScopeCluster, aws: *p.AWS, creds: s.Spec.Credentials}, nil
}

// namespacedCloud reads the provider of an ObjectStore. It has no
// endpointOverride, which the lib type leaves out by design.
func namespacedCloud(s *s3v1alpha1.ObjectStore) (cloudStore, error) {
	p := s.Spec.Provider
	if p == nil {
		return cloudStore{}, nil
	}
	if p.Type != cloud.ProviderAWS || p.AWS == nil {
		return cloudStore{}, fmt.Errorf("%w: %w", server.ErrInvalidServer, ErrCloudStore)
	}
	return cloudStore{
		set: true, scope: cloud.ScopeNamespaced, aws: cloud.AWSClusterBlock{AWSBlock: *p.AWS},
		creds: s.Spec.Credentials.AsCredentials(),
	}, nil
}

// awsEndpoint is the address of the S3 API of an AWS store: the emulator
// override, or the regional endpoint (the China partition has its own domain).
func awsEndpoint(b cloud.AWSClusterBlock) string {
	if b.EndpointOverride != "" {
		return b.EndpointOverride
	}
	suffix := ""
	if strings.HasPrefix(b.Region, "cn-") {
		suffix = ".cn"
	}
	return "https://s3." + b.Region + ".amazonaws.com" + suffix
}

// cloudEgress is the egress of a cloud store: the operator's mode and policy,
// the store's proxy and, when it names one, its CA bundle. Its serverName and
// minVersion do not apply to the SDK.
func (c Config) cloudEgress(conn server.Connection) (cloud.Egress, error) {
	e := cloud.Egress{Mode: c.EgressMode, Policy: c.Egress, Proxy: conn.Proxy}
	if len(conn.CABundle) == 0 {
		return e, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(conn.CABundle) {
		return cloud.Egress{}, s3client.ErrInvalidCA
	}
	e.RootCAs = pool
	return e, nil
}

// awsConfig builds the AWS configuration of a store through pkg/cloud, which
// retrieves the credentials once to prove they work.
func (c Config) awsConfig(ctx context.Context, cs cloudStore, cc server.CloudConnection) (aws.Config, error) {
	e, err := c.cloudEgress(cc.Connection)
	if err != nil {
		return aws.Config{}, err
	}
	return e.AWS(ctx, cs.scope, cs.aws, cs.creds, cc.Credentials)
}

// awsBackend resolves a store that runs on AWS. The Store is kept between
// reconciles like a RustFS client, and is built (the credentials retrieved)
// only when the store, its credentials, its CA bundle or its proxy changed.
func (c Config) awsBackend(ctx context.Context, cacheKey string, obj storeObject, cc server.CloudConnection) (*backend, error) {
	spec := obj.spec
	spec.Region = obj.cloud.aws.Region
	b := &backend{spec: spec, conn: cc.Connection, host: hostOf(awsEndpoint(obj.cloud.aws))}
	var err error
	b.store, err = c.Cache.Get(cacheKey, awsStamp(obj, cc), func() (Store, error) {
		cfg, err := c.awsConfig(ctx, obj.cloud, cc)
		if err != nil {
			return nil, err
		}
		return c.NewAWSStore(cfg), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", server.ErrInvalidServer, err)
	}
	return b, nil
}

// hostOf is the host of an endpoint, for the audit log; "" for one that does
// not parse (the schema only checks the scheme of an endpointOverride, and
// pkg/cloud refuses a bad one when the configuration is built).
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

// region is the signing region: the store's, us-east-1 when it names none.
func (b *backend) region() string { return regionOf(b.spec) }

func regionOf(spec s3v1alpha1.ObjectStoreConfig) string { return spec.RegionOf() }

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
// server.ResolveCloud (which enforces allowedNamespaces), the store's S3 fields,
// and the client, whose connections dial through the egress policy and the
// store's proxy.
func (c Config) backend(ctx context.Context, ns string, ref common.ServerRef) (*backend, error) {
	cacheKey := storeKey(ns, ref)
	cc, err := server.ResolveCloud(ctx, c.Client, ns, ref, server.WithOperatorNamespace(c.OperatorNamespace))
	conn := cc.Connection
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
	if obj.cloud.set {
		return c.awsBackend(ctx, cacheKey, obj, cc)
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
	cloud      cloudStore
}

// fetchStore reads the store, whose S3 fields server.Connection does not
// carry (ADR 0002, open issues), and its identity for the client cache.
func (c Config) fetchStore(ctx context.Context, ns string, ref common.ServerRef) (storeObject, error) {
	if ref.Kind == s3v1alpha1.KindClusterObjectStore {
		var s s3v1alpha1.ClusterObjectStore
		if err := c.Client.Get(ctx, client.ObjectKey{Name: ref.Name}, &s); err != nil {
			return storeObject{}, err
		}
		cs, err := clusterCloud(&s)
		return storeObject{s.UID, s.Generation, s.Spec.ObjectStoreConfig, cs}, err
	}
	var s s3v1alpha1.ObjectStore
	if err := c.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, &s); err != nil {
		return storeObject{}, err
	}
	cs, err := namespacedCloud(&s)
	return storeObject{s.UID, s.Generation, s.Spec.ObjectStoreConfig, cs}, err
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
	return hashParts(
		[]byte(o.uid), []byte(strconv.FormatInt(o.generation, 10)), []byte(t.Endpoint), []byte(t.Region),
		[]byte(t.AccessKey), []byte(t.SecretKey), []byte(t.ServerName), t.CABundle,
		[]byte(strconv.FormatUint(uint64(t.MinTLS), 10)), []byte(proxyText(proxy)),
	)
}

// awsStamp is stamp for a store on AWS: the store object (its generation
// covers region, role and endpoint) and the data of the credentials Secret, the
// CA bundle and the proxy, which it does not. A workload identity has no
// Secret; the SDK renews its token itself.
func awsStamp(o storeObject, cc server.CloudConnection) string {
	parts := [][]byte{
		[]byte(o.uid), []byte(strconv.FormatInt(o.generation, 10)), cc.CABundle, []byte(proxyText(cc.Proxy)),
	}
	for _, k := range slices.Sorted(maps.Keys(cc.Credentials)) {
		parts = append(parts, []byte(k), cc.Credentials[k])
	}
	return hashParts(parts...)
}

// hashParts hashes the parts, each length-prefixed so ("ab","c") and ("a","bc")
// differ. The credentials only go into a hash.
func hashParts(parts ...[]byte) string {
	h := sha256.New()
	for _, part := range parts {
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
