package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

// listStub answers List with a fixed set of accesses and records the options.
type listStub struct {
	client.Reader
	items []s3v1alpha1.BucketAccess
	err   error
	opts  *client.ListOptions
}

func (l *listStub) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if l.err != nil {
		return l.err
	}
	l.opts = &client.ListOptions{}
	l.opts.ApplyOptions(opts)
	list.(*s3v1alpha1.BucketAccessList).Items = l.items
	return nil
}

func TestIndexBucketRef(t *testing.T) {
	a := &s3v1alpha1.BucketAccess{}
	a.Spec.BucketRef.Name = "uploads"
	if got := indexBucketRef(a); len(got) != 1 || got[0] != "uploads" {
		t.Errorf("index = %v", got)
	}
}

// A Bucket event reaches the accesses of its namespace that name it.
func TestAccessesOfBucket(t *testing.T) {
	ctx := context.Background()
	mk := func(name string) s3v1alpha1.BucketAccess {
		return s3v1alpha1.BucketAccess{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant"}}
	}
	stub := &listStub{items: []s3v1alpha1.BucketAccess{mk("one"), mk("two")}}
	bucket := &s3v1alpha1.Bucket{ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "tenant"}}

	reqs := accessesOfBucket(stub)(ctx, bucket)
	if len(reqs) != 2 || reqs[0].Name != "one" || reqs[1].Namespace != "tenant" {
		t.Errorf("requests = %v", reqs)
	}
	if stub.opts.Namespace != "tenant" || stub.opts.FieldSelector.String() != bucketRefIndex+"=uploads" {
		t.Errorf("list options = %+v", stub.opts)
	}

	stub.err = errStore
	if reqs := accessesOfBucket(stub)(ctx, bucket); len(reqs) != 0 {
		t.Errorf("a failed list gave %v", reqs)
	}
}

// failingIndexer and indexFailure make the registration of the index fail.
type failingIndexer struct{}

func (failingIndexer) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return errStore
}

type indexFailure struct{ ctrl.Manager }

func (indexFailure) GetFieldIndexer() client.FieldIndexer { return failingIndexer{} }

func TestAccessSetupReportsAnIndexError(t *testing.T) {
	if err := (&AccessReconciler{}).SetupWithManager(indexFailure{}); !errors.Is(err, errStore) {
		t.Errorf("setup: %v", err)
	}
}
