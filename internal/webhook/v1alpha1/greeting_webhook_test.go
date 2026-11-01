package v1alpha1

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	samplev1alpha1 "github.com/craftainer/template-k8s-operator/api/v1alpha1"
)

func greetingWith(message string) *samplev1alpha1.Greeting {
	return &samplev1alpha1.Greeting{
		ObjectMeta: metav1.ObjectMeta{Name: "hello", Namespace: "default"},
		Spec:       samplev1alpha1.GreetingSpec{Message: message},
	}
}

// Unit tests of the validator itself.
func TestValidator(t *testing.T) {
	g := NewWithT(t)
	v := &GreetingValidator{}

	warnings, err := v.ValidateCreate(context.Background(), greetingWith("Hello"))
	g.Expect(warnings).To(BeEmpty())
	g.Expect(err).NotTo(HaveOccurred())

	_, err = v.ValidateCreate(context.Background(), greetingWith("  \t"))
	g.Expect(err).To(MatchError(ErrBlankMessage))

	_, err = v.ValidateUpdate(context.Background(), greetingWith("Hello"), greetingWith("Hi"))
	g.Expect(err).NotTo(HaveOccurred())
	_, err = v.ValidateUpdate(context.Background(), greetingWith("Hello"), greetingWith(" "))
	g.Expect(err).To(MatchError(ErrBlankMessage))

	// An object being deleted is always admitted, so finalizer removal is
	// never blocked.
	deleting := greetingWith(" ")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	_, err = v.ValidateUpdate(context.Background(), greetingWith("Hello"), deleting)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = v.ValidateDelete(context.Background(), greetingWith(""))
	g.Expect(err).NotTo(HaveOccurred())
}

// The same rule, reached through the API server and the served webhook.
var _ = Describe("Greeting webhook", func() {
	It("admits a valid Greeting and rejects a blank message", func() {
		ok := greetingWith("Hello")
		ok.Name = "valid"
		Expect(k8sClient.Create(ctx, ok)).To(Succeed())

		blank := greetingWith("   ")
		blank.Name = "blank"
		err := k8sClient.Create(ctx, blank)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrBlankMessage.Error()))

		ok.Spec.Message = "  "
		err = k8sClient.Update(ctx, ok)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(ErrBlankMessage.Error()))
		Expect(k8sClient.Delete(ctx, ok)).To(Succeed())
	})
})
