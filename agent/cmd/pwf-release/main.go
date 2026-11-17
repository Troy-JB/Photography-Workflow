// Command pwf-release is the operations tool for agent releases. It creates
// the Ed25519 signing key and signs update manifests; the private key must
// never ship with the agent.
//
// Publish flow (run on Windows for stable releases):
//
//	pwf-release keygen -out release-key.json
//	makensis -DAPP_VERSION=0.5.0 ... agent/build/installer.nsi
//	signtool sign /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 \
//	    /a PWF-Agent-Setup-0.5.0.exe
//	pwf-release sign -key release-key.json -installer PWF-Agent-Setup-0.5.0.exe \
//	    -version 0.5.0 -channel stable -url https://download.example.com/agent/windows/stable/PWF-Agent-Setup-0.5.0.exe \
//	    -min-version 0.4.0 -signer-thumbprint <sha1> -notes-file notes.md -out manifest.json
//	# upload the installer and manifest.json to the download host
//
// A stable manifest is refused unless the installer carries a valid
// Authenticode signature from the pinned publisher.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"photography-workflow/agent/internal/updater"
)

const maxNotesBytes = 8 << 10

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "sign":
		err = cmdSign(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "check-signature":
		err = cmdCheckSignature(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pwf-release — PWF Agent 发布签名工具

用法:
  pwf-release keygen -out release-key.json [-force]
  pwf-release sign   -key release-key.json -installer Setup.exe -version 0.5.0
                     -channel dev|stable -url https://... [-min-version 0.4.0]
                     [-notes "..." | -notes-file notes.md]
                     [-published-at RFC3339] [-signer-thumbprint SHA1]
                     -out manifest.json
  pwf-release verify -key release-key.json -channel dev -in manifest.json
  pwf-release verify -public-key <base64> -channel dev -in manifest.json
  pwf-release check-signature -file PWF-Agent-Setup-0.5.0.exe -thumbprint <sha1>

stable 通道必须提供 -signer-thumbprint，并在 Windows 上校验安装包的
Authenticode 签名；缺少签名或发布者不一致时签名失败（失败关闭）。
`)
}

type keyFile struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "release-key.json", "私钥文件输出路径")
	force := fs.Bool("force", false, "覆盖已存在的密钥文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("缺少 -out")
	}
	if _, err := os.Stat(*out); err == nil && !*force {
		return fmt.Errorf("%s 已存在；密钥轮换需要显式 -force", *out)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	kf := keyFile{
		PublicKey:  base64.StdEncoding.EncodeToString(pub),
		PrivateKey: base64.StdEncoding.EncodeToString(priv),
	}
	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOrDot(*out)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("私钥已写入 %s（请离线保管，绝不能随安装包分发）\n", *out)
	fmt.Printf("内置公钥（构建参数 updater.BuildPublicKey）：\n%s\n", kf.PublicKey)
	return nil
}

func loadKey(path string) (ed25519.PublicKey, ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("读取密钥失败：%w", err)
	}
	var kf keyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, nil, fmt.Errorf("密钥文件格式无效：%w", err)
	}
	privRaw, err := base64.StdEncoding.DecodeString(kf.PrivateKey)
	if err != nil || len(privRaw) != ed25519.PrivateKeySize {
		return nil, nil, errors.New("私钥无效")
	}
	priv := ed25519.PrivateKey(privRaw)
	pub := priv.Public().(ed25519.PublicKey)
	if kf.PublicKey != "" {
		want, err := base64.StdEncoding.DecodeString(kf.PublicKey)
		if err != nil || !ed25519.PublicKey(want).Equal(pub) {
			return nil, nil, errors.New("密钥文件中的公私钥不匹配")
		}
	}
	return pub, priv, nil
}

func cmdSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := fs.String("key", "", "Ed25519 私钥文件")
	installer := fs.String("installer", "", "安装包路径（用于计算 SHA-256 与大小）")
	version := fs.String("version", "", "发布版本，例如 0.5.0")
	channel := fs.String("channel", "", "发布通道：dev 或 stable")
	rawURL := fs.String("url", "", "安装包下载地址（https://）")
	minVersion := fs.String("min-version", "", "强制更新的最低旧版本（可选）")
	notes := fs.String("notes", "", "更新说明（可选）")
	notesFile := fs.String("notes-file", "", "从文件读取更新说明")
	published := fs.String("published-at", "", "发布时间 RFC3339（默认当前时间）")
	thumbprint := fs.String("signer-thumbprint", "", "stable 通道要求：发布证书 SHA-1 指纹")
	out := fs.String("out", "manifest.json", "清单输出路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || *installer == "" || *version == "" || *channel == "" || *rawURL == "" {
		return errors.New("-key/-installer/-version/-channel/-url 均为必填")
	}
	channelName := strings.ToLower(strings.TrimSpace(*channel))
	if channelName != "dev" && channelName != "stable" {
		return fmt.Errorf("未知通道 %q", *channel)
	}
	if err := updater.CheckVersion(*version); err != nil {
		return err
	}
	if err := updater.CheckDownloadURL(*rawURL); err != nil {
		return err
	}
	if *minVersion != "" {
		if err := updater.CheckVersion(*minVersion); err != nil {
			return err
		}
		if updater.Compare(*minVersion, *version) > 0 {
			return errors.New("-min-version 不能高于发布版本")
		}
	}

	info, err := os.Stat(*installer)
	if err != nil {
		return fmt.Errorf("读取安装包失败：%w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("安装包必须是已存在的非空文件")
	}
	body, err := os.ReadFile(*installer)
	if err != nil {
		return fmt.Errorf("读取安装包失败：%w", err)
	}
	sum := sha256.Sum256(body)

	m := updater.Manifest{
		Version:    strings.TrimSpace(*version),
		Channel:    channelName,
		URL:        strings.TrimSpace(*rawURL),
		SHA256:     hex.EncodeToString(sum[:]),
		Size:       info.Size(),
		MinVersion: strings.TrimSpace(*minVersion),
	}
	if *notesFile != "" {
		data, err := os.ReadFile(*notesFile)
		if err != nil {
			return fmt.Errorf("读取更新说明失败：%w", err)
		}
		m.Notes = string(data)
	} else {
		m.Notes = *notes
	}
	if len(m.Notes) > maxNotesBytes {
		return fmt.Errorf("更新说明超过 %d 字节", maxNotesBytes)
	}
	if *published != "" {
		t, err := time.Parse(time.RFC3339, *published)
		if err != nil {
			return fmt.Errorf("-published-at 需要 RFC3339 时间：%w", err)
		}
		m.PublishedAt = t.UTC()
	} else {
		m.PublishedAt = time.Now().UTC().Truncate(time.Second)
	}

	if channelName == "stable" {
		if strings.TrimSpace(*thumbprint) == "" {
			return errors.New("stable 通道必须提供 -signer-thumbprint；未签名的安装包不得发布")
		}
		if err := updater.VerifyAuthenticode(*installer, *thumbprint); err != nil {
			return fmt.Errorf("stable 安装包代码签名校验失败：%w", err)
		}
		got, err := updater.SignerThumbprint(*installer)
		if err != nil {
			return fmt.Errorf("读取签名者指纹失败：%w", err)
		}
		fmt.Printf("Authenticode 校验通过，签名者 %s\n", got)
	} else {
		fmt.Println("dev 通道：安装包未做代码签名，仅可用于内部测试")
	}

	_, priv, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	envelope, err := updater.Sign(priv, m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absOrDot(*out)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, envelope, 0o644); err != nil {
		return err
	}
	fmt.Printf("清单已写入 %s\n  version   %s\n  channel   %s\n  sha256    %s\n  size      %d\n",
		*out, m.Version, m.Channel, m.SHA256, m.Size)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	keyPath := fs.String("key", "", "密钥文件（使用其中公钥）")
	pubB64 := fs.String("public-key", "", "公钥 base64（与 -key 二选一）")
	channel := fs.String("channel", "", "期望通道")
	in := fs.String("in", "", "清单路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" || *channel == "" {
		return errors.New("-in 与 -channel 均为必填")
	}
	var pub ed25519.PublicKey
	switch {
	case *pubB64 != "":
		raw, err := base64.StdEncoding.DecodeString(*pubB64)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return errors.New("公钥无效")
		}
		pub = ed25519.PublicKey(raw)
	case *keyPath != "":
		p, _, err := loadKey(*keyPath)
		if err != nil {
			return err
		}
		pub = p
	default:
		return errors.New("需要 -key 或 -public-key")
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	m, err := updater.ParseEnvelope(data, pub, *channel)
	if err != nil {
		return err
	}
	fmt.Printf("清单有效\n  version   %s\n  channel   %s\n  url       %s\n  sha256    %s\n  size      %d\n  min       %s\n  published %s\n",
		m.Version, m.Channel, m.URL, m.SHA256, m.Size, m.MinVersion, m.PublishedAt.Format(time.RFC3339))
	return nil
}

func cmdCheckSignature(args []string) error {
	fs := flag.NewFlagSet("check-signature", flag.ContinueOnError)
	file := fs.String("file", "", "待校验的可执行文件")
	thumbprint := fs.String("thumbprint", "", "期望的发布证书 SHA-1 指纹")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" || *thumbprint == "" {
		return errors.New("-file 与 -thumbprint 均为必填")
	}
	if err := updater.VerifyAuthenticode(*file, *thumbprint); err != nil {
		return fmt.Errorf("Authenticode 校验失败：%w", err)
	}
	got, err := updater.SignerThumbprint(*file)
	if err != nil {
		return err
	}
	fmt.Printf("签名有效：%s\n签名者 SHA-1：%s\n", filepath.Base(*file), got)
	return nil
}

func absOrDot(p string) string {
	if p == "" {
		return "."
	}
	return p
}
