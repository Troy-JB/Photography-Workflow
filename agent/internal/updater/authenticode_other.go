//go:build !windows

package updater

// VerifyAuthenticode is Windows-only; every other platform refuses stable
// installer gating instead of pretending the file is signed.
func VerifyAuthenticode(path, pinnedThumbprint string) error {
	return ErrAuthenticodeUnsupported
}

// SignerThumbprint is Windows-only.
func SignerThumbprint(path string) (string, error) {
	return "", ErrAuthenticodeUnsupported
}
