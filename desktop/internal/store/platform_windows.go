//go:build windows

package store

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformDefaultDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "SvyazEst")
}

// secureDir: only SYSTEM and Administrators (protected DACL, inherited by
// every file inside). DPAPI machine scope alone would let any local user
// decrypt the token, the ACL is what keeps them out.
func secureDir(dir string) {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return
	}
	_ = windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

const (
	cryptprotectUIForbidden  = 0x1
	cryptprotectLocalMachine = 0x4
)

var entropy = []byte("svyazest-agent/v1")

func blob(b []byte) *windows.DataBlob {
	if len(b) == 0 {
		return &windows.DataBlob{}
	}
	return &windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func takeBlob(out *windows.DataBlob) []byte {
	if out.Data == nil {
		return nil
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

var magic = []byte("SEDP1:")

func protect(b []byte) ([]byte, error) {
	var out windows.DataBlob
	name, _ := windows.UTF16PtrFromString("svyazest-agent")
	if err := windows.CryptProtectData(blob(b), name, blob(entropy), 0, nil,
		cryptprotectUIForbidden|cryptprotectLocalMachine, &out); err != nil {
		return nil, err
	}
	return append(append([]byte(nil), magic...), takeBlob(&out)...), nil
}

func unprotect(b []byte) ([]byte, error) {
	if len(b) < len(magic) || string(b[:len(magic)]) != string(magic) {
		return nil, errors.New("not a DPAPI blob")
	}
	b = b[len(magic):]
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(blob(b), nil, blob(entropy), 0, nil, cryptprotectUIForbidden, &out); err != nil {
		return nil, err
	}
	return takeBlob(&out), nil
}

func ProtectionLabel() string { return "DPAPI (компьютер)" }
