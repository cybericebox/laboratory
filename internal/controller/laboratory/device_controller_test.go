/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package laboratory

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
)

var _ = Describe(
	"Device Controller switch readiness", func() {
		ctx := context.Background()

		reconcileOnce := func(name string) {
			r := &DeviceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
		}
		readyOf := func(name string) bool {
			var d laboratoryv1alpha1.Device
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &d)).To(Succeed())
			return d.Status.Ready
		}

		It(
			"keeps a switch not-ready until its VNI and connection are up", func() {
				sw := &laboratoryv1alpha1.Device{
					ObjectMeta: metav1.ObjectMeta{Name: "sw-a", Namespace: "default"},
					Spec: laboratoryv1alpha1.DeviceSpec{
						Type:   laboratoryv1alpha1.DeviceTypeUnmanagedSwitch,
						Name:   "sw",
						LabRef: "lab-a",
					},
				}
				Expect(k8sClient.Create(ctx, sw)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, sw) })

				// No VNI, no connection → not ready.
				reconcileOnce("sw-a")
				Expect(readyOf("sw-a")).To(BeFalse())

				// VNI allocated but still no connection → not ready.
				var cur laboratoryv1alpha1.Device
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sw-a", Namespace: "default"}, &cur)).To(Succeed())
				vni := uint(42)
				cur.Status.VNI = &vni
				Expect(k8sClient.Status().Update(ctx, &cur)).To(Succeed())
				reconcileOnce("sw-a")
				Expect(readyOf("sw-a")).To(BeFalse())

				// Connection referencing the switch, not yet wired → not ready.
				conn := &laboratoryv1alpha1.Connection{
					ObjectMeta: metav1.ObjectMeta{Name: "conn-a", Namespace: "default"},
					Spec: laboratoryv1alpha1.ConnectionSpec{
						LabRef: "lab-a",
						Endpoints: []laboratoryv1alpha1.EndpointSpec{
							{Device: "sw"},
							{Device: "host", Interface: "eth0"},
						},
					},
				}
				Expect(k8sClient.Create(ctx, conn)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, conn) })
				reconcileOnce("sw-a")
				Expect(readyOf("sw-a")).To(BeFalse())

				// Connection wired → switch ready.
				var c laboratoryv1alpha1.Connection
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "conn-a", Namespace: "default"}, &c)).To(Succeed())
				c.Status.Ready = true
				Expect(k8sClient.Status().Update(ctx, &c)).To(Succeed())
				reconcileOnce("sw-a")
				Expect(readyOf("sw-a")).To(BeTrue())
			},
		)
	},
)

var _ = Describe(
	"Device Controller", func() {
		Context(
			"When reconciling a resource", func() {
				const resourceName = "test-resource"

				ctx := context.Background()

				typeNamespacedName := types.NamespacedName{
					Name:      resourceName,
					Namespace: "default", // TODO(user):Modify as needed
				}
				device := &laboratoryv1alpha1.Device{}

				BeforeEach(
					func() {
						By("creating the custom resource for the Kind Device")
						err := k8sClient.Get(ctx, typeNamespacedName, device)
						if err != nil && errors.IsNotFound(err) {
							resource := &laboratoryv1alpha1.Device{
								ObjectMeta: metav1.ObjectMeta{
									Name:      resourceName,
									Namespace: "default",
								},
								Spec: laboratoryv1alpha1.DeviceSpec{
									Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch,
								},
							}
							Expect(k8sClient.Create(ctx, resource)).To(Succeed())
						}
					},
				)

				AfterEach(
					func() {
						// TODO(user): Cleanup logic after each test, like removing the resource instance.
						resource := &laboratoryv1alpha1.Device{}
						err := k8sClient.Get(ctx, typeNamespacedName, resource)
						Expect(err).NotTo(HaveOccurred())

						By("Cleanup the specific resource instance Device")
						Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
					},
				)
				It(
					"should successfully reconcile the resource", func() {
						By("Reconciling the created resource")
						controllerReconciler := &DeviceReconciler{
							Client: k8sClient,
							Scheme: k8sClient.Scheme(),
						}

						_, err := controllerReconciler.Reconcile(
							ctx, reconcile.Request{
								NamespacedName: typeNamespacedName,
							},
						)
						Expect(err).NotTo(HaveOccurred())
						// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
						// Example: If you expect a certain status condition after reconciliation, verify it here.
					},
				)
			},
		)
	},
)
