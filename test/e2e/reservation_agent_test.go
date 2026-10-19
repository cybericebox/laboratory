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
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/cybericebox/laboratory/api/laboratory/v1alpha1"
	"github.com/cybericebox/laboratory/pkg/agent/client"
	"github.com/cybericebox/laboratory/pkg/agent/protobuf"
	"github.com/cybericebox/laboratory/test/utils"
)

const (
	agentImage     = "cybericebox/laboratory-agent:e2e"
	agentNamespace = "laboratory-agent"
	agentDomain    = "agent.e2e.example.com"
	// agentTenant is the tenant the test calls the agent as: the chart creates it and cert-manager issues its client
	// certificate (agent.tenantCertificates).
	agentTenant = "e2e"
)

var _ = Describe(
	"Reservations through the agent", Ordered, func() {
		SetDefaultEventuallyTimeout(2 * time.Minute)
		SetDefaultEventuallyPollingInterval(time.Second)

		var (
			lm         protobuf.LabManagerClient
			conn       *grpc.ClientConn
			forward    *exec.Cmd
			cancelAll  context.CancelFunc
			ctx        context.Context
			reasonOf   = func(r *protobuf.BatchResult) string { return r.GetResults()[0].GetReason() }
			stateOf    = func(r *protobuf.BatchResult) protobuf.ItemState { return r.GetResults()[0].GetState() }
			labVariant = func() *protobuf.LabVariant {
				raw, err := json.Marshal(map[string]any{"devices": []map[string]any{{"name": "dev", "type": "container", "image": pauseImage,
					"resources": map[string]string{"cpuLimit": "100m", "memoryLimit": "64Mi"}}}})
				Expect(err).NotTo(HaveOccurred())
				return &protobuf.LabVariant{VariantId: "pause", SpecJson: raw}
			}
		)

		BeforeAll(
			func() {
				By("building and loading the node, lab and agent images")
				_, err := utils.Run(exec.Command("make", "docker-build-node", "docker-build-lab", "docker-build-agent",
					fmt.Sprintf("NODE_IMG=%s", nodeImage), fmt.Sprintf("LAB_IMG=%s", labImage), fmt.Sprintf("AGENT_IMG=%s", agentImage)))
				Expect(err).NotTo(HaveOccurred(), "Failed to build the images")
				for _, img := range []string{nodeImage, labImage, agentImage} {
					Expect(utils.LoadImageToKindClusterWithName(img)).To(Succeed(), "Failed to load %s into Kind", img)
				}

				By("waiting for the namespace of an earlier deployment to go")
				Eventually(func() string { return kubectlJSONPath("ns", namespace, "", "{.status.phase}") }).ShouldNot(Equal("Terminating"))

				By("installing the chart with the agent")
				repository, tag, _ := strings.Cut(projectImage, ":")
				agentRepository, agentTag, _ := strings.Cut(agentImage, ":")
				// No --wait: the agent's server certificate is made below, from the agent CA the chart creates. Repeated while the
				// webhook of a cert-manager installed just before does not answer yet (its CA bundle comes a little later).
				install := func() error {
					_, err := utils.Run(exec.Command(
						"helm", "upgrade", "--install", reservationRelease, "charts/laboratory",
						"--namespace", namespace, "--create-namespace",
						"-f", "hack/kind/e2e-values.yaml",
						"--set", "operator.image.repository="+repository, "--set", "operator.image.tag="+tag,
						"--set", fmt.Sprintf("reservations.endingNoticeSeconds=%d", int(endingNotice.Seconds())),
						"--set", "agent.enabled=true", "--set", "agent.replicas=1", "--set", "agent.networkPolicy.enabled=false", "--set", "agent.domain="+agentDomain,
						"--set", "agent.image.repository="+agentRepository, "--set", "agent.image.tag="+agentTag,
						"--set", "agent.tenantCertificates.enabled=true", "--set", "tenants."+agentTenant+".receivesLabErrors=false",
					))
					return err
				}
				Eventually(install, 3*time.Minute, 10*time.Second).Should(Succeed(), "Failed to install the chart")
				apply(fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: laboratory-agent-server
  namespace: %s
spec:
  secretName: laboratory-agent-server-tls
  dnsNames: [%q]
  issuerRef:
    name: laboratory-agent-ca
    kind: Issuer
`, agentNamespace, agentDomain))
				for _, args := range [][]string{
					{"-n", namespace, "rollout", "status", "deployment/laboratory-controller-manager", "--timeout=5m"},
					{"-n", namespace, "rollout", "status", "daemonset/laboratory-node-agent", "--timeout=5m"},
					{"-n", agentNamespace, "rollout", "status", "deployment/laboratory-agent", "--timeout=5m"},
				} {
					_, err := utils.Run(exec.Command("kubectl", args...))
					Expect(err).NotTo(HaveOccurred(), "rollout %v", args)
				}

				By("connecting to the agent over mTLS as tenant " + agentTenant)
				port := freePort()
				forward = exec.Command("kubectl", "-n", agentNamespace, "port-forward", "svc/laboratory-agent", fmt.Sprintf("%d:5454", port))
				Expect(forward.Start()).To(Succeed())
				creds := agentCredentials()
				conn, err = grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(creds))
				Expect(err).NotTo(HaveOccurred())
				lm = protobuf.NewLabManagerClient(conn)
				ctx, cancelAll = context.WithCancel(context.Background())
				Eventually(func() error { _, err := lm.Ping(ctx, &protobuf.Empty{}); return err }, time.Minute).Should(Succeed())
			},
		)

		AfterAll(
			func() {
				if cancelAll != nil {
					cancelAll()
				}
				if conn != nil {
					_ = conn.Close()
				}
				if forward != nil && forward.Process != nil {
					_ = forward.Process.Kill()
				}
				By("deleting what the tests left")
				deleteAll("reservations")
				deleteAll("labgroups")
				waitNone("labgroups")

				By("uninstalling the chart")
				_, _ = utils.Run(exec.Command("kubectl", "-n", agentNamespace, "delete", "certificate", "laboratory-agent-server", "--ignore-not-found"))
				_, _ = utils.Run(exec.Command("helm", "uninstall", reservationRelease, "--namespace", namespace))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", "charts/laboratory/crds", "--ignore-not-found"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, agentNamespace, "--ignore-not-found", "--wait=false"))
				Eventually(func() string { return kubectlJSONPath("ns", namespace, "", "{.metadata.name}") }, 3*time.Minute).Should(BeEmpty())
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
						{"-n", agentNamespace, "logs", "deployment/laboratory-agent", "--tail=200"},
						{"-n", namespace, "logs", "deployment/laboratory-controller-manager", "--tail=300"},
					} {
						out, err := utils.Run(exec.Command("kubectl", args...))
						_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(error: %v)\n", strings.Join(args, " "), out, err)
					}
				}
			},
		)

		reserve := func(id string, window time.Duration, cpu, memory int64) {
			GinkgoHelper()
			now := time.Now()
			res, err := lm.CreateReservations(ctx, &protobuf.CreateReservationsRequest{Items: []*protobuf.ReservationItem{{
				Id: id, FromUnixMs: now.Add(-5 * time.Second).UnixMilli(), ToUnixMs: now.Add(window).UnixMilli(), CpuMillicores: cpu, MemoryBytes: memory}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(stateOf(res)).To(Equal(protobuf.ItemState_ITEM_STATE_CREATED), "%v", res)
		}
		group := func(name, reservation string) {
			GinkgoHelper()
			res, err := lm.CreateLabGroups(ctx, &protobuf.CreateLabGroupsRequest{Items: []*protobuf.LabGroupItem{{Name: name, ReservationId: reservation, VpnDisabled: true}}})
			Expect(err).NotTo(HaveOccurred())
			Expect(stateOf(res)).To(Equal(protobuf.ItemState_ITEM_STATE_CREATED), "%v", res)
			Eventually(func() string { return groupNamespace(name) }).ShouldNot(BeEmpty())
		}
		lab := func(groupName, name, reservation string) *protobuf.BatchResult {
			GinkgoHelper()
			var res *protobuf.BatchResult
			// the group's namespace may still be on its way: that answer is retryable
			Eventually(func() bool {
				var err error
				res, err = lm.CreateLabs(ctx, &protobuf.CreateLabsRequest{Variants: []*protobuf.LabVariant{labVariant()},
					Items: []*protobuf.LabItem{{LabGroup: groupName, Name: name, VariantId: "pause", ReservationId: reservation}}})
				Expect(err).NotTo(HaveOccurred())
				return !res.GetResults()[0].GetRetryable() || res.GetResults()[0].GetReason() == client.ReasonReservationFull
			}, time.Minute).Should(BeTrue())
			return res
		}
		// phases follows the Monitoring stream of the tenant and records the phase of every reservation as it arrives.
		phases := func(sctx context.Context) func(id string) []protobuf.ReservationPhase {
			stream, err := lm.Monitoring(sctx, &protobuf.MonitoringRequest{MinIntervalMs: 200})
			Expect(err).NotTo(HaveOccurred())
			seen := map[string][]protobuf.ReservationPhase{}
			updates := make(chan *protobuf.MonitoringUpdate, 1024)
			go func() {
				defer GinkgoRecover()
				for {
					u, err := stream.Recv()
					if err != nil {
						close(updates)
						return
					}
					updates <- u
				}
			}()
			return func(id string) []protobuf.ReservationPhase {
				for {
					select {
					case u, ok := <-updates:
						if !ok {
							return seen[id]
						}
						for _, r := range u.GetReservations() {
							p := r.GetStatus().GetPhase()
							if l := seen[r.GetId()]; len(l) == 0 || l[len(l)-1] != p {
								seen[r.GetId()] = append(l, p)
							}
						}
					default:
						return seen[id]
					}
				}
			}
		}

		It(
			"refuses work without a reservation and work over the rest of one", func() {
				reserve("e2e-agent-groups", 10*time.Minute, 2000, 2<<30)
				reserve("e2e-agent-small", 10*time.Minute, 150, 1<<30)
				group("e2e-agent-g1", "e2e-agent-groups")

				By("a lab without a reservation id")
				res := lab("e2e-agent-g1", "no-id", "")
				Expect(stateOf(res)).To(Equal(protobuf.ItemState_ITEM_STATE_FAILED))
				Expect(reasonOf(res)).To(Equal(client.ReasonReservationRequired))

				By("two labs of 100m in a reservation of 150m")
				Expect(stateOf(lab("e2e-agent-g1", "fits", "e2e-agent-small"))).To(Equal(protobuf.ItemState_ITEM_STATE_CREATED))
				res = lab("e2e-agent-g1", "over", "e2e-agent-small")
				Expect(stateOf(res)).To(Equal(protobuf.ItemState_ITEM_STATE_FAILED))
				Expect(reasonOf(res)).To(Equal(client.ReasonReservationFull))
				Expect(res.GetResults()[0].GetError()).To(ContainSubstring("not enough room in this reservation"))
			},
		)

		It(
			"streams the ending notice, deletes the work at the end, and an extension calls the end off", func() {
				sctx, cancel := context.WithCancel(ctx)
				defer cancel()
				seen := phases(sctx)
				start := time.Now()
				reserve("e2e-agent-end", reservationWindow, 1000, 2<<30)
				reserve("e2e-agent-ext", reservationWindow, 1000, 2<<30)
				group("e2e-agent-end", "e2e-agent-end")
				group("e2e-agent-ext", "e2e-agent-ext")
				Expect(stateOf(lab("e2e-agent-end", "end-lab", "e2e-agent-end"))).To(Equal(protobuf.ItemState_ITEM_STATE_CREATED))
				Expect(stateOf(lab("e2e-agent-ext", "ext-lab", "e2e-agent-ext"))).To(Equal(protobuf.ItemState_ITEM_STATE_CREATED))

				By("waiting for the lab to start")
				Eventually(func() string {
					list, err := lm.ListLabs(ctx, &protobuf.ListRequest{Items: []*protobuf.ItemRef{{LabGroup: "e2e-agent-end", Name: "end-lab"}}})
					Expect(err).NotTo(HaveOccurred())
					if len(list.GetItems()) == 0 {
						return ""
					}
					Expect(list.GetItems()[0].GetReservationId()).To(Equal("e2e-agent-end"))
					return list.GetItems()[0].GetStatus().GetPhase()
				}, 45*time.Second).Should(Equal("Ready"))

				By("the notice comes through the stream one notice before the end")
				Eventually(func() []protobuf.ReservationPhase { return seen("e2e-agent-ext") }, reservationWindow).Should(
					ContainElement(protobuf.ReservationPhase_RESERVATION_PHASE_ENDING))
				Expect(time.Since(start)).To(BeNumerically("~", reservationWindow-endingNotice, endingNotice/2+clockSlack))

				By("extending one during its notice")
				to := time.Now().Add(endingNotice + 60*time.Second).UnixMilli()
				res, err := lm.UpdateReservations(ctx, &protobuf.UpdateReservationsRequest{Items: []*protobuf.UpdateReservationItem{
					{Id: "e2e-agent-ext", Changes: &protobuf.ReservationChanges{ToUnixMs: &to}}}})
				Expect(err).NotTo(HaveOccurred())
				Expect(stateOf(res)).To(Equal(protobuf.ItemState_ITEM_STATE_UPDATED), "%v", res)
				Eventually(func() protobuf.ReservationPhase {
					p := seen("e2e-agent-ext")
					return p[len(p)-1]
				}, 30*time.Second).Should(Equal(protobuf.ReservationPhase_RESERVATION_PHASE_ACTIVE))

				By("the other ends: its group and lab are deleted")
				Eventually(func() bool { return gone("labgroup", "e2e-agent-end", "") }, reservationWindow).Should(BeTrue())
				Expect(seen("e2e-agent-end")).To(ContainElements(protobuf.ReservationPhase_RESERVATION_PHASE_ENDING, protobuf.ReservationPhase_RESERVATION_PHASE_ENDED))
				Expect(time.Since(start)).To(BeNumerically(">=", reservationWindow-clockSlack), "deleted before the end")
				Expect(gone("labgroup", "e2e-agent-ext", "")).To(BeFalse(), "the extended reservation keeps its work")
				Expect(reservationPhase("e2e-agent-ext")).To(Equal(string(v1alpha1.ReservationPhaseActive)))
			},
		)
	},
)

// freePort is a local TCP port nothing listens on now.
func freePort() int {
	GinkgoHelper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// secretData reads one key of a Secret; an error while it is not there yet.
func secretData(ns, name, key string) ([]byte, error) {
	out := kubectlJSONPath("secret", name, ns, fmt.Sprintf("{.data.%s}", strings.ReplaceAll(key, ".", `\.`)))
	raw, err := base64.StdEncoding.DecodeString(out)
	if err == nil && len(raw) == 0 {
		err = fmt.Errorf("%s/%s has no %s yet", ns, name, key)
	}
	return raw, err
}

// agentCredentials are the mTLS credentials of the test tenant: its client certificate and the agent CA, with the agent's
// domain as the server name (the connection goes through a port-forward).
func agentCredentials() credentials.TransportCredentials {
	GinkgoHelper()
	var cert tls.Certificate
	pool := x509.NewCertPool()
	Eventually(func() error {
		crt, err := secretData(agentNamespace, "laboratory-agent-client-"+agentTenant+"-tls", "tls.crt")
		if err != nil {
			return err
		}
		key, err := secretData(agentNamespace, "laboratory-agent-client-"+agentTenant+"-tls", "tls.key")
		if err != nil {
			return err
		}
		ca, err := secretData(agentNamespace, "laboratory-agent-server-tls", "ca.crt")
		if err != nil {
			return err
		}
		if !pool.AppendCertsFromPEM(ca) {
			return fmt.Errorf("the agent CA does not parse")
		}
		cert, err = tls.X509KeyPair(crt, key)
		return err
	}).Should(Succeed())
	return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: agentDomain, MinVersion: tls.VersionTLS12})
}
