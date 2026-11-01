package v1alpha1

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	s3v1alpha1 "github.com/craftainer-labs/operator-s3/api/v1alpha1"
)

var clusterobjectstorelog = logf.Log.WithName("clusterobjectstore-resource")

// SetupClusterObjectStoreWebhookWithManager registers the ClusterObjectStore webhooks with the manager.
func SetupClusterObjectStoreWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &s3v1alpha1.ClusterObjectStore{}).
		WithValidator(&ClusterObjectStoreValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-s3-craftainer-dev-v1alpha1-clusterobjectstore,mutating=false,failurePolicy=fail,sideEffects=None,groups=s3.craftainer.dev,resources=clusterobjectstores,verbs=create;update,versions=v1alpha1,name=vclusterobjectstore-v1alpha1.kb.io,admissionReviewVersions=v1

// ClusterObjectStoreValidator validates ClusterObjectStores on create and update.
type ClusterObjectStoreValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *ClusterObjectStoreValidator) ValidateCreate(_ context.Context, obj *s3v1alpha1.ClusterObjectStore) (admission.Warnings, error) {
	clusterobjectstorelog.Info("validating create", "name", obj.GetName())
	return nil, v.validate(obj)
}

// ValidateUpdate implements admission.Validator. An object that is being
// deleted is always admitted.
func (v *ClusterObjectStoreValidator) ValidateUpdate(_ context.Context, _, newObj *s3v1alpha1.ClusterObjectStore) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	clusterobjectstorelog.Info("validating update", "name", newObj.GetName())
	return nil, v.validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *ClusterObjectStoreValidator) ValidateDelete(_ context.Context, _ *s3v1alpha1.ClusterObjectStore) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClusterObjectStoreValidator) validate(obj *s3v1alpha1.ClusterObjectStore) error {
	return invalid("ClusterObjectStore", obj.GetName(), validateStore(&obj.Spec.ObjectStoreConfig, obj.Spec.ProxyURL))
}
