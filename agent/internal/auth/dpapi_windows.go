//go:build windows

package auth

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect encrypts with DPAPI scoped to the current Windows user. The identity
// is used as additional entropy, so a copied file cannot be decrypted for a
// different account or installation.
func protect(data, entropy []byte) ([]byte, error) {
	in := blob(data)
	ent := blob(entropy)
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, &ent, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyAndFree(out), nil
}

func unprotect(data, entropy []byte) ([]byte, error) {
	in := blob(data)
	ent := blob(entropy)
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, &ent, 0, nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return copyAndFree(out), nil
}

func blob(data []byte) windows.DataBlob {
	b := windows.DataBlob{Size: uint32(len(data))}
	if len(data) > 0 {
		b.Data = &data[0]
	}
	return b
}

func copyAndFree(out windows.DataBlob) []byte {
	if out.Size == 0 || out.Data == nil {
		return nil
	}
	plain := unsafe.Slice(out.Data, out.Size)
	result := make([]byte, len(plain))
	copy(result, plain)
	return result
}
