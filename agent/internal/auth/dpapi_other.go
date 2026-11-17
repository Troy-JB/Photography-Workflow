//go:build !windows

package auth

// Non-Windows builds are developer/CI targets. The secret file is written
// 0600, which is the strongest protection this platform offers without a
// dedicated keychain dependency; release builds are Windows-only.
func protect(data, _ []byte) ([]byte, error) {
	return data, nil
}

func unprotect(data, _ []byte) ([]byte, error) {
	return data, nil
}
