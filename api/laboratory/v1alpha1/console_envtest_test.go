package v1alpha1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func consoleEnvtest(t *testing.T) client.Client {
	t.Helper()
	dir := os.Getenv("KUBEBUILDER_ASSETS")
	if dir == "" {
		dirs, _ := filepath.Glob("../../../bin/k8s/*")
		if len(dirs) == 0 {
			dirs, _ = filepath.Glob("../../../../../bin/k8s/*")
		}
		if len(dirs) == 0 {
			t.Skip("no envtest assets")
		}
		dir = dirs[len(dirs)-1]
	}
	env := &envtest.Environment{BinaryAssetsDirectory: dir, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	if err := AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns"}}); err != nil {
		t.Fatal(err)
	}
	return c
}

// The console and serial ports of a device and the console cables of a lab are checked by the API server: names, the shape of a console
// port, the limits, the two ends of a cable, and ports only on a container device.
func TestConsolePortsAndCablesValidation(t *testing.T) {
	c := consoleEnvtest(t)
	ctx := context.Background()
	router := func(ports ...ConsolePortSpec) DeviceTemplate {
		return DeviceTemplate{Name: "r1", Type: DeviceTypeContainer, ConsolePorts: ports}
	}
	pc := func(ports ...SerialPortSpec) DeviceTemplate {
		return DeviceTemplate{Name: "pc1", Type: DeviceTypeContainer, SerialPorts: ports}
	}
	cable := func(sd, sp, cd, cp string) ConsoleCableTemplate {
		return ConsoleCableTemplate{Serial: PortRef{Device: sd, Port: sp}, Console: PortRef{Device: cd, Port: cp}}
	}
	n := 0
	try := func(devices []DeviceTemplate, cables ...ConsoleCableTemplate) error {
		n++
		lab := &Lab{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("lab%d", n), Namespace: "ns"}}
		lab.Spec.Devices, lab.Spec.ConsoleCables = devices, cables
		return c.Create(ctx, lab)
	}
	many := func(k int) []ConsoleCableTemplate {
		out := make([]ConsoleCableTemplate, k)
		for i := range out {
			out[i] = cable("pc1", "serial0", "r1", "console0")
		}
		return out
	}
	serials := func(k int) []SerialPortSpec {
		out := make([]SerialPortSpec, k)
		for i := range out {
			out[i] = SerialPortSpec{Name: fmt.Sprintf("serial%d", i)}
		}
		return out
	}
	for name, tc := range map[string]struct {
		devices []DeviceTemplate
		cables  []ConsoleCableTemplate
		want    string
	}{
		"a full lab": {[]DeviceTemplate{
			router(ConsolePortSpec{Name: "console0", Command: []string{"/usr/bin/vtysh"}}, ConsolePortSpec{Name: "aux0"},
				ConsolePortSpec{Name: "console1", Native: &NativeConsoleSpec{Kind: NativeConsoleTCP, Port: 2001}},
				ConsolePortSpec{Name: "console2", Native: &NativeConsoleSpec{Kind: NativeConsoleStdio}}),
			pc(SerialPortSpec{Name: "serial0"}, SerialPortSpec{Name: "serial1", Path: "/dev/ttyCON1"}),
		}, []ConsoleCableTemplate{cable("pc1", "serial0", "r1", "console0"), cable("pc1", "serial1", "r1", "aux0")}, ""},
		"bad console name":                {[]DeviceTemplate{router(ConsolePortSpec{Name: "tty0"})}, nil, "consolePorts"},
		"console number too long":         {[]DeviceTemplate{router(ConsolePortSpec{Name: "console01"})}, nil, "consolePorts"},
		"bad serial name":                 {[]DeviceTemplate{pc(SerialPortSpec{Name: "com1"})}, nil, "serialPorts"},
		"serial path /dev/tty":            {[]DeviceTemplate{pc(SerialPortSpec{Name: "serial0", Path: "/dev/tty"})}, nil, "path"},
		"serial path /dev/console":        {[]DeviceTemplate{pc(SerialPortSpec{Name: "serial0", Path: "/dev/console"})}, nil, "path"},
		"two console ports with one name": {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0"}, ConsolePortSpec{Name: "console0"})}, nil, "Duplicate value"},
		"command and native":              {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Command: []string{"/bin/sh"}, Native: &NativeConsoleSpec{Kind: NativeConsoleStdio}})}, nil, "not both"},
		"user and native":                 {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", User: "root", Native: &NativeConsoleSpec{Kind: NativeConsoleStdio}})}, nil, "not both"},
		"relative command":                {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Command: []string{"login"}})}, nil, "absolute program path"},
		"command in /proc":                {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Command: []string{"/proc/self/exe"}})}, nil, "absolute program path"},
		"tcp without a port":              {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Native: &NativeConsoleSpec{Kind: NativeConsoleTCP}})}, nil, "tcp needs a port"},
		"stdio with a port":               {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Native: &NativeConsoleSpec{Kind: NativeConsoleStdio, Port: 23}})}, nil, "only a tcp console"},
		"unknown native":                  {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0", Native: &NativeConsoleSpec{Kind: "ssh"}})}, nil, "Unsupported value"},
		"five console ports": {[]DeviceTemplate{router(ConsolePortSpec{Name: "console0"}, ConsolePortSpec{Name: "console1"}, ConsolePortSpec{Name: "console2"},
			ConsolePortSpec{Name: "console3"}, ConsolePortSpec{Name: "aux0"})}, nil, "Too many"},
		"sixteen serial ports":   {[]DeviceTemplate{pc(serials(16)...)}, nil, ""},
		"seventeen serial ports": {[]DeviceTemplate{pc(serials(17)...)}, nil, "Too many"},
		"ports on a switch": {[]DeviceTemplate{{Name: "sw", Type: DeviceTypeUnmanagedSwitch, SerialPorts: []SerialPortSpec{{Name: "serial0"}}}}, nil,
			"only a container device has console and serial ports"},
		"two serial ends":  {nil, []ConsoleCableTemplate{cable("pc1", "serial0", "pc2", "serial0")}, "the console end names a console port"},
		"two console ends": {nil, []ConsoleCableTemplate{cable("r1", "console0", "r2", "console0")}, "the serial end names a serial port"},
		"one device":       {nil, []ConsoleCableTemplate{cable("pc1", "serial0", "pc1", "console0")}, "two different devices"},
		"a NIC as an end":  {nil, []ConsoleCableTemplate{cable("pc1", "eth0", "r1", "console0")}, "consoleCables"},
		"64 cables":        {nil, many(64), ""},
		"65 cables":        {nil, many(65), "Too many"},
	} {
		t.Run(name, func(t *testing.T) {
			err := try(tc.devices, tc.cables...)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("must be accepted: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// The ports of a Device never change after it was made, and a ConsoleCable is immutable: a line is re-plugged by a new object.
func TestDevicePortsAndConsoleCablesAreImmutable(t *testing.T) {
	c := consoleEnvtest(t)
	ctx := context.Background()
	d := &Device{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: "ns"}, Spec: DeviceSpec{LabRef: "l", Name: "r1", Type: DeviceTypeContainer,
		ConsolePorts: []ConsolePortSpec{{Name: "console0"}}}}
	if err := c.Create(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.Spec.ConsolePorts[0].Command = []string{"/bin/sh"}
	if err := c.Update(ctx, d); err == nil || !strings.Contains(err.Error(), "console ports of a device are immutable") {
		t.Fatalf("a changed console port must be refused: %v", err)
	}
	d.Spec.ConsolePorts = nil
	if err := c.Update(ctx, d); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("removing the console ports must be refused: %v", err)
	}
	d.Spec.ConsolePorts = []ConsolePortSpec{{Name: "console0"}}
	d.Spec.SerialPorts = []SerialPortSpec{{Name: "serial0"}}
	if err := c.Update(ctx, d); err == nil || !strings.Contains(err.Error(), "serial ports of a device are immutable") {
		t.Fatalf("adding a serial port must be refused: %v", err)
	}
	d.Spec.SerialPorts = nil
	d.Spec.Image = "nginx"
	if err := c.Update(ctx, d); err != nil {
		t.Fatalf("another field of the device still changes: %v", err)
	}

	cc := &ConsoleCable{ObjectMeta: metav1.ObjectMeta{Name: "l--pc1-serial0", Namespace: "ns"}, Spec: ConsoleCableSpec{LabRef: "l",
		Serial: PortRef{Device: "pc1", Port: "serial0"}, Console: PortRef{Device: "r1", Port: "console0"}}}
	if err := c.Create(ctx, cc); err != nil {
		t.Fatal(err)
	}
	cc.Spec.Console.Port = "aux0"
	if err := c.Update(ctx, cc); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("a re-plugged cable must be refused: %v", err)
	}
	var got ConsoleCable
	if err := c.Get(ctx, client.ObjectKeyFromObject(cc), &got); err != nil {
		t.Fatal(err)
	}
	got.Status.LineUp = true
	got.Status.Console = ConsoleCableEndStatus{NodeName: "n1", Attached: true}
	if err := c.Status().Update(ctx, &got); err != nil {
		t.Fatalf("the status is written apart from the spec: %v", err)
	}
}

func TestSerialPortDevicePathAndDefaultCommand(t *testing.T) {
	if p := (SerialPortSpec{Name: "serial12"}).DevicePath(); p != "/dev/ttyCON12" {
		t.Fatalf("default path: %q", p)
	}
	if p := (SerialPortSpec{Name: "serial0", Path: "/dev/ttyUSB0"}).DevicePath(); p != "/dev/ttyUSB0" {
		t.Fatalf("declared path: %q", p)
	}
	if c := (ConsolePortSpec{Name: "console0"}).EffectiveCommand(); len(c) != 1 || c[0] != "/bin/login" {
		t.Fatalf("default command: %v", c)
	}
	if c := (ConsolePortSpec{Name: "console0", Native: &NativeConsoleSpec{Kind: NativeConsoleStdio}}).EffectiveCommand(); c != nil {
		t.Fatalf("a native port runs no command: %v", c)
	}
	spec := LabSpec{Devices: []DeviceTemplate{{Name: "a"}}}
	if spec.UsesConsoleCables() {
		t.Fatal("no ports, no cables")
	}
	spec.Devices[0].SerialPorts = []SerialPortSpec{{Name: "serial0"}}
	if !spec.UsesConsoleCables() {
		t.Fatal("a port uses the feature")
	}
}
