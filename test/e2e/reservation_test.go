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

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/test/utils"
)

const (
	// reservationRelease is the Helm release of the reservation tests (the chart, with the node-agent and OVS, on Kind).
	reservationRelease = "laboratory"

	// endingNotice is the chart value reservations.endingNoticeSeconds of the tests.
	endingNotice = 30 * time.Second

	// reservationWindow is how long the Reservation of the end test lasts.
	reservationWindow = 90 * time.Second

	// clockSlack covers the clock of the test against the clock of the operator and the poll interval.
	clockSlack = 3 * time.Second

	nodeImage = "cybericebox/laboratory-node:e2e"
	labImage  = "cybericebox/laboratory-lab:e2e"

	// pauseImage is on every Kind node already (the pause image of the sandbox), so a device needs no pull.
	pauseImage = "registry.k8s.io/pause:3.10"

	labNodeLabel = "cybericebox.com/lab=true"

	// labCPU is the CPU of a device of the gang test: small, memory is what fills a node there.
	labCPU = "100m"

	// e2eTenant is the tenant of the test objects: the chart creates it.
	e2eTenant = "default"
)

var _ = Describe(
	"Reservations", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(time.Second)

		BeforeAll(
			func() {
				By("building and loading the node and lab images")
				_, err := utils.Run(
					exec.Command(
						"make", "docker-build-node", "docker-build-lab",
						fmt.Sprintf("NODE_IMG=%s", nodeImage), fmt.Sprintf("LAB_IMG=%s", labImage),
					),
				)
				Expect(err).NotTo(HaveOccurred(), "Failed to build the node and lab images")
				for _, img := range []string{nodeImage, labImage} {
					Expect(utils.LoadImageToKindClusterWithName(img)).To(Succeed(), "Failed to load %s into Kind", img)
				}

				By("waiting for the namespace of an earlier deployment to go")
				Eventually(
					func() string { return kubectlJSONPath("ns", namespace, "", "{.status.phase}") },
				).ShouldNot(Equal("Terminating"))

				By("installing the chart")
				repository, tag, _ := strings.Cut(projectImage, ":")
				_, err = utils.Run(
					exec.Command(
						"helm", "upgrade", "--install", reservationRelease, "charts/laboratory",
						"--namespace", namespace, "--create-namespace",
						"-f", "hack/kind/e2e-values.yaml",
						"--set", "operator.image.repository="+repository,
						"--set", "operator.image.tag="+tag,
						"--set", fmt.Sprintf("reservations.endingNoticeSeconds=%d", int(endingNotice.Seconds())),
						"--wait", "--timeout", "5m",
					),
				)
				Expect(err).NotTo(HaveOccurred(), "Failed to install the chart")
			},
		)

		AfterAll(
			func() {
				By("deleting what the tests left")
				deleteAll("reservations")
				deleteAll("labgroups")
				waitNone("labgroups")

				By("uninstalling the chart")
				_, _ = utils.Run(exec.Command("helm", "uninstall", reservationRelease, "--namespace", namespace))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", "charts/laboratory/crds", "--ignore-not-found"))
				// The namespace must be gone before the next test creates it again (Ginkgo runs the containers in any order).
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found", "--wait=false"))
				Eventually(
					func() string { return kubectlJSONPath("ns", namespace, "", "{.metadata.name}") }, 3*time.Minute,
				).Should(BeEmpty())

				// The node-agent leaves its wrapper of the CNI config behind, and the wrapper refuses every new pod
				// of the node until a node-agent runs there again.
				for _, node := range labNodes() {
					_, _ = utils.Run(exec.Command("docker", "exec", node.name, "rm", "-f", "/etc/cni/net.d/00-cybericebox.conflist"))
				}
			},
		)

		AfterEach(
			func() {
				if CurrentSpecReport().Failed() {
					for _, args := range [][]string{
						{"get", "reservations", "-o", "yaml"},
						{"get", "labgroups,labs", "-A", "-o", "yaml"},
						{"get", "pods", "-A", "-o", "wide"},
						{"get", "events", "-A", "--sort-by=.lastTimestamp"},
						{"describe", "nodes", "-l", labNodeLabel},
						{"-n", namespace, "logs", "daemonset/laboratory-node-agent", "--all-containers", "--tail=200"},
						{"-n", namespace, "logs", "deployment/laboratory-controller-manager", "--tail=300"},
					} {
						out, err := utils.Run(exec.Command("kubectl", args...))
						_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(error: %v)\n", strings.Join(args, " "), out, err)
					}
				}
				deleteAll("reservations")
				deleteAll("labgroups")
				waitNone("labgroups")
			},
		)

		Context(
			"the end of a reservation", func() {
				It(
					"gives notice, then deletes the groups and labs of the reservation", func() {
						const reservation, group = "e2e-end", "e2e-end-group"
						now := time.Now().UTC().Truncate(time.Second)
						from, to := now.Add(-5*time.Second), now.Add(reservationWindow)

						By("making a reservation and a group and a lab under it")
						apply(reservationManifest(reservation, from, to, "4", "4Gi"))
						apply(labGroupManifest(group, reservation))
						ns := groupNamespace(group)
						apply(labManifest(ns, "end-lab", reservation, "", "dev", "", ""))

						By("waiting for the lab to start")
						Eventually(labPhase(ns, "end-lab"), 45*time.Second).Should(Equal("Ready"))
						Expect(reservationPhase(reservation)).To(Equal(string(v1alpha1.ReservationPhaseActive)))
						Expect(time.Now()).To(BeTemporally("<", to.Add(-endingNotice)), "the lab started after the notice began")

						By("watching the reservation end")
						var endingAt time.Time
						var noticeAt string
						var violations []string
						Eventually(
							func() string {
								// The objects are read before the phase: a deletion seen first is followed by a phase read
								// that is already past the end (the phase never goes back).
								objectsGone := gone("labgroup", group, "") && gone("lab", "end-lab", ns)
								anyDeleting := deleting("labgroup", group, "") || deleting("lab", "end-lab", ns)
								phase := reservationPhase(reservation)
								// The end is written to the status before the first deletion, so a deletion is only fine
								// once the phase is Ended; and the notice (Ending) comes with everything still running.
								if anyDeleting && phase != string(v1alpha1.ReservationPhaseEnded) {
									violations = append(violations, fmt.Sprintf("deleted in phase %q", phase))
								}
								if phase == string(v1alpha1.ReservationPhaseEnding) && endingAt.IsZero() {
									endingAt = time.Now()
									noticeAt = kubectlJSONPath("reservation", reservation, "", "{.status.noticeAt}")
								}
								if objectsGone {
									return phase
								}
								return "objects remain"
							}, reservationWindow+90*time.Second, 500*time.Millisecond,
						).Should(Equal(string(v1alpha1.ReservationPhaseEnded)))
						endedAt := time.Now()
						Expect(violations).To(BeEmpty())

						By("checking the order: notice, then end, then deletion")
						Expect(endingAt).NotTo(BeZero(), "the Ending phase was never seen")
						Expect(endingAt).To(BeTemporally("~", to.Add(-endingNotice), endingNotice/2+clockSlack))
						Expect(endingAt).To(BeTemporally("<", to.Add(clockSlack)))
						parsed, err := time.Parse(time.RFC3339, noticeAt)
						Expect(err).NotTo(HaveOccurred(), "status.noticeAt was not set with the notice")
						Expect(parsed).To(BeTemporally("~", to.Add(-endingNotice), clockSlack))
						Expect(endedAt).To(BeTemporally(">=", to.Add(-clockSlack)), "deleted before spec.to")
						Expect(endedAt).To(BeTemporally("<=", to.Add(30*time.Second)), "deleted long after spec.to")
						Expect(
							kubectlJSONPath("reservation", reservation, "", `{.status.conditions[?(@.type=="Ending")].status}`),
						).NotTo(BeEmpty())
					},
				)
			},
		)

		Context(
			"admission of a deploy group", func() {
				It(
					"starts the labs of a group together or not at all", func() {
						const group, deployGroup = "e2e-gang-group", "e2e-gang"

						By("sizing the labs from the lab nodes")
						nodes := labNodes()
						Expect(len(nodes)).To(BeNumerically(">=", 2), "the gang test needs two lab nodes")
						// The size of a device is capped in CPU (limits.device) but not in memory (e2e values), so memory decides.
						// Each lab asks 45% of the memory of the smallest lab node: one fits an empty node (the room
						// after the platform and packing reserves is about 76%, less the pods of the platform), two
						// never fit on one. An unreserved lab takes half of a node, which leaves less than one lab there.
						memory := labMemory(nodes, 45)
						holdMemory := labMemory(nodes, 50)

						By("making a group for the labs")
						apply(labGroupManifest(group, ""))
						ns := groupNamespace(group)
						Eventually(groupPodsStarted(group), 90*time.Second).Should(BeTrue())

						By("occupying the room of a lab node with an unreserved lab")
						apply(labManifest(ns, "gang-hold", "", "", "dev-hold", labCPU, holdMemory))
						Eventually(labPhase(ns, "gang-hold")).Should(Equal("Ready"))

						By("making a deploy group of two labs that fit the empty nodes only together")
						apply(labManifest(ns, "gang-a", "", deployGroup, "dev-a", labCPU, memory))
						apply(labManifest(ns, "gang-b", "", deployGroup, "dev-b", labCPU, memory))

						By("checking that neither lab starts while the room is taken")
						for _, lab := range []string{"gang-a", "gang-b"} {
							Eventually(labSchedulingReason(ns, lab), 60*time.Second).Should(Equal(v1alpha1.WaitInsufficient))
						}
						Consistently(
							func() []string { return devicePods(ns) }, 20*time.Second, time.Second,
						).Should(ConsistOf(HavePrefix("dev-hold-")), "a lab of the group started without the other")

						By("freeing the room")
						_, err := utils.Run(exec.Command("kubectl", "delete", "lab", "gang-hold", "-n", ns, "--wait=false"))
						Expect(err).NotTo(HaveOccurred())

						By("waiting for both labs to start")
						for _, lab := range []string{"gang-a", "gang-b"} {
							Eventually(labPhase(ns, lab), 2*time.Minute).Should(Equal("Ready"))
						}
						Eventually(func() []string { return devicePods(ns) }).Should(HaveLen(2), "the held lab is gone, the two labs of the group run")
					},
				)
			},
		)
	},
)

// labNode is a lab node and the memory it can hold.
type labNode struct {
	name     string
	memBytes int64
}

// labNodes lists the lab nodes with their allocatable CPU and memory.
func labNodes() []labNode {
	out, err := utils.Run(exec.Command("kubectl", "get", "nodes", "-l", labNodeLabel, "-o", "json"))
	Expect(err).NotTo(HaveOccurred())
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
			} `json:"status"`
		} `json:"items"`
	}
	Expect(json.Unmarshal([]byte(out), &list)).To(Succeed())
	nodes := make([]labNode, 0, len(list.Items))
	for _, item := range list.Items {
		mem, err := resource.ParseQuantity(item.Status.Allocatable["memory"])
		Expect(err).NotTo(HaveOccurred())
		nodes = append(nodes, labNode{name: item.Metadata.Name, memBytes: mem.Value()})
	}
	return nodes
}

// labMemory is percent of the memory of the smallest lab node, as a device request.
func labMemory(nodes []labNode, percent int64) string {
	smallest := nodes[0].memBytes
	for _, n := range nodes {
		smallest = min(smallest, n.memBytes)
	}
	return fmt.Sprintf("%dMi", smallest*percent/100/(1<<20))
}

func reservationManifest(name string, from, to time.Time, cpu, memory string) string {
	return fmt.Sprintf(
		`apiVersion: laboratory.cybericebox.com/v1alpha1
kind: Reservation
metadata:
  name: %s
spec:
  tenant: %s
  from: "%s"
  to: "%s"
  resources:
    cpu: "%s"
    memory: %s
`, name, e2eTenant, from.Format(time.RFC3339), to.Format(time.RFC3339), cpu, memory,
	)
}

func labGroupManifest(name, reservation string) string {
	label := ""
	if reservation != "" {
		label = fmt.Sprintf("\n    %s: %s", names.LabelReservation, reservation)
	}
	return fmt.Sprintf(
		`apiVersion: laboratory.cybericebox.com/v1alpha1
kind: LabGroup
metadata:
  name: %s
  labels:
    %s: %s%s
spec: {}
`, name, names.LabelTenant, e2eTenant, label,
	)
}

// labManifest is a lab with one container device. cpu and memory empty leave the device its defaults.
func labManifest(namespace, name, reservation, deployGroup, device, cpu, memory string) string {
	var labels strings.Builder
	fmt.Fprintf(&labels, "    %s: %s\n", names.LabelTenant, e2eTenant)
	if reservation != "" {
		fmt.Fprintf(&labels, "    %s: %s\n", names.LabelReservation, reservation)
	}
	if deployGroup != "" {
		fmt.Fprintf(&labels, "    %s: %s\n", names.LabelDeployGroup, deployGroup)
	}
	resources := ""
	if cpu != "" {
		resources = fmt.Sprintf("      resources:\n        cpuRequest: %s\n        memoryRequest: %s\n", cpu, memory)
	}
	return fmt.Sprintf(
		`apiVersion: laboratory.cybericebox.com/v1alpha1
kind: Lab
metadata:
  name: %s
  namespace: %s
  labels:
%sspec:
  devices:
    - name: %s
      type: container
      image: %s
%s`, name, namespace, labels.String(), device, pauseImage, resources,
	)
}

// apply creates the objects of a manifest.
func apply(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "kubectl apply failed for:\n%s", manifest)
}

// kubectlJSONPath is a field of an object, empty when the object or the field is not there.
func kubectlJSONPath(kind, name, namespace, path string) string {
	args := []string{"get", kind, name, "--ignore-not-found", "-o", "jsonpath=" + path}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	out, _ := utils.Run(exec.Command("kubectl", args...))
	return strings.TrimSpace(out)
}

func reservationPhase(name string) string {
	return kubectlJSONPath("reservation", name, "", "{.status.phase}")
}

func labPhase(namespace, name string) func() string {
	return func() string { return kubectlJSONPath("lab", name, namespace, "{.status.phase}") }
}

func labSchedulingReason(namespace, name string) func() string {
	return func() string { return kubectlJSONPath("lab", name, namespace, "{.status.scheduling.reason}") }
}

// groupNamespace waits for the namespace of a LabGroup.
func groupNamespace(group string) string {
	GinkgoHelper()
	var ns string
	Eventually(
		func() string {
			ns = kubectlJSONPath("labgroup", group, "", "{.status.namespace}")
			return ns
		}, 60*time.Second,
	).ShouldNot(BeEmpty(), "LabGroup %s got no namespace", group)
	return ns
}

// groupPodsStarted says that the VPN and gateway pods of a group have started.
func groupPodsStarted(group string) func() bool {
	return func() bool {
		out := kubectlJSONPath("labgroup", group, "", `{range .status.pods[*]}{.state}{"\n"}{end}`)
		states := utils.GetNonEmptyLines(out)
		if len(states) != 2 {
			return false
		}
		for _, s := range states {
			if s != string(v1alpha1.PodStarted) {
				return false
			}
		}
		return true
	}
}

// gone says that the object does not exist.
func gone(kind, name, namespace string) bool {
	args := []string{"get", kind, name, "--ignore-not-found", "-o", "name"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	out, err := utils.Run(exec.Command("kubectl", args...))
	return err == nil && strings.TrimSpace(out) == ""
}

// deleting says that the object is deleted or on its way out.
func deleting(kind, name, namespace string) bool {
	return gone(kind, name, namespace) || kubectlJSONPath(kind, name, namespace, "{.metadata.deletionTimestamp}") != ""
}

// devicePods are the pods of the devices of the labs of a namespace (not the VPN and gateway of its group).
func devicePods(namespace string) []string {
	var pods []string
	for name := range devicePodsOnNodes(namespace) {
		pods = append(pods, name)
	}
	return pods
}

// devicePodsOnNodes maps the device pods of a namespace to their nodes.
func devicePodsOnNodes(namespace string) map[string]string {
	out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-n", namespace, "-o", "json"))
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(out), &list) != nil {
		return nil
	}
	pods := map[string]string{}
	for _, item := range list.Items {
		name := item.Metadata.Name
		if strings.HasPrefix(name, "vpn-") || strings.HasPrefix(name, "gateway-") {
			continue
		}
		pods[name] = item.Spec.NodeName
	}
	return pods
}

// deleteAll deletes every object of a cluster-scoped kind without waiting.
func deleteAll(kind string) {
	_, _ = utils.Run(exec.Command("kubectl", "delete", kind, "--all", "--ignore-not-found", "--wait=false"))
}

// waitNone waits until no object of the kind is left (a LabGroup goes once its labs and pods are gone).
func waitNone(kind string) {
	Eventually(
		func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", kind, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(strings.TrimSpace(out)).To(BeEmpty())
		}, 3*time.Minute,
	).Should(Succeed())
}
