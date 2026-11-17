//go:build windows

package updater

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	wintrust                = windows.NewLazySystemDLL("wintrust.dll")
	crypt32                 = windows.NewLazySystemDLL("crypt32.dll")
	procWinVerifyTrust      = wintrust.NewProc("WinVerifyTrust")
	procCryptMsgGetParam    = crypt32.NewProc("CryptMsgGetParam")
	procCryptMsgClose       = crypt32.NewProc("CryptMsgClose")
	procCertGetCertProperty = crypt32.NewProc("CertGetCertificateContextProperty")
)

const (
	wtdUINone               = 2
	wtdRevokeNone           = 0
	wtdChoiceFile           = 1
	wtdStateActionVerify    = 1
	wtdStateActionClose     = 2
	wtdProvRevokeNone       = 0x10
	wtdProvCacheOnly        = 0x1000
	wtdSafer                = 0x100
	certSha1HashPropID      = 3
	cmsgSignerCertInfoParam = 7
	certFindSubjectCert     = 11
	certQueryObjectFile     = 1
	certQueryContentPKCS7   = 0x00000400 // CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED
	certQueryFormatBinary   = 0x00000002
	x509AsnEncoding         = 0x00000001
	pkcs7AsnEncoding        = 0x00010000
)

type winTrustFileInfo struct {
	cbStruct       uint32
	pcwszFilePath  *uint16
	hFile          windows.Handle
	pgKnownSubject *windows.GUID
}

type winTrustData struct {
	cbStruct            uint32
	pPolicyCallbackData uintptr
	pSIPClientData      uintptr
	dwUIChoice          uint32
	fdwRevocationChecks uint32
	dwUnionChoice       uint32
	pFile               *winTrustFileInfo
	dwStateAction       uint32
	hWVTStateData       windows.Handle
	pwszURLReference    *uint16
	dwProvFlags         uint32
	dwUIContext         uint32
	pSignatureSettings  uintptr
}

// actionGenericVerifyV2 is WINTRUST_ACTION_GENERIC_VERIFY_V2.
var actionGenericVerifyV2 = windows.GUID{
	Data1: 0x00AAC56B, Data2: 0xCD44, Data3: 0x11d0,
	Data4: [8]byte{0x8c, 0xc2, 0x00, 0xc0, 0x4f, 0xc2, 0x95, 0xee},
}

// VerifyAuthenticode checks that path carries a valid Authenticode signature
// from a trusted publisher. When pinnedThumbprint is not empty the signing
// certificate's SHA-1 fingerprint must match it (hex, colons ignored).
func VerifyAuthenticode(path, pinnedThumbprint string) error {
	pinned := normalizeThumbprint(pinnedThumbprint)
	if pinned == "" {
		return errors.New("未配置签名者指纹，拒绝安装 stable 更新")
	}
	if err := winVerifyTrust(path); err != nil {
		return err
	}
	thumb, err := SignerThumbprint(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(thumb, pinned) {
		return fmt.Errorf("安装包签名者指纹不匹配：得到 %s", thumb)
	}
	return nil
}

// SignerThumbprint returns the SHA-1 fingerprint (upper-case hex) of the
// certificate that signed path.
func SignerThumbprint(path string) (string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	var hStore, hMsg windows.Handle
	if err := windows.CryptQueryObject(
		certQueryObjectFile, unsafe.Pointer(pathPtr),
		certQueryContentPKCS7, certQueryFormatBinary, 0,
		nil, nil, nil, &hStore, &hMsg, nil,
	); err != nil {
		return "", fmt.Errorf("读取安装包的内嵌签名失败（目录签名的文件不支持指纹固定）：%w", err)
	}
	defer windows.CertCloseStore(hStore, 0)
	defer procCryptMsgClose.Call(uintptr(hMsg))

	var infoLen uint32
	ok, _, e := procCryptMsgGetParam.Call(uintptr(hMsg), cmsgSignerCertInfoParam, 0, 0, uintptr(unsafe.Pointer(&infoLen)))
	if ok == 0 || infoLen == 0 {
		return "", fmt.Errorf("读取签名证书信息失败：%w", e)
	}
	info := make([]byte, infoLen)
	ok, _, e = procCryptMsgGetParam.Call(uintptr(hMsg), cmsgSignerCertInfoParam, 0,
		uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&infoLen)))
	if ok == 0 {
		return "", fmt.Errorf("读取签名证书信息失败：%w", e)
	}
	cert, err := windows.CertFindCertificateInStore(
		hStore, x509AsnEncoding|pkcs7AsnEncoding, 0, certFindSubjectCert,
		unsafe.Pointer(&info[0]), nil,
	)
	if err != nil {
		return "", fmt.Errorf("未找到签名证书：%w", err)
	}
	defer windows.CertFreeCertificateContext(cert)

	var size uint32
	ok, _, e = procCertGetCertProperty.Call(uintptr(unsafe.Pointer(cert)), certSha1HashPropID, 0, uintptr(unsafe.Pointer(&size)))
	if ok == 0 || size == 0 {
		return "", fmt.Errorf("读取证书指纹失败：%w", e)
	}
	buf := make([]byte, size)
	ok, _, e = procCertGetCertProperty.Call(uintptr(unsafe.Pointer(cert)), certSha1HashPropID,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return "", fmt.Errorf("读取证书指纹失败：%w", e)
	}
	buf = buf[:size]
	const hexDigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(buf)*2)
	for _, b := range buf {
		out = append(out, hexDigits[b>>4], hexDigits[b&0xf])
	}
	return string(out), nil
}

func winVerifyTrust(path string) error {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	fileInfo := winTrustFileInfo{pcwszFilePath: pathPtr}
	fileInfo.cbStruct = uint32(unsafe.Sizeof(fileInfo))
	data := winTrustData{
		dwUIChoice:          wtdUINone,
		fdwRevocationChecks: wtdRevokeNone,
		dwUnionChoice:       wtdChoiceFile,
		pFile:               &fileInfo,
		dwStateAction:       wtdStateActionVerify,
		dwProvFlags:         wtdProvRevokeNone | wtdProvCacheOnly | wtdSafer,
	}
	data.cbStruct = uint32(unsafe.Sizeof(data))
	status, _, _ := procWinVerifyTrust.Call(0, uintptr(unsafe.Pointer(&actionGenericVerifyV2)), uintptr(unsafe.Pointer(&data)))
	// Close the state even when verification failed.
	data.dwStateAction = wtdStateActionClose
	procWinVerifyTrust.Call(0, uintptr(unsafe.Pointer(&actionGenericVerifyV2)), uintptr(unsafe.Pointer(&data)))
	if status != 0 {
		return fmt.Errorf("安装包没有有效的代码签名（代码 0x%08X）", uint32(status))
	}
	return nil
}
