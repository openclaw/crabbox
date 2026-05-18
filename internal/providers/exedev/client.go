package exedev

import (
	"encoding/json"
	"net"
	"strings"
)

type exeDevListResponse struct {
	VMs []exeDevVM `json:"vms"`
}

type exeDevVM struct {
	VMName        string   `json:"vm_name"`
	NameValue     string   `json:"name"`
	SSHDest       string   `json:"ssh_dest"`
	Status        string   `json:"status"`
	HTTPSURL      string   `json:"https_url"`
	Region        string   `json:"region"`
	RegionDisplay string   `json:"region_display"`
	Tags          []string `json:"tags"`
}

type exeDevSSHAddress struct {
	User string
	Host string
	Port string
}

func (vm exeDevVM) Name() string {
	return blank(strings.TrimSpace(vm.VMName), strings.TrimSpace(vm.NameValue))
}

func (vm exeDevVM) SSHHost() string {
	return vm.SSHAddress().Host
}

func (vm exeDevVM) SSHAddress() exeDevSSHAddress {
	value := strings.TrimSpace(vm.SSHDest)
	if value == "" && vm.Name() != "" {
		return exeDevSSHAddress{Host: vm.Name() + ".exe.xyz"}
	}
	user := ""
	if strings.Contains(value, "@") {
		user, value, _ = strings.Cut(value, "@")
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		_, port, _ := net.SplitHostPort(value)
		return exeDevSSHAddress{User: strings.TrimSpace(user), Host: host, Port: port}
	}
	return exeDevSSHAddress{User: strings.TrimSpace(user), Host: value}
}

func parseExeDevVM(out string) (exeDevVM, error) {
	var direct exeDevVM
	if err := json.Unmarshal([]byte(out), &direct); err == nil && direct.Name() != "" {
		return direct, nil
	}
	var wrapped struct {
		VM exeDevVM `json:"vm"`
	}
	if err := json.Unmarshal([]byte(out), &wrapped); err != nil {
		return exeDevVM{}, err
	}
	return wrapped.VM, nil
}

func exeDevErrorMessage(out string) string {
	var res struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return ""
	}
	return strings.TrimSpace(res.Error)
}
