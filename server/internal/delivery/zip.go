package delivery

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Package specs. Each delivery produces one ZIP per spec.
const (
	SpecWeb      = "web"
	SpecOriginal = "original"
)

const (
	ManifestName    = "manifest.json"
	ManifestSchema  = 1
	maxEntryRunes   = 120
	maxCodeRunes    = 48
	maxStemRunes    = 80
	zipTimestampStr = "1980-01-01T00:00:00Z"
)

// ErrChecksumMismatch reports a source whose bytes no longer match the hash
// recorded when the delivery item was snapshotted.
var ErrChecksumMismatch = errors.New("entry checksum mismatch")

// zipEpoch keeps archives reproducible: no local timezone, no mtimes of the
// source files leak into the package.
var zipEpoch = func() time.Time {
	t, _ := time.Parse(time.RFC3339, zipTimestampStr)
	return t
}()

// ZipEntry is one file in a package. Open is called exactly once; the reader
// is closed by Write. ExpectedSHA256/ExpectedSize are optional integrity
// checks against the snapshot taken when the item was planned.
type ZipEntry struct {
	Name           string
	Open           func(ctx context.Context) (io.ReadCloser, error)
	ExpectedSHA256 string
	ExpectedSize   int64
}

// ZipEntryResult describes a written file. SHA256 is the hash of the bytes
// copied into the archive, so it can be compared with the manifest.
type ZipEntryResult struct {
	Name   string `json:"entry"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ZipResult summarises the finished archive. Entries excludes the manifest;
// EntryCount includes it.
type ZipResult struct {
	Size       int64
	SHA256     string
	Entries    []ZipEntryResult
	EntryCount int
}

// Write streams entries into w, appending a manifest produced by build when it
// is not nil. Every entry is stored uncompressed so the original bytes stay
// byte-identical, and everything is hashed while copying so a large original
// never has to be buffered in memory.
func Write(ctx context.Context, w io.Writer, entries []ZipEntry, build func([]ZipEntryResult) ([]byte, error)) (*ZipResult, error) {
	if len(entries) == 0 && build == nil {
		return nil, errors.New("zip: no entries")
	}
	hasher := sha256.New()
	cw := &countingWriter{w: w}
	zw := zip.NewWriter(io.MultiWriter(cw, hasher))

	res := &ZipResult{Entries: make([]ZipEntryResult, 0, len(entries))}
	seen := make(map[string]struct{}, len(entries))
	for i := range entries {
		name, err := validEntryName(entries[i].Name, seen)
		if err != nil {
			return nil, err
		}
		if entries[i].Open == nil {
			return nil, fmt.Errorf("zip: entry %q has no source", name)
		}
		result, err := writeEntry(ctx, zw, name, entries[i])
		if err != nil {
			return nil, err
		}
		res.Entries = append(res.Entries, result)
	}
	res.EntryCount = len(res.Entries)

	if build != nil {
		body, err := build(res.Entries)
		if err != nil {
			return nil, fmt.Errorf("zip: build manifest: %w", err)
		}
		_, err = writeEntry(ctx, zw, ManifestName, ZipEntry{
			Name:         ManifestName,
			ExpectedSize: int64(len(body)),
			Open: func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			},
		})
		if err != nil {
			return nil, err
		}
		res.EntryCount++
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("zip: close: %w", err)
	}
	res.Size = cw.n
	res.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	return res, nil
}

func validEntryName(raw string, seen map[string]struct{}) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("zip: invalid entry name %q", raw)
	}
	if _, dup := seen[name]; dup {
		return "", fmt.Errorf("zip: duplicate entry name %q", name)
	}
	seen[name] = struct{}{}
	return name, nil
}

func writeEntry(ctx context.Context, zw *zip.Writer, name string, entry ZipEntry) (ZipEntryResult, error) {
	src, err := entry.Open(ctx)
	if err != nil {
		return ZipEntryResult{}, fmt.Errorf("zip: open entry %q: %w", name, err)
	}
	defer src.Close()

	hdr := &zip.FileHeader{Name: name, Method: zip.Store, Modified: zipEpoch}
	hdr.SetMode(0o644)
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return ZipEntryResult{}, fmt.Errorf("zip: create entry %q: %w", name, err)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return ZipEntryResult{}, fmt.Errorf("zip: write entry %q: %w", name, err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if entry.ExpectedSHA256 != "" && !strings.EqualFold(entry.ExpectedSHA256, sum) {
		return ZipEntryResult{}, fmt.Errorf("%w: %s", ErrChecksumMismatch, name)
	}
	if entry.ExpectedSize > 0 && n != entry.ExpectedSize {
		return ZipEntryResult{}, fmt.Errorf("%w: %s (size %d, expected %d)", ErrChecksumMismatch, name, n, entry.ExpectedSize)
	}
	return ZipEntryResult{Name: name, Size: n, SHA256: sum}, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// BytesEntry builds an in-memory entry, used for the manifest and tests.
func BytesEntry(name string, data []byte) ZipEntry {
	return ZipEntry{
		Name:         name,
		ExpectedSize: int64(len(data)),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

// EntryName builds the packaged file name for one item:
// <project_code>_<position>_<stem><ext>. The extension is normalised from ext
// (the caller passes ".jpg" for web renders and the original extension for
// originals), and the whole name is bounded so long client names cannot break
// archive tools on Windows.
func EntryName(projectCode string, position int, originalName, ext string) string {
	prefix := sanitizeStem(projectCode)
	if prefix == "" {
		prefix = "item"
	}
	prefix = truncateRunes(prefix, maxCodeRunes)
	if position < 1 {
		position = 1
	}

	stem := sanitizeStem(fileStem(originalName))
	if stem == "" {
		stem = "file"
	}
	stem = truncateRunes(stem, maxStemRunes)

	extension := normalizeExtension(ext)
	if extension == "" {
		extension = normalizeExtension(path.Ext(strings.ReplaceAll(originalName, "\\", "/")))
	}

	build := func(stem string) string {
		return fmt.Sprintf("%s_%04d_%s%s", prefix, position, stem, extension)
	}
	name := build(stem)
	for utf8.RuneCountInString(name) > maxEntryRunes && stem != "" {
		stem = truncateRunes(stem, utf8.RuneCountInString(stem)-1)
		name = build(stem)
	}
	if utf8.RuneCountInString(name) > maxEntryRunes {
		over := utf8.RuneCountInString(name) - maxEntryRunes
		prefix = truncateRunes(prefix, utf8.RuneCountInString(prefix)-over)
		if prefix == "" {
			prefix = "item"
		}
		name = build(stem)
	}
	return name
}

// PackageName is the download file name for a spec:
// <project_code>_<spec>_<YYYYMMDD>.zip. A non-ASCII project code falls back to
// a stable delivery id suffix so HTTP headers never carry raw CJK bytes.
func PackageName(projectCode, spec string, at time.Time, deliveryID uuid.UUID) string {
	code := asciiCode(projectCode)
	if code == "" {
		short := strings.ReplaceAll(deliveryID.String(), "-", "")
		if len(short) > 8 {
			short = short[:8]
		}
		if short == "" {
			short = "unknown"
		}
		code = "delivery_" + short
	}
	return fmt.Sprintf("%s_%s_%s.zip", code, specOrDefault(spec), at.UTC().Format("20060102"))
}

func specOrDefault(spec string) string {
	if strings.EqualFold(strings.TrimSpace(spec), SpecOriginal) {
		return SpecOriginal
	}
	return SpecWeb
}

// PackageKey is the storage key of one built ZIP.
func PackageKey(tenantID, deliveryID uuid.UUID, version int, spec string) string {
	return fmt.Sprintf("t/%s/deliveries/%s/v%d/%s.zip", tenantID, deliveryID, version, specOrDefault(spec))
}

// WebItemKey is the storage key of one per-item web render.
func WebItemKey(tenantID, deliveryID uuid.UUID, version, position int) string {
	return fmt.Sprintf("t/%s/deliveries/%s/v%d/web/%d.jpg", tenantID, deliveryID, version, position)
}

// Manifest is embedded in the ZIP root and copied into
// delivery_packages.manifest for support and audit.
type Manifest struct {
	SchemaVersion int            `json:"schema_version"`
	DeliveryID    uuid.UUID      `json:"delivery_id"`
	ProjectCode   string         `json:"project_code"`
	Spec          string         `json:"spec"`
	BuildVersion  int            `json:"build_version"`
	GeneratedAt   time.Time      `json:"generated_at"`
	Items         []ManifestItem `json:"items"`
	Warnings      []string       `json:"warnings"`
}

// ManifestItem mirrors one packed file. Source is "revision" when the latest
// approved retouch revision was used and "original" otherwise.
type ManifestItem struct {
	Entry        string     `json:"entry"`
	AssetID      uuid.UUID  `json:"asset_id"`
	OriginalName string     `json:"original_name"`
	Source       string     `json:"source"`
	RevisionID   *uuid.UUID `json:"revision_id,omitempty"`
	RevisionNo   int        `json:"revision_no,omitempty"`
	SHA256       string     `json:"sha256"`
	Size         int64      `json:"size"`
	Width        int        `json:"width,omitempty"`
	Height       int        `json:"height,omitempty"`
	Warning      string     `json:"warning,omitempty"`
}

// JSON renders the manifest with a trailing newline so the file is friendly to
// command-line inspection after extraction.
func (m Manifest) JSON() ([]byte, error) {
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// WebFileName is the customer-facing name of one web render. The web package
// always uses .jpg, so any RAW/TIFF/HEIC extension loses its stem only.
func WebFileName(originalName string) string {
	stem := sanitizeStem(fileStem(originalName))
	if stem == "" {
		stem = "file"
	}
	return truncateRunes(stem, maxStemRunes) + ".jpg"
}

func fileStem(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	if ext := path.Ext(name); ext != "" {
		name = strings.TrimSuffix(name, ext)
	}
	switch name {
	case "", ".", "..":
		return ""
	}
	return name
}

func normalizeExtension(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	ext = strings.TrimPrefix(ext, ".")
	var b strings.Builder
	for _, r := range ext {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 || b.Len() > 8 {
		return ""
	}
	return "." + b.String()
}

// sanitizeStem keeps Unicode letters and digits plus "._-" so Chinese project
// and file names survive; everything that could act as a path separator or is
// illegal on Windows becomes "_".
func sanitizeStem(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		valid := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_'
		if !valid {
			r = '_'
		}
		if r == '_' {
			if lastUnderscore {
				continue
			}
			lastUnderscore = true
		} else {
			lastUnderscore = false
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return ""
	}
	if reservedWindowsName(out) {
		out = "_" + out
	}
	return out
}

func reservedWindowsName(stem string) bool {
	base := strings.ToUpper(stem)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		prefix := base[:3]
		if (prefix == "COM" || prefix == "LPT") && base[3] >= '1' && base[3] <= '9' {
			return true
		}
	}
	return false
}

func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

func asciiCode(code string) string {
	clean := sanitizeStem(strings.TrimSpace(code))
	if clean == "" {
		return ""
	}
	for _, r := range clean {
		if r > unicode.MaxASCII {
			return ""
		}
	}
	return truncateRunes(clean, maxCodeRunes)
}
