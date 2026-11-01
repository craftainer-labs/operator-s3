// Package v1alpha1 holds the admission webhooks of the sample API. Defaults
// live in the CRD schema, not in a mutating webhook, so only a validating
// webhook is needed: it shows the wiring (serving certificate from
// cert-manager, a ValidatingWebhookConfiguration with a CA injected by
// cert-manager) for rules the schema cannot express.
package v1alpha1

import (
	"context"
	"errors"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	samplev1alpha1 "github.com/craftainer/template-k8s-operator/api/v1alpha1"
)

var greetinglog = logf.Log.WithName("greeting-resource")

// ErrBlankMessage is returned for a message that is empty or only whitespace.
var ErrBlankMessage = errors.New("spec.message must contain a non-blank value")

// SetupGreetingWebhookWithManager registers the Greeting webhooks with the manager.
func SetupGreetingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &samplev1alpha1.Greeting{}).
		WithValidator(&GreetingValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-sample-craftainer-dev-v1alpha1-greeting,mutating=false,failurePolicy=fail,sideEffects=None,groups=sample.craftainer.dev,resources=greetings,verbs=create;update,versions=v1alpha1,name=vgreeting-v1alpha1.kb.io,admissionReviewVersions=v1

// GreetingValidator validates Greetings on create and update.
type GreetingValidator struct{}

// ValidateCreate implements admission.Validator.
func (v *GreetingValidator) ValidateCreate(_ context.Context, obj *samplev1alpha1.Greeting) (admission.Warnings, error) {
	greetinglog.Info("validating create", "name", obj.GetName())
	return nil, validate(obj)
}

// ValidateUpdate implements admission.Validator.
//
// An object that is being deleted is always admitted: the only updates left
// on it are the finalizer removal that completes the delete, and that must
// never be blocked (for example while the webhook's own pod is down).
func (v *GreetingValidator) ValidateUpdate(_ context.Context, _, newObj *samplev1alpha1.Greeting) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() {
		return nil, nil
	}
	greetinglog.Info("validating update", "name", newObj.GetName())
	return nil, validate(newObj)
}

// ValidateDelete implements admission.Validator; deletes are always admitted
// (the verbs marker above does not even send them here).
func (v *GreetingValidator) ValidateDelete(_ context.Context, _ *samplev1alpha1.Greeting) (admission.Warnings, error) {
	return nil, nil
}

func validate(obj *samplev1alpha1.Greeting) error {
	if strings.TrimSpace(obj.Spec.Message) == "" {
		return ErrBlankMessage
	}
	return nil
}
