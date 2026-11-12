package names

import "testing"

func TestTheConsoleHostIsAReservedName(t *testing.T) {
	if err := ValidateNewDeviceName("terminal"); err == nil {
		t.Fatal("a device may be named like the console's default host")
	}
	if err := ValidateNewDeviceName("shell"); err != nil {
		t.Fatalf("an ordinary name is refused: %v", err)
	}
	ReserveHost("shell") // terminal.host=shell
	if err := ValidateNewDeviceName("shell"); err == nil {
		t.Fatal("a device may be named like the configured console host")
	}
	reservedHostsMu.Lock()
	delete(reservedHosts, "shell")
	reservedHostsMu.Unlock()
}
