//go:build !windows

package api

import (
	"os"
	"strings"

	"svyazest-agent/internal/store"
)

func platformInfo() (model, osVersion, machineID string) {
	model = readTrim("/proc/device-tree/model")
	if model == "" {
		vendor := readTrim("/sys/class/dmi/id/sys_vendor")
		prod := readTrim("/sys/class/dmi/id/product_name")
		switch {
		case prod != "" && vendor != "" && !strings.Contains(strings.ToLower(prod), strings.ToLower(vendor)) &&
			!junkDMI(vendor):
			model = vendor + " " + prod
		case prod != "":
			model = prod
		}
		if junkDMI(model) {
			model = ""
		}
	}
	if model == "" {
		model = "Linux-компьютер"
	}
	if store.InDocker() {
		model += " (Docker)"
	}
	osVersion = osRelease() + " " + archLabel()
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if v := readTrim(p); len(v) >= 16 {
			machineID = v
			break
		}
	}
	return
}

func junkDMI(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return l == "" || strings.Contains(l, "to be filled") || l == "default string" || l == "system product name" || l == "none"
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Trim(string(b), "\x00\n "))
}

// osRelease: "Debian 13", "Ubuntu 24.04", "Raspbian 12", "Alpine 3.20".
func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		b, _ = os.ReadFile("/usr/lib/os-release")
	}
	kv := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	name := kv["NAME"]
	name = strings.TrimSuffix(name, " GNU/Linux")
	name = strings.TrimSuffix(name, " Linux")
	if name == "" {
		name = "Linux"
	}
	ver := kv["VERSION_ID"]
	if ver == "" {
		return name
	}
	return name + " " + ver
}
