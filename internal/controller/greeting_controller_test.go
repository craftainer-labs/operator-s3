package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	samplev1alpha1 "github.com/craftainer/template-k8s-operator/api/v1alpha1"
)

var _ = Describe("Greeting controller", func() {
	newGreetingIn := func(name string, spec samplev1alpha1.GreetingSpec) *samplev1alpha1.Greeting {
		return &samplev1alpha1.Greeting{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
	}
	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: "default"} }

	ready := func(name string) func(Gomega) {
		return func(g Gomega) {
			var got samplev1alpha1.Greeting
			g.Expect(k8sClient.Get(ctx, key(name), &got)).To(Succeed())
			for _, ct := range []string{ConditionReady, ConditionSynced} {
				c := meta.FindStatusCondition(got.Status.Conditions, ct)
				g.Expect(c).NotTo(BeNil())
				g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
				g.Expect(c.ObservedGeneration).To(Equal(got.Generation))
			}
		}
	}

	It("defaults the spec, provisions a ConfigMap and becomes Ready", func() {
		Expect(k8sClient.Create(ctx, newGreetingIn("defaults", samplev1alpha1.GreetingSpec{}))).To(Succeed())
		Eventually(ready("defaults")).Should(Succeed())

		var got samplev1alpha1.Greeting
		Expect(k8sClient.Get(ctx, key("defaults"), &got)).To(Succeed())
		Expect(got.Spec.Message).To(Equal("Hello"))
		Expect(got.Spec.DeletionPolicy).To(Equal(samplev1alpha1.DeletionPolicyDelete))

		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, key("defaults"), &cm)).To(Succeed())
		Expect(cm.Data).To(HaveKeyWithValue(MessageKey, "Hello"))

		By("following a spec change")
		got.Spec.Message = "Bonjour"
		Expect(k8sClient.Update(ctx, &got)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key("defaults"), &cm)).To(Succeed())
			g.Expect(cm.Data[MessageKey]).To(Equal("Bonjour"))
		}).Should(Succeed())
		Eventually(ready("defaults")).Should(Succeed())

		By("deleting the ConfigMap with the Greeting (policy Delete)")
		Expect(k8sClient.Delete(ctx, &got)).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key("defaults"), &samplev1alpha1.Greeting{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key("defaults"), &corev1.ConfigMap{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("keeps the ConfigMap when the deletion policy is Retain", func() {
		Expect(k8sClient.Create(ctx, newGreetingIn("retained", samplev1alpha1.GreetingSpec{
			Message: "keep me", DeletionPolicy: samplev1alpha1.DeletionPolicyRetain,
		}))).To(Succeed())
		Eventually(ready("retained")).Should(Succeed())

		Expect(k8sClient.Delete(ctx, newGreetingIn("retained", samplev1alpha1.GreetingSpec{}))).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key("retained"), &samplev1alpha1.Greeting{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())

		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, key("retained"), &cm)).To(Succeed())
		Expect(cm.OwnerReferences).To(BeEmpty())
		Expect(k8sClient.Delete(ctx, &cm)).To(Succeed())
	})

	It("refuses to take over a ConfigMap it did not create", func() {
		foreign := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: "default"},
			Data:       map[string]string{MessageKey: "not yours"},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		Expect(k8sClient.Create(ctx, newGreetingIn("taken", samplev1alpha1.GreetingSpec{}))).To(Succeed())

		Eventually(func(g Gomega) {
			var got samplev1alpha1.Greeting
			g.Expect(k8sClient.Get(ctx, key("taken"), &got)).To(Succeed())
			c := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			g.Expect(c).NotTo(BeNil())
			g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(c.Reason).To(Equal("NameCollision"))
		}).Should(Succeed())

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(foreign), foreign)).To(Succeed())
		Expect(foreign.Data[MessageKey]).To(Equal("not yours"))

		Expect(k8sClient.Delete(ctx, newGreetingIn("taken", samplev1alpha1.GreetingSpec{}))).To(Succeed())
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, key("taken"), &samplev1alpha1.Greeting{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
	})

	It("rejects an invalid spec at the API server", func() {
		err := k8sClient.Create(ctx, newGreetingIn("bad-policy", samplev1alpha1.GreetingSpec{DeletionPolicy: "Maybe"}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})
})
