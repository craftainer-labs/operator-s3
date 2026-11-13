package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

var bucketaccesslog = logf.Log.WithName("bucketaccess-resource")

// SetupBucketAccessWebhookWithManager registers the BucketAccess webhooks with
// the manager.
func SetupBucketAccessWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &s3v1alpha1.BucketAccess{}).
		WithValidator(&BucketAccessValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-s3-craftainer-dev-v1alpha1-bucketaccess,mutating=false,failurePolicy=fail,sideEffects=None,groups=s3.craftainer.dev,resources=bucketaccesses,verbs=create;update,versions=v1alpha1,name=vbucketaccess-v1alpha1.kb.io,admissionReviewVersions=v1

// BucketAccessValidator validates BucketAccesses on create and update.
type BucketAccessValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *BucketAccessValidator) ValidateCreate(_ context.Context, obj *s3v1alpha1.BucketAccess) (admission.Warnings, error) {
	bucketaccesslog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is always admitted.
func (v *BucketAccessValidator) ValidateUpdate(_ context.Context, _, newObj *s3v1alpha1.BucketAccess) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	bucketaccesslog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *BucketAccessValidator) ValidateDelete(_ context.Context, _ *s3v1alpha1.BucketAccess) (admission.Warnings, error) {
	return nil, nil
}

// validate refuses a prefix with a wildcard or a policy variable: the prefix
// is written into an IAM policy, where `*`, `?` and `${...}` would widen it.
// The CRD schema has the same rule; the webhook states it with a clear field.
func (v *BucketAccessValidator) validate(obj *s3v1alpha1.BucketAccess) error {
	var errs field.ErrorList
	if !s3v1alpha1.PrefixOK(obj.Spec.Prefix) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "prefix"), obj.Spec.Prefix, "must not contain *, ? or $"))
	}
	return invalid("BucketAccess", obj.GetName(), errs)
}
