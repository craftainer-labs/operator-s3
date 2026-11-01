// Package controller holds the sample Greeting reconciler. It shows the
// conventions every operator built from this template follows: a finalizer
// with a deletion policy, Ready and Synced conditions set on the first
// reconcile with observedGeneration, drift correction on a requeue interval
// (overridable per object), Events, and a reconcile counter.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	samplev1alpha1 "github.com/craftainer/template-k8s-operator/api/v1alpha1"
)

const (
	// Finalizer is the finalizer every claim of the operator family carries.
	Finalizer = "craftainer.dev/finalizer"
	// PollIntervalAnnotation overrides DefaultPollInterval for one object.
	PollIntervalAnnotation = "craftainer.dev/poll-interval"
	// DefaultPollInterval is how often an object is re-checked for drift.
	DefaultPollInterval = 10 * time.Minute

	// ConditionReady summarises the object.
	ConditionReady = "Ready"
	// ConditionSynced says whether the provisioned object matches the spec.
	ConditionSynced = "Synced"

	// MessageKey is the ConfigMap key the Greeting's message is written to.
	MessageKey = "message"

	// MinPollInterval is the smallest drift-check interval an annotation can
	// ask for; a smaller value is raised to it, so no object can make the
	// operator requeue in a hot loop.
	MinPollInterval = 10 * time.Second

	// ManagedByLabel marks a ConfigMap the operator provisioned. It stays on
	// a ConfigMap released by deletionPolicy Retain, which is how a
	// re-created Greeting of the same name recognises it and re-adopts it.
	ManagedByLabel = "craftainer.dev/managed-by"
	// ManagedByValue is the value of ManagedByLabel.
	ManagedByValue = "greeting-operator"

	// conflictRequeue is the delay before retrying after a write conflict.
	conflictRequeue = time.Second
)

// ErrNameCollision means the ConfigMap a Greeting would provision already exists
// and belongs to someone else. The ownership rule: an existing ConfigMap is
// taken over only if this Greeting controls it, or if it has no controller and
// carries ManagedByLabel (one released by deletionPolicy Retain). Anything
// else, including an object without the label, is never touched.
var ErrNameCollision = errors.New("name collision")

// +kubebuilder:rbac:groups=sample.craftainer.dev,resources=greetings,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sample.craftainer.dev,resources=greetings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sample.craftainer.dev,resources=greetings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// GreetingReconciler reconciles a Greeting object.
type GreetingReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// Reconcile moves the cluster towards the Greeting's spec.
func (r *GreetingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	defer func() { countReconcile(err) }()
	result, err = r.reconcile(ctx, req)
	if apierrors.IsConflict(err) {
		// Someone else wrote the object first. That is routine, not a failure:
		// retry shortly against fresh state, without an error or an Event.
		return ctrl.Result{RequeueAfter: conflictRequeue}, nil
	}
	return result, err
}

func (r *GreetingReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var greeting samplev1alpha1.Greeting
	if err := r.Get(ctx, req.NamespacedName, &greeting); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !greeting.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &greeting)
	}

	if controllerutil.AddFinalizer(&greeting, Finalizer) {
		if err := r.Update(ctx, &greeting); err != nil {
			return ctrl.Result{}, err
		}
	}

	op, err := r.syncConfigMap(ctx, &greeting)
	if apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	if err != nil {
		r.recordFailure(ctx, &greeting, err)
		return ctrl.Result{}, err
	}
	if op != controllerutil.OperationResultNone {
		log.Info("provisioned ConfigMap", "operation", op)
		r.Recorder.Eventf(&greeting, nil, corev1.EventTypeNormal, "Synced", "Sync", "ConfigMap %s %s", greeting.Name, op)
	}

	interval, warning := pollInterval(&greeting)
	if warning != "" && greeting.Status.ObservedGeneration != greeting.Generation {
		// Once per generation: later reconciles of the same spec stay quiet.
		r.Recorder.Eventf(&greeting, nil, corev1.EventTypeWarning, "InvalidPollInterval", "Sync", "%s", warning)
	}

	greeting.Status.ConfigMapName = greeting.Name
	setConditions(&greeting, metav1.ConditionTrue, "ReconcileSuccess", "the ConfigMap matches the spec")
	if err := r.Status().Update(ctx, &greeting); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// syncConfigMap creates or updates the ConfigMap the Greeting provisions.
func (r *GreetingReconciler) syncConfigMap(ctx context.Context, greeting *samplev1alpha1.Greeting) (controllerutil.OperationResult, error) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: greeting.Name, Namespace: greeting.Namespace}}
	return controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		// Never take over an object this Greeting did not create: the name is
		// derived from the Greeting, so a collision is a conflict, not a hint.
		if cm.ResourceVersion != "" && !metav1.IsControlledBy(cm, greeting) && !released(cm) { // an existing object
			return fmt.Errorf("%w: ConfigMap %s/%s exists and is not controlled by this Greeting", ErrNameCollision, cm.Namespace, cm.Name)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[ManagedByLabel] = ManagedByValue
		cm.Data[MessageKey] = greeting.Spec.Message
		return controllerutil.SetControllerReference(greeting, cm, r.Scheme)
	})
}

// released reports whether cm was provisioned by the operator and then let go
// by deletionPolicy Retain: it has no controller and still carries the label.
func released(cm *corev1.ConfigMap) bool {
	return metav1.GetControllerOf(cm) == nil && cm.Labels[ManagedByLabel] == ManagedByValue
}

// finalize applies the deletion policy, then releases the Greeting.
func (r *GreetingReconciler) finalize(ctx context.Context, greeting *samplev1alpha1.Greeting) error {
	if !controllerutil.ContainsFinalizer(greeting, Finalizer) {
		return nil
	}
	cm := &corev1.ConfigMap{}
	done := false // whether an owned ConfigMap was actually deleted or released
	err := r.Get(ctx, client.ObjectKeyFromObject(greeting), cm)
	switch {
	case apierrors.IsNotFound(err):
		// Already gone: nothing to delete or release.
	case err != nil:
		return err
	case !metav1.IsControlledBy(cm, greeting):
		// Not ours (see syncConfigMap): leave it alone.
	case greeting.Spec.DeletionPolicy == samplev1alpha1.DeletionPolicyRetain:
		// Release the ConfigMap from garbage collection before the owner goes.
		if err := controllerutil.RemoveControllerReference(greeting, cm, r.Scheme); err != nil {
			return err
		}
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
		done = true
	default:
		if err := r.Delete(ctx, cm); client.IgnoreNotFound(err) != nil {
			return err
		}
		done = true
	}
	if done {
		r.Recorder.Eventf(greeting, nil, corev1.EventTypeNormal, "Deleted", "Delete", "released ConfigMap %s (%s)", greeting.Name, greeting.Spec.DeletionPolicy)
	}
	controllerutil.RemoveFinalizer(greeting, Finalizer)
	return r.Update(ctx, greeting)
}

// recordFailure reports a failed sync on the object. Status errors are only
// logged: the reconcile already returns the error that caused them.
func (r *GreetingReconciler) recordFailure(ctx context.Context, greeting *samplev1alpha1.Greeting, cause error) {
	reason := "ReconcileError"
	if errors.Is(cause, ErrNameCollision) {
		reason = "NameCollision"
	}
	r.Recorder.Eventf(greeting, nil, corev1.EventTypeWarning, reason, "Sync", "%s", cause.Error())
	setConditions(greeting, metav1.ConditionFalse, reason, cause.Error())
	if err := r.Status().Update(ctx, greeting); err != nil {
		logf.FromContext(ctx).Error(err, "cannot record the failure in status")
	}
}

// pollInterval returns the object's drift-check interval and, when the
// annotation is unusable, a message saying what was done instead: the
// annotation is used if it parses as a duration of at least MinPollInterval; a
// shorter one is raised to it, and anything else falls back to the default.
func pollInterval(greeting *samplev1alpha1.Greeting) (time.Duration, string) {
	raw, ok := greeting.Annotations[PollIntervalAnnotation]
	if !ok {
		return DefaultPollInterval, ""
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil || d <= 0:
		return DefaultPollInterval, fmt.Sprintf("ignoring %s=%q, using %s", PollIntervalAnnotation, raw, DefaultPollInterval)
	case d < MinPollInterval:
		return MinPollInterval, fmt.Sprintf("raising %s=%q to the minimum %s", PollIntervalAnnotation, raw, MinPollInterval)
	}
	return d, ""
}

// setConditions sets Ready and Synced together. The operator sets both on
// every reconcile, so they exist after the first one.
func setConditions(greeting *samplev1alpha1.Greeting, status metav1.ConditionStatus, reason, message string) {
	greeting.Status.ObservedGeneration = greeting.Generation
	for _, conditionType := range []string{ConditionReady, ConditionSynced} {
		meta.SetStatusCondition(&greeting.Status.Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: greeting.Generation,
		})
	}
}

// SetupWithManager registers the reconciler with the manager.
func (r *GreetingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&samplev1alpha1.Greeting{}).
		Owns(&corev1.ConfigMap{}).
		Named("greeting").
		Complete(r)
}
