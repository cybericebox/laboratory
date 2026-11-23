package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cybericebox/laboratory/internal/names"
	"github.com/cybericebox/laboratory/test/utils"
)

// Console cables on kind (docs/internal-docs/laboratory/specs/console-ports-design.md, phases 6 and 7): two busybox devices, a router r1
// with console0 (the default /bin/login) and aux0 (a shell), and a PC pc1 with serial0 and serial1 cabled to them. A program in pc1 writes
// to /dev/ttyCON0 and reads the login prompt of r1; it runs a command on aux0 through /dev/ttyCON1 and reads its output. First with both
// devices on one node (the device access service copies the bytes in process), then with pc1 moved to the other node (the cable stream
// between the two device access pods), and after a restart of a device access pod. The devices have no interface at all: the line never
// uses the lab network.

const (
	busyboxSource = "busybox:1.37"
	busyboxImage  = utils.KindRegistryHost + "/busybox:1.37"
)

var _ = Describe(
	"Console cables", Ordered, func() {
		SetDefaultEventuallyTimeout(3 * time.Minute)
		SetDefaultEventuallyPollingInterval(time.Second)
		const group, lab = "e2e-cables", "cables"
		var ns string
		var nodes []string

		BeforeAll(
			func() {
				By("building and loading the node and lab images")
				_, err := utils.MiseRun([]string{"NODE_IMG=" + nodeImage, "LAB_IMG=" + labImage}, "image:build", "node", "lab")
				Expect(err).NotTo(HaveOccurred())
				Expect(utils.LoadImageToKindClusterWithName(nodeImage)).To(Succeed())
				for image, repo := range map[string]string{nodeImage: "cybericebox/laboratory-node", labImage: "cybericebox/laboratory-lab", busyboxSource: "busybox"} {
					_, err = utils.PublishToKindRegistry(image, repo)
					Expect(err).NotTo(HaveOccurred(), "publish %s", image)
				}
				Eventually(func() string { return kubectlJSONPath("ns", namespace, "", "{.status.phase}") }).ShouldNot(Equal("Terminating"))

				// The FIFO directory of the device access service is made on the nodes beforehand (DEPLOY.md, "Device console").
				for _, n := range labNodes() {
					_, err = utils.Run(exec.Command("docker", "exec", n.name, "mkdir", "-p", "/run/containerd/fifo"))
					Expect(err).NotTo(HaveOccurred())
				}

				By("installing the chart with console cables on")
				repository, tag, _ := strings.Cut(projectImage, ":")
				_, err = utils.Run(exec.Command("helm", "upgrade", "--install", reservationRelease, "charts/laboratory",
					"--namespace", namespace, "--create-namespace", "-f", "hack/kind/e2e-values.yaml",
					"--set", "registry.gate.address="+utils.GateAddress(),
					"--set", "operator.image.repository="+repository, "--set", "operator.image.tag="+tag,
					// kind runs no Cilium: the cable stream is authenticated on its own (HMAC, time, nonce)
					"--set", "consoleCables.enabled=true", "--set", "terminal.networkPolicy.enabled=false",
					"--wait", "--timeout", "5m"))
				Expect(err).NotTo(HaveOccurred(), "Failed to install the chart")
				Eventually(func() string {
					return kubectlJSONPath("ds", "laboratory-device-access", "laboratory-device-access", "{.status.numberReady}")
				}).Should(Equal("2"))
				for _, n := range labNodes() {
					nodes = append(nodes, n.name)
				}
				Expect(nodes).To(HaveLen(2))
			},
		)

		AfterAll(
			func() {
				for _, n := range nodes {
					_, _ = utils.Run(exec.Command("kubectl", "uncordon", n))
				}
				deleteAll("labgroups")
				waitNone("labgroups")
				_, _ = utils.Run(exec.Command("helm", "uninstall", reservationRelease, "--namespace", namespace))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", "charts/laboratory/crds", "--ignore-not-found"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--ignore-not-found", "--wait=false"))
				Eventually(func() string { return kubectlJSONPath("ns", namespace, "", "{.metadata.name}") }).Should(BeEmpty())
				for _, node := range labNodes() {
					_, _ = utils.Run(exec.Command("docker", "exec", node.name, "rm", "-f", "/etc/cni/net.d/00-cybericebox.conflist"))
				}
			},
		)

		AfterEach(
			func() {
				if CurrentSpecReport().Failed() {
					for _, args := range [][]string{
						{"get", "labs,devices,consolecables", "-A", "-o", "yaml"},
						{"get", "pods", "-A", "-o", "wide"},
						{"get", "nodes", "-o", "yaml"},
						{"-n", "laboratory-device-access", "logs", "daemonset/laboratory-device-access", "--tail=200", "--prefix", "--all-pods"},
						{"-n", namespace, "logs", "daemonset/laboratory-node-agent", "--all-containers", "--tail=200", "--prefix", "--all-pods"},
						{"-n", namespace, "logs", "deployment/laboratory-controller-manager", "--tail=200"},
					} {
						out, err := utils.Run(exec.Command("kubectl", args...))
						_, _ = fmt.Fprintf(GinkgoWriter, "kubectl %s:\n%s\n(error: %v)\n", strings.Join(args, " "), out, err)
					}
				}
			},
		)

		// state is lineUp/serial node/console node/console port of the cable of a serial port of pc1.
		state := func(serial string) []string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "consolecables", "-n", ns, "-o", "jsonpath="+fmt.Sprintf(
				`{range .items[?(@.spec.serial.port=="%s")]}{.status.lineUp}/{.status.serial.nodeName}/{.status.console.nodeName}/{.spec.console.port}{end}`, serial)))
			return strings.Split(strings.TrimSpace(out), "/")
		}
		lineUp := func(serial, console string) func() bool {
			return func() bool {
				s := state(serial)
				return len(s) == 4 && s[0] == "true" && s[3] == console
			}
		}
		nodesOf := func(serial string) (string, string) {
			s := state(serial)
			Expect(s).To(HaveLen(4))
			return s[1], s[2]
		}
		pcPod := func() string {
			out, _ := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns, "-l", names.LabelDevice+"=pc1",
				"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}"))
			return strings.TrimSpace(out)
		}
		// onPC runs a shell script in the pc1 device: what a participant does at its terminal.
		onPC := func(script string) string {
			out, _ := utils.Run(exec.Command("kubectl", "exec", "-n", ns, pcPod(), "-c", "pc1", "--", "/bin/sh", "-c", script))
			return out
		}
		talk := func() {
			By("reading the login prompt of r1 from /dev/ttyCON0")
			Eventually(func() string {
				return onPC(`exec 3<>/dev/ttyCON0; printf '\r' >&3; timeout 3 cat <&3 || true`)
			}, 60*time.Second, 2*time.Second).Should(ContainSubstring("login:"))
			By("running a command on r1's aux0 shell through /dev/ttyCON1")
			Eventually(func() string {
				return onPC(`exec 3<>/dev/ttyCON1; printf 'echo CIB-$((6*7))\r' >&3; timeout 3 cat <&3 || true`)
			}, 60*time.Second, 2*time.Second).Should(ContainSubstring("CIB-42"))
		}

		It(
			"joins two devices on one node", func() {
				By("keeping the devices on " + nodes[0])
				_, err := utils.Run(exec.Command("kubectl", "cordon", nodes[1]))
				Expect(err).NotTo(HaveOccurred())
				apply(labGroupManifest(group, ""))
				ns = groupNamespace(group)
				apply(cablesLab(ns, lab))
				Eventually(labPhase(ns, lab)).Should(Equal("Ready"))
				Eventually(lineUp("serial0", "console0")).Should(BeTrue())
				Eventually(lineUp("serial1", "aux0")).Should(BeTrue())
				serialNode, consoleNode := nodesOf("serial0")
				Expect(serialNode).To(Equal(consoleNode))
				talk()
				Eventually(func() string { return kubectlJSONPath("lab", lab, ns, "{.status.consoleCables[*].lineUp}") }).Should(Equal("true true"))

				By("checking that nothing listens on a network in the devices: the lines are unix sockets of the keeper only")
				for _, dev := range []string{"r1", "pc1"} {
					pod, _ := utils.Run(exec.Command("kubectl", "get", "pods", "-n", ns, "-l", names.LabelDevice+"="+dev, "-o", "jsonpath={.items[0].metadata.name}"))
					out, err := utils.Run(exec.Command("kubectl", "exec", "-n", ns, strings.TrimSpace(pod), "-c", dev, "--", "netstat", "-ln"))
					Expect(err).NotTo(HaveOccurred())
					for _, line := range utils.GetNonEmptyLines(out) {
						Expect(line).NotTo(MatchRegexp(`^(tcp|udp)`), "a network listener in %s: %s", dev, line)
						if strings.HasPrefix(line, "unix") && strings.Contains(line, "LISTEN") {
							Expect(line).To(ContainSubstring("/.cib/ports/"), "a unix listener in %s: %s", dev, line)
						}
					}
				}
			},
		)

		It(
			"joins them across two nodes when one moves", func() {
				By("moving pc1 to " + nodes[1])
				_, err := utils.Run(exec.Command("kubectl", "uncordon", nodes[1]))
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "cordon", nodes[0]))
				Expect(err).NotTo(HaveOccurred())
				_, err = utils.Run(exec.Command("kubectl", "delete", "pod", "-n", ns, pcPod(), "--wait=false"))
				Expect(err).NotTo(HaveOccurred())
				Eventually(func() bool {
					s, c := nodesOf("serial0")
					return s == nodes[1] && c == nodes[0] && lineUp("serial0", "console0")()
				}).Should(BeTrue())
				Eventually(lineUp("serial1", "aux0")).Should(BeTrue())
				talk()
			},
		)

		It(
			"comes back after a restart of a device access pod", func() {
				_, consoleNode := nodesOf("serial0")
				out, err := utils.Run(exec.Command("kubectl", "delete", "pod", "-n", "laboratory-device-access", "-l", "app=laboratory-device-access",
					"--field-selector", "spec.nodeName="+consoleNode))
				Expect(err).NotTo(HaveOccurred(), out)
				Eventually(func() string {
					return kubectlJSONPath("ds", "laboratory-device-access", "laboratory-device-access", "{.status.numberReady}")
				}).Should(Equal("2"))
				Eventually(lineUp("serial0", "console0")).Should(BeTrue())
				talk()
			},
		)
	},
)

// cablesLab is a lab of two devices without interfaces, joined by two console cables.
func cablesLab(namespace, name string) string {
	return fmt.Sprintf(
		`apiVersion: laboratory.cybericebox.com/v1alpha1
kind: Lab
metadata:
  name: %[1]s
  namespace: %[2]s
  labels:
    %[3]s: %[4]s
spec:
  devices:
    - name: r1
      type: container
      image: %[5]s
      service: {command: [/bin/sleep, "86400"], user: root}
      consolePorts:
        - {name: console0}
        - {name: aux0, command: [/bin/sh, -i]}
    - name: pc1
      type: container
      image: %[5]s
      service: {command: [/bin/sleep, "86400"], user: root}
      serialPorts:
        - {name: serial0}
        - {name: serial1}
  consoleCables:
    - {serial: {device: pc1, port: serial0}, console: {device: r1, port: console0}}
    - {serial: {device: pc1, port: serial1}, console: {device: r1, port: aux0}}
`, name, namespace, names.LabelTenant, e2eTenant, busyboxImage)
}
