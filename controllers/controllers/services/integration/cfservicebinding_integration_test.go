package integration_test

import (
	"context"
	"time"

	. "github.com/onsi/gomega/gstruct"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "code.cloudfoundry.org/cf-k8s-controllers/controllers/apis/services/v1alpha1"
	. "code.cloudfoundry.org/cf-k8s-controllers/controllers/controllers/workloads/testutils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("CFServiceBinding", func() {
	var namespace *corev1.Namespace

	BeforeEach(func() {
		namespace = BuildNamespaceObject(GenerateGUID())
		Expect(
			k8sClient.Create(context.Background(), namespace),
		).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.Background(), namespace)).To(Succeed())
	})

	When("a new CFServiceBinding is Created", func() {
		var (
			secretData        map[string]string
			secret            *corev1.Secret
			cfServiceInstance *servicesv1alpha1.CFServiceInstance
			cfServiceBinding  *servicesv1alpha1.CFServiceBinding
		)
		BeforeEach(func() {
			ctx := context.Background()

			secretData = map[string]string{
				"foo": "bar",
			}
			secret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-name",
					Namespace: namespace.Name,
				},
				StringData: secretData,
			}
			Expect(
				k8sClient.Create(ctx, secret),
			).To(Succeed())

			cfServiceInstance = &servicesv1alpha1.CFServiceInstance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "service-instance-guid",
					Namespace: namespace.Name,
				},
				Spec: servicesv1alpha1.CFServiceInstanceSpec{
					Name:       "service-instance-name",
					SecretName: secret.Name,
					Type:       "user-provided",
					Tags:       []string{},
				},
			}
			Expect(
				k8sClient.Create(ctx, cfServiceInstance),
			).To(Succeed())

			cfServiceBinding = &servicesv1alpha1.CFServiceBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      GenerateGUID(),
					Namespace: namespace.Name,
				},
				Spec: servicesv1alpha1.CFServiceBindingSpec{
					Service: corev1.ObjectReference{
						Kind:       "ServiceInstance",
						Name:       cfServiceInstance.Name,
						APIVersion: "services.cloudfoundry.org/v1alpha1",
					},
					AppRef: corev1.LocalObjectReference{
						Name: "",
					},
				},
			}
		})

		JustBeforeEach(func() {
			Expect(
				k8sClient.Create(context.Background(), cfServiceBinding),
			).To(Succeed())
		})

		When("and the secret exists", func() {
			It("eventually resolves the secretName and updates the CFServiceBinding status", func() {
				Eventually(func() servicesv1alpha1.CFServiceBindingStatus {
					updatedCFServiceBinding := new(servicesv1alpha1.CFServiceBinding)
					Expect(
						k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cfServiceBinding), updatedCFServiceBinding),
					).To(Succeed())

					return updatedCFServiceBinding.Status
				}).Should(MatchFields(IgnoreExtras, Fields{
					"Binding": MatchFields(IgnoreExtras, Fields{"Name": Equal(secret.Name)}),
					"Conditions": ContainElement(MatchFields(IgnoreExtras, Fields{
						"Type":    Equal("BindingSecretAvailable"),
						"Status":  Equal(metav1.ConditionTrue),
						"Reason":  Equal("SecretFound"),
						"Message": Equal(""),
					})),
				}))
			})
		})

		When("and the referenced secret does not exist", func() {
			var otherSecret *corev1.Secret

			BeforeEach(func() {
				ctx := context.Background()
				otherSecret = &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-secret-name",
						Namespace: namespace.Name,
					},
				}
				instance := &servicesv1alpha1.CFServiceInstance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "other-service-instance-guid",
						Namespace: namespace.Name,
					},
					Spec: servicesv1alpha1.CFServiceInstanceSpec{
						Name:       "other-service-instance-name",
						SecretName: otherSecret.Name,
						Type:       "user-provided",
						Tags:       []string{},
					},
				}
				Expect(
					k8sClient.Create(ctx, instance),
				).To(Succeed())

				cfServiceBinding.Spec.Service.Name = instance.Name
			})

			It("updates the CFServiceBinding status", func() {
				Eventually(func() servicesv1alpha1.CFServiceBindingStatus {
					updatedCFServiceBinding := new(servicesv1alpha1.CFServiceBinding)
					Expect(
						k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cfServiceBinding), updatedCFServiceBinding),
					).To(Succeed())

					return updatedCFServiceBinding.Status
				}).Should(MatchFields(IgnoreExtras, Fields{
					"Binding": MatchFields(IgnoreExtras, Fields{"Name": Equal("")}),
					"Conditions": ContainElement(MatchFields(IgnoreExtras, Fields{
						"Type":    Equal("BindingSecretAvailable"),
						"Status":  Equal(metav1.ConditionFalse),
						"Reason":  Equal("SecretNotFound"),
						"Message": Equal("Binding secret does not exist"),
					})),
				}))
			})

			When("the referenced secret is created afterwards", func() {
				JustBeforeEach(func() {
					time.Sleep(100 * time.Millisecond)
					Expect(
						k8sClient.Create(context.Background(), otherSecret),
					).To(Succeed())
				})

				It("eventually resolves the secretName and updates the CFServiceBinding status", func() {
					Eventually(func() servicesv1alpha1.CFServiceBindingStatus {
						updatedCFServiceBinding := new(servicesv1alpha1.CFServiceBinding)
						Expect(
							k8sClient.Get(context.Background(), client.ObjectKeyFromObject(cfServiceBinding), updatedCFServiceBinding),
						).To(Succeed())

						return updatedCFServiceBinding.Status
					}).Should(MatchFields(IgnoreExtras, Fields{
						"Binding": MatchFields(IgnoreExtras, Fields{"Name": Equal(otherSecret.Name)}),
						"Conditions": ContainElement(MatchFields(IgnoreExtras, Fields{
							"Type":    Equal("BindingSecretAvailable"),
							"Status":  Equal(metav1.ConditionTrue),
							"Reason":  Equal("SecretFound"),
							"Message": Equal(""),
						})),
					}))
				})
			})
		})
	})
})
