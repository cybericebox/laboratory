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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	laboratoryv1alpha1 "github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
)

var _ = Describe(
	"Device Controller container workload", func() {
		ctx := context.Background()

		It("keeps a suspended LabGroup device deployment at zero replicas", func() {
			const groupName = "suspend-devices"
			namespace := laboratoryv1alpha1.LabGroupNamespace(groupName)
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{names.LabelGroup: groupName}}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, ns) })

			group := &laboratoryv1alpha1.LabGroup{ObjectMeta: metav1.ObjectMeta{Name: groupName}}
			Expect(k8sClient.Create(ctx, group)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, group) })

			dev := &laboratoryv1alpha1.Device{
				ObjectMeta: metav1.ObjectMeta{Name: "lab-suspend-web", Namespace: namespace},
				Spec: laboratoryv1alpha1.DeviceSpec{
					Type:   laboratoryv1alpha1.DeviceTypeContainer,
					Name:   "web",
					LabRef: "lab-suspend",
					Image:  "nginx:alpine",
				},
			}
			Expect(k8sClient.Create(ctx, dev)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, dev) })

			r := &DeviceReconciler{Client: k8sClient, Reader: standaloneRunningLabReader{k8sClient}, Scheme: k8sClient.Scheme()}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: dev.Name, Namespace: dev.Namespace}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: group.Name}, group)).To(Succeed())
			group.Spec.Suspended = true
			Expect(k8sClient.Update(ctx, group)).To(Succeed())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			var dep appsv1.Deployment
			Expect(k8sClient.Get(ctx, req.NamespacedName, &dep)).To(Succeed())
			Expect(*dep.Spec.Replicas).To(Equal(int32(0)))
		})

		It(
			"creates a Guaranteed Deployment with lab co-location affinity and drops the legacy PDB", func() {
				dev := &laboratoryv1alpha1.Device{
					ObjectMeta: metav1.ObjectMeta{Name: "lab1-web", Namespace: "default"},
					Spec: laboratoryv1alpha1.DeviceSpec{
						Type:   laboratoryv1alpha1.DeviceTypeContainer,
						Name:   "web",
						LabRef: "lab1",
						Image:  "nginx:alpine",
					},
				}
				Expect(k8sClient.Create(ctx, dev)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, dev) })

				r := &DeviceReconciler{Client: k8sClient, Reader: standaloneRunningLabReader{k8sClient}, Scheme: k8sClient.Scheme(), Defaults: DeviceDefaults{CPU: "250m", Memory: "256Mi"}}
				_, err := r.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: "lab1-web", Namespace: "default"},
				})
				Expect(err).NotTo(HaveOccurred())

				var dep appsv1.Deployment
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "lab1-web", Namespace: "default"}, &dep)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, &dep) })
				aff := dep.Spec.Template.Spec.Affinity
				Expect(aff).NotTo(BeNil())
				Expect(aff.PodAffinity).NotTo(BeNil())
				terms := aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution
				Expect(terms).To(HaveLen(2))
				Expect(terms[0].PodAffinityTerm.TopologyKey).To(Equal(names.TopologyKeyHostname))
				Expect(terms[0].PodAffinityTerm.LabelSelector.MatchLabels).To(HaveKeyWithValue(names.LabelLab, "lab1"))
				// B-11: then the group's own node (every pod of the namespace), softer than the lab's, and never required.
				Expect(terms[1].Weight).To(BeNumerically("<", terms[0].Weight))
				Expect(terms[1].PodAffinityTerm.TopologyKey).To(Equal(names.TopologyKeyHostname))
				Expect(terms[1].PodAffinityTerm.LabelSelector).NotTo(BeNil())
				Expect(terms[1].PodAffinityTerm.LabelSelector.MatchLabels).To(BeEmpty())
				Expect(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution).To(BeEmpty())

				// The container is Guaranteed: requests equal limits, defaults applied.
				res := dep.Spec.Template.Spec.Containers[0].Resources
				Expect(res.Requests).To(Equal(res.Limits))
				Expect(res.Requests.Cpu().String()).To(Equal("250m"))
				Expect(res.Requests.Memory().String()).To(Equal("256Mi"))

				// A per-device budget of an older version is removed: the group budget
				// covers the pod, and a pod under two budgets cannot be evicted.
				var current laboratoryv1alpha1.Device
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "lab1-web", Namespace: "default"}, &current)).To(Succeed())
				legacy := &policyv1.PodDisruptionBudget{
					ObjectMeta: metav1.ObjectMeta{Name: "lab1-web", Namespace: "default"},
					Spec: policyv1.PodDisruptionBudgetSpec{
						MinAvailable: ptrIntstr(1),
						Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{names.LabelDevice: "web"}},
					},
				}
				Expect(controllerutil.SetControllerReference(&current, legacy, k8sClient.Scheme())).To(Succeed())
				Expect(k8sClient.Create(ctx, legacy)).To(Succeed())
				_, err = r.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: "lab1-web", Namespace: "default"},
				})
				Expect(err).NotTo(HaveOccurred())
				err = k8sClient.Get(ctx, types.NamespacedName{Name: "lab1-web", Namespace: "default"}, &policyv1.PodDisruptionBudget{})
				Expect(errors.IsNotFound(err)).To(BeTrue(), "legacy per-device PDB must be deleted, got %v", err)
			},
		)
	},
)

var _ = Describe(
	"Device Controller switch readiness", func() {
		ctx := context.Background()

		reconcileOnce := func(name string) {
			r := &DeviceReconciler{Client: k8sClient, Reader: standaloneRunningLabReader{k8sClient}, Scheme: k8sClient.Scheme()}
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
	"Device Controller switch-only lab", func() {
		ctx := context.Background()

		It(
			"makes a switch ready once its VNI is allocated when the lab cables nothing to it", func() {
				lab := &laboratoryv1alpha1.Lab{
					ObjectMeta: metav1.ObjectMeta{Name: "lab-sw-only", Namespace: "default"},
					Spec: laboratoryv1alpha1.LabSpec{
						Devices: []laboratoryv1alpha1.DeviceTemplate{
							{Name: "sw", Type: laboratoryv1alpha1.DeviceTypeUnmanagedSwitch},
						},
					},
				}
				Expect(k8sClient.Create(ctx, lab)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, lab) })
				sw := &laboratoryv1alpha1.Device{
					ObjectMeta: metav1.ObjectMeta{Name: "sw-only", Namespace: "default"},
					Spec: laboratoryv1alpha1.DeviceSpec{
						Type:   laboratoryv1alpha1.DeviceTypeUnmanagedSwitch,
						Name:   "sw",
						LabRef: "lab-sw-only",
					},
				}
				Expect(k8sClient.Create(ctx, sw)).To(Succeed())
				DeferCleanup(func() { _ = k8sClient.Delete(ctx, sw) })

				r := &DeviceReconciler{Client: k8sClient, Reader: standaloneRunningLabReader{k8sClient}, Scheme: k8sClient.Scheme()}
				req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "sw-only", Namespace: "default"}}
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				var cur laboratoryv1alpha1.Device
				Expect(k8sClient.Get(ctx, req.NamespacedName, &cur)).To(Succeed())
				Expect(cur.Status.Ready).To(BeFalse(), "no VNI yet")

				vni := uint(43)
				cur.Status.VNI = &vni
				Expect(k8sClient.Status().Update(ctx, &cur)).To(Succeed())
				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, req.NamespacedName, &cur)).To(Succeed())
				Expect(cur.Status.Ready).To(BeTrue())
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
							Reader: standaloneRunningLabReader{k8sClient}, Client: k8sClient,
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

func ptrIntstr(i int32) *intstr.IntOrString {
	v := intstr.FromInt32(i)
	return &v
}
