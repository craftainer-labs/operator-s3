package v1alpha1

import (
	"context"

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

var objectstorelog = logf.Log.WithName("objectstore-resource")

// validateStore checks the two URLs of a store, for both store kinds. The
// schema already requires the endpoint to start with https://; the parse here
// also rejects a missing host and malformed URLs.
func validateStore(spec *s3v1alpha1.ObjectStoreConfig, proxyURL string) field.ErrorList {
	var errs field.ErrorList
	path := field.NewPath("spec")
	if !validURL(spec.Endpoint, "https") {
		errs = append(errs, field.Invalid(path.Child("endpoint"), redacted, "must be an https URL with a host and without user:password@"))
	}
	if proxyURL != "" && !validURL(proxyURL, "http", "https", "socks5") {
		errs = append(errs, field.Invalid(path.Child("proxyURL"), redacted, "must be an http, https or socks5 URL with a host and without user:password@"))
	}
	return errs
}

// SetupObjectStoreWebhookWithManager registers the ObjectStore webhooks with the manager.
func SetupObjectStoreWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &s3v1alpha1.ObjectStore{}).
		WithValidator(&ObjectStoreValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-s3-craftainer-dev-v1alpha1-objectstore,mutating=false,failurePolicy=fail,sideEffects=None,groups=s3.craftainer.dev,resources=objectstores,verbs=create;update,versions=v1alpha1,name=vobjectstore-v1alpha1.kb.io,admissionReviewVersions=v1

// ObjectStoreValidator validates ObjectStores on create and update.
type ObjectStoreValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *ObjectStoreValidator) ValidateCreate(_ context.Context, obj *s3v1alpha1.ObjectStore) (admission.Warnings, error) {
	objectstorelog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is always admitted.
func (v *ObjectStoreValidator) ValidateUpdate(_ context.Context, _, newObj *s3v1alpha1.ObjectStore) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	objectstorelog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *ObjectStoreValidator) ValidateDelete(_ context.Context, _ *s3v1alpha1.ObjectStore) (admission.Warnings, error) {
	return nil, nil
}

func (v *ObjectStoreValidator) validate(obj *s3v1alpha1.ObjectStore) error {
	return invalid("ObjectStore", obj.GetName(), validateStore(&obj.Spec.ObjectStoreConfig, obj.Spec.ProxyURL))
}
