package exedev

import "testing"

func TestExeDevSSHAddressRequiresAdvertisedDestination(t *testing.T) {
	for _, destination := range []string{"", " \t "} {
		vm := exeDevVM{VMName: "fixture-vm", SSHDest: destination}
		if address := vm.SSHAddress(); address != (exeDevSSHAddress{}) {
			t.Fatalf("ssh_dest=%q produced unadvertised address %#v", destination, address)
		}
	}
}
