package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

var bucketlog = logf.Log.WithName("bucket-resource")

// minQuotaBytes is the smallest quota.hardBytes the API allows.
var minQuotaBytes = resource.MustParse("1")

// SetupBucketWebhookWithManager registers the Bucket webhooks with the manager.
func SetupBucketWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &s3v1alpha1.Bucket{}).
		WithValidator(&BucketValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-s3-craftainer-dev-v1alpha1-bucket,mutating=false,failurePolicy=fail,sideEffects=None,groups=s3.craftainer.dev,resources=buckets,verbs=create;update,versions=v1alpha1,name=vbucket-v1alpha1.kb.io,admissionReviewVersions=v1

// BucketValidator validates Buckets on create and update.
type BucketValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *BucketValidator) ValidateCreate(_ context.Context, obj *s3v1alpha1.Bucket) (admission.Warnings, error) {
	bucketlog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is always admitted.
func (v *BucketValidator) ValidateUpdate(_ context.Context, _, newObj *s3v1alpha1.Bucket) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	bucketlog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *BucketValidator) ValidateDelete(_ context.Context, _ *s3v1alpha1.Bucket) (admission.Warnings, error) {
	return nil, nil
}

func (v *BucketValidator) validate(obj *s3v1alpha1.Bucket) error {
	var errs field.ErrorList
	if q := obj.Spec.Quota; q != nil && q.HardBytes.Cmp(minQuotaBytes) < 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "quota", "hardBytes"), q.HardBytes.String(), "must be at least 1 byte"))
	}
	return invalid("Bucket", obj.GetName(), errs)
}
