package controller

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/craftainer-labs/lib-operator-go/pkg/apis/common"
	"github.com/craftainer-labs/lib-operator-go/pkg/server"

	"github.com/craftainer-labs/operator-s3/internal/s3client"
)

// closeStore is a Store that counts the closing of its idle connections.
type closeStore struct {
	*memStore
	closed int
}

func (c *closeStore) CloseIdleConnections() { c.closed++ }

func TestStoreCache(t *testing.T) {
	c := NewStoreCache(time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }
	builds := 0
	build := func() (Store, error) { builds++; return &closeStore{memStore: newMemStore()}, nil }

	s1, err := c.Get("k", "v1", build)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Get("k", "v1", build); again != s1 || builds != 1 {
		t.Errorf("a hit built a new store (builds = %d)", builds)
	}

	// A new stamp replaces the entry and closes the old connections.
	s2, _ := c.Get("k", "v2", build)
	if s2 == s1 || builds != 2 || s1.(*closeStore).closed != 1 {
		t.Errorf("invalidation: builds = %d, closed = %d", builds, s1.(*closeStore).closed)
	}

	// A failed build caches nothing and keeps no stale entry.
	if _, err := c.Get("bad", "v", func() (Store, error) { return nil, errStore }); !errors.Is(err, errStore) || c.Len() != 1 {
		t.Errorf("failed build: %v, len = %d", err, c.Len())
	}
	if _, err := c.Get("k", "v3", func() (Store, error) { return nil, errStore }); !errors.Is(err, errStore) || c.Len() != 0 {
		t.Errorf("failed rebuild: %v, len = %d", err, c.Len())
	}

	// A store that cannot close idle connections is dropped all the same.
	if _, err := c.Get("plain", "v", func() (Store, error) { return newMemStore(), nil }); err != nil {
		t.Fatal(err)
	}
	c.Forget("plain")
	c.Forget("plain")
	if c.Len() != 0 {
		t.Errorf("len after Forget = %d", c.Len())
	}

	// Removal: the store is gone.
	s3, _ := c.Get("gone", "v", build)
	c.Forget("gone")
	if c.Len() != 0 || s3.(*closeStore).closed != 1 {
		t.Errorf("Forget: len = %d, closed = %d", c.Len(), s3.(*closeStore).closed)
	}

	// An entry that is not used for longer than the idle time is dropped by
	// the next Get, whatever its key.
	s4, _ := c.Get("old", "v", build)
	now = now.Add(30 * time.Second)
	_, _ = c.Get("young", "v", build)
	now = now.Add(45 * time.Second)
	_, _ = c.Get("young", "v", build)
	if c.Len() != 1 || s4.(*closeStore).closed != 1 {
		t.Errorf("idle: len = %d, closed = %d", c.Len(), s4.(*closeStore).closed)
	}
}

func TestNilStoreCacheCachesNothing(t *testing.T) {
	var c *StoreCache
	builds := 0
	for range 2 {
		if _, err := c.Get("k", "v", func() (Store, error) { builds++; return newMemStore(), nil }); err != nil {
			t.Fatal(err)
		}
	}
	c.Forget("k")
	if builds != 2 {
		t.Errorf("builds = %d", builds)
	}
}

func TestStampCoversEverythingTheClientIsBuiltFrom(t *testing.T) {
	o := storeObject{uid: "uid", generation: 1}
	tg := s3client.Target{Endpoint: "https://s", AccessKey: "a", SecretKey: "s", CABundle: []byte("ca")}
	proxy, _ := url.Parse("http://proxy:3128")
	base := stamp(o, tg, nil)
	if stamp(o, tg, nil) != base {
		t.Error("the stamp is not stable")
	}
	changed := map[string]string{
		"proxy":       stamp(o, tg, proxy),
		"uid":         stamp(storeObject{uid: "other", generation: 1}, tg, nil),
		"generation":  stamp(storeObject{uid: "uid", generation: 2}, tg, nil),
		"secret key":  stamp(o, s3client.Target{Endpoint: "https://s", AccessKey: "a", SecretKey: "t", CABundle: []byte("ca")}, nil),
		"CA bundle":   stamp(o, s3client.Target{Endpoint: "https://s", AccessKey: "a", SecretKey: "s", CABundle: []byte("cb")}, nil),
		"field split": stamp(o, s3client.Target{Endpoint: "https://s", AccessKey: "as", SecretKey: "", CABundle: []byte("ca")}, nil),
	}
	for what, got := range changed {
		if got == base {
			t.Errorf("a different %s gives the same stamp", what)
		}
	}
}

// The controllers reuse the Store of a store until the store or its
// credentials change, and drop it when the store is gone.
func TestBackendCachesTheStore(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, common.DeletionRetain, nil)
	h.cfg.Cache = NewStoreCache(time.Hour)
	builds := func() int {
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		return len(h.fake.targets)
	}
	resolve := func() {
		t.Helper()
		if _, err := h.cfg.backend(ctx, h.ns, h.ref()); err != nil {
			t.Fatal(err)
		}
	}

	resolve()
	resolve()
	if builds() != 1 || !h.fake.targets[0].KeepAlive {
		t.Fatalf("builds = %d, targets = %+v", builds(), h.fake.targets)
	}

	// The credentials change.
	var admin corev1.Secret
	h.must(env.Client.Get(ctx, client.ObjectKey{Namespace: h.ops, Name: h.server.Spec.AdminCredentialsSecretRef.Name}, &admin))
	admin.Data["secretKey"] = []byte("rotated")
	h.must(env.Client.Update(ctx, &admin))
	resolve()
	if builds() != 2 {
		t.Errorf("after a credentials change: builds = %d", builds())
	}

	// The store changes (its generation).
	h.patch(h.server, func() { h.server.Spec.Region = "eu-west-1" })
	resolve()
	resolve()
	if builds() != 3 {
		t.Errorf("after a spec change: builds = %d", builds())
	}

	// The store is deleted: its entry goes.
	h.must(env.Client.Delete(ctx, h.server))
	if _, err := h.cfg.backend(ctx, h.ns, h.ref()); !errors.Is(err, server.ErrServerNotFound) {
		t.Errorf("a deleted store: %v", err)
	}
	if h.cfg.Cache.Len() != 0 {
		t.Errorf("len = %d", h.cfg.Cache.Len())
	}
}
