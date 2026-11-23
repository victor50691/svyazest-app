//go:build windows

package api

import (
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func regString(root registry.Key, path, name string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func platformInfo() (model, osVersion, machineID string) {
	const bios = `HARDWARE\DESCRIPTION\System\BIOS`
	vendor := regString(registry.LOCAL_MACHINE, bios, "SystemManufacturer")
	prod := regString(registry.LOCAL_MACHINE, bios, "SystemProductName")
	switch {
	case prod != "" && vendor != "" && !strings.Contains(strings.ToLower(prod), strings.ToLower(vendor)) &&
		!strings.Contains(strings.ToLower(vendor), "to be filled"):
		model = vendor + " " + prod
	case prod != "":
		model = prod
	default:
		model = "Windows-компьютер"
	}
	const cv = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	product := regString(registry.LOCAL_MACHINE, cv, "ProductName") // "Windows 10 Pro" even on 11
	build, _ := strconv.Atoi(regString(registry.LOCAL_MACHINE, cv, "CurrentBuildNumber"))
	disp := regString(registry.LOCAL_MACHINE, cv, "DisplayVersion")
	if disp == "" {
		disp = regString(registry.LOCAL_MACHINE, cv, "ReleaseId")
	}
	name := "Windows"
	switch {
	case build >= 22000:
		name = "Windows 11"
	case build >= 10240:
		name = "Windows 10"
	case product != "":
		name = product
	}
	if strings.Contains(product, "Server") {
		name = strings.Replace(product, "Windows Server", "Windows Server", 1)
	}
	osVersion = strings.TrimSpace(name + " " + disp + " " + archLabel())
	machineID = regString(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, "MachineGuid")
	return
}
