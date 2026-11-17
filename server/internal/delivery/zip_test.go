package delivery

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

func TestWriteArchiveWithManifest(t *testing.T) {
	first := []byte("first original bytes")
	second := []byte("second original bytes")
	entries := []ZipEntry{
		BytesEntry("PWF-001_0001_a.cr3", first),
		BytesEntry("PWF-001_0002_b.cr3", second),
	}
	var buf bytes.Buffer
	res, err := Write(context.Background(), &buf, entries, func(results []ZipEntryResult) ([]byte, error) {
		manifest := Manifest{
			SchemaVersion: ManifestSchema,
			DeliveryID:    uuid.MustParse("33333333-3333-3333-3333-333333333333"),
			ProjectCode:   "PWF-001",
			Spec:          SpecOriginal,
			BuildVersion:  2,
			GeneratedAt:   time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
			Warnings:      []string{"web_spec_unsupported_mime"},
		}
		for i, r := range results {
			manifest.Items = append(manifest.Items, ManifestItem{
				Entry: r.Name, AssetID: uuid.New(), OriginalName: r.Name,
				Source: "original", SHA256: r.SHA256, Size: r.Size, Warning: []string{"", "skip"}[i],
			})
		}
		return manifest.JSON()
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.EntryCount != 3 {
		t.Fatalf("entry count = %d, want 3 (2 files + manifest)", res.EntryCount)
	}
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(res.Entries))
	}
	sum := sha256.Sum256(buf.Bytes())
	if res.SHA256 != hex.EncodeToString(sum[:]) || res.Size != int64(buf.Len()) {
		t.Fatalf("zip sha/size = %s/%d, want %s/%d", res.SHA256, res.Size, hex.EncodeToString(sum[:]), buf.Len())
	}

	reader, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	if len(reader.File) != 3 {
		t.Fatalf("zip entries = %d, want 3", len(reader.File))
	}
	byName := map[string]*zip.File{}
	for _, f := range reader.File {
		byName[f.Name] = f
		if f.Method != zip.Store {
			t.Errorf("%s method = %d, want Store", f.Name, f.Method)
		}
		if f.Modified.UTC().Format("2006-01-02 15:04:05") != "1980-01-01 00:00:00" {
			t.Errorf("%s modified = %s, want the fixed epoch", f.Name, f.Modified.UTC())
		}
	}
	assertZipContent(t, byName["PWF-001_0001_a.cr3"], first)
	assertZipContent(t, byName["PWF-001_0002_b.cr3"], second)

	mf, ok := byName[ManifestName]
	if !ok {
		t.Fatal("manifest missing from archive")
	}
	body := readZip(t, mf)
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Spec != SpecOriginal || manifest.BuildVersion != 2 || len(manifest.Items) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Items[1].Warning != "skip" {
		t.Fatalf("manifest warning = %q", manifest.Items[1].Warning)
	}
	if manifest.Warnings[0] != "web_spec_unsupported_mime" {
		t.Fatalf("manifest warnings = %v", manifest.Warnings)
	}
}

func TestWriteRejectsChecksumMismatch(t *testing.T) {
	entry := BytesEntry("a.cr3", []byte("payload"))
	entry.ExpectedSHA256 = strings.Repeat("00", 32)
	if _, err := Write(context.Background(), io.Discard, []ZipEntry{entry}, nil); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("sha mismatch err = %v", err)
	}
	sizeMismatch := BytesEntry("a.cr3", []byte("payload"))
	sizeMismatch.ExpectedSize = 999
	_, err := Write(context.Background(), io.Discard, []ZipEntry{sizeMismatch}, nil)
	if !errors.Is(err, ErrChecksumMismatch) || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size mismatch err = %v", err)
	}
}

func TestWriteRejectsInvalidEntries(t *testing.T) {
	if _, err := Write(context.Background(), io.Discard, nil, nil); err == nil {
		t.Fatal("empty archive must fail")
	}
	for _, name := range []string{"../escape.jpg", "dir/file.jpg", `dir\file.jpg`, "  "} {
		if _, err := Write(context.Background(), io.Discard, []ZipEntry{BytesEntry(name, []byte("x"))}, nil); err == nil {
			t.Errorf("entry %q must be rejected", name)
		}
	}
	dup := []ZipEntry{BytesEntry("a.jpg", []byte("1")), BytesEntry("a.jpg", []byte("2"))}
	if _, err := Write(context.Background(), io.Discard, dup, nil); err == nil {
		t.Fatal("duplicate entry names must fail")
	}
	noSource := []ZipEntry{{Name: "a.jpg"}}
	if _, err := Write(context.Background(), io.Discard, noSource, nil); err == nil {
		t.Fatal("entry without Open must fail")
	}
}

func TestWritePropagatesManifestError(t *testing.T) {
	boom := errors.New("boom")
	_, err := Write(context.Background(), io.Discard, []ZipEntry{BytesEntry("a.jpg", []byte("1"))}, func([]ZipEntryResult) ([]byte, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("manifest error = %v", err)
	}
}

func TestEntryNameSanitises(t *testing.T) {
	cases := []struct {
		code     string
		position int
		original string
		ext      string
		want     string
	}{
		{"PWF-001", 3, `C:\photos\IMG_0001.CR3`, ".cr3", "PWF-001_0003_IMG_0001.cr3"},
		{"", 1, "a/b/../../secret.jpg", ".jpg", "item_0001_secret.jpg"},
		{"项目A", 2, "婚礼 001.JPG", ".jpg", "项目A_0002_婚礼_001.jpg"},
		{"PWF", 1, "CON.jpg", ".jpg", "PWF_0001__CON.jpg"},
		{"PWF", 1, "a:b*c?.jpg", ".jpg", "PWF_0001_a_b_c.jpg"},
	}
	for _, tc := range cases {
		got := EntryName(tc.code, tc.position, tc.original, tc.ext)
		if got != tc.want {
			t.Errorf("EntryName(%q,%d,%q,%q) = %q, want %q", tc.code, tc.position, tc.original, tc.ext, got, tc.want)
		}
		if strings.ContainsAny(got, `/\:*?"<>|`) {
			t.Errorf("entry %q still contains path or Windows-reserved characters", got)
		}
	}

	long := EntryName("CODE", 7, strings.Repeat("照", 300)+".jpg", ".jpg")
	if utf8.RuneCountInString(long) > maxEntryRunes {
		t.Fatalf("long entry has %d runes, want <= %d: %q", utf8.RuneCountInString(long), maxEntryRunes, long)
	}
	if !strings.HasSuffix(long, ".jpg") || !strings.HasPrefix(long, "CODE_0007_") {
		t.Fatalf("long entry lost its prefix or extension: %q", long)
	}
}

func TestPackageName(t *testing.T) {
	day := time.Date(2026, 3, 4, 23, 30, 0, 0, time.FixedZone("CST", 8*3600))
	id := uuid.MustParse("abcdef01-2345-6789-abcd-ef0123456789")
	if got := PackageName("PWF-001", SpecOriginal, day, id); got != "PWF-001_original_20260304.zip" {
		t.Fatalf("package name = %q", got)
	}
	if got := PackageName("项目A", SpecWeb, day, id); got != "delivery_abcdef01_web_20260304.zip" {
		t.Fatalf("non-ASCII fallback = %q", got)
	}
	if got := PackageName("PWF 001", "bogus", day, id); got != "PWF_001_web_20260304.zip" {
		t.Fatalf("sanitised/fallback spec = %q", got)
	}
}

func TestObjectKeys(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	deliveryID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	if got := PackageKey(tenant, deliveryID, 2, SpecWeb); got != "t/11111111-1111-1111-1111-111111111111/deliveries/22222222-2222-2222-2222-222222222222/v2/web.zip" {
		t.Fatalf("package key = %q", got)
	}
	if got := WebItemKey(tenant, deliveryID, 3, 7); got != "t/11111111-1111-1111-1111-111111111111/deliveries/22222222-2222-2222-2222-222222222222/v3/web/7.jpg" {
		t.Fatalf("web item key = %q", got)
	}
}

func assertZipContent(t *testing.T, f *zip.File, want []byte) {
	t.Helper()
	body := readZip(t, f)
	if !bytes.Equal(body, want) {
		t.Fatalf("%s content = %q, want %q", f.Name, body, want)
	}
}

func readZip(t *testing.T, f *zip.File) []byte {
	t.Helper()
	if f == nil {
		t.Fatal("zip entry missing")
	}
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("open %s: %v", f.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", f.Name, err)
	}
	return body
}
