package meta

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const sampleJSON = `[{
	"SourceFile": "C:/cards/IMG_0001.CR3",
	"File:MIMEType": "image/x-canon-cr3",
	"File:ImageWidth": 6000,
	"File:ImageHeight": 4000,
	"EXIF:Make": "Canon",
	"EXIF:Model": "Canon EOS R5",
	"EXIF:LensModel": "RF24-70mm F2.8 L IS USM",
	"EXIF:DateTimeOriginal": "2026-10-30T12:34:56",
	"EXIF:OffsetTimeOriginal": "+08:00",
	"EXIF:ISO": 100,
	"EXIF:ExposureTime": 0.004,
	"EXIF:FNumber": 2.8,
	"EXIF:FocalLength": 50,
	"EXIF:Orientation": 1,
	"Composite:GPSLatitude": 31.2304,
	"Composite:GPSLongitude": 121.4737
}]`

func TestParseMapsWorkflowFields(t *testing.T) {
	md, err := Parse([]byte(sampleJSON), "IMG_0001.CR3")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if md.MIME != "image/x-canon-cr3" || md.Width != 6000 || md.Height != 4000 {
		t.Fatalf("unexpected basics: %+v", md)
	}
	if md.Camera != "Canon EOS R5" {
		t.Fatalf("camera = %q (make prefix must not repeat)", md.Camera)
	}
	if md.Lens != "RF24-70mm F2.8 L IS USM" {
		t.Fatalf("lens = %q", md.Lens)
	}
	want := time.Date(2026, 10, 30, 12, 34, 56, 0, time.FixedZone("", 8*3600))
	if md.CapturedAt == nil || !md.CapturedAt.Equal(want) {
		t.Fatalf("captured = %v, want %v", md.CapturedAt, want)
	}
	if md.ExifJSON == "" || len(md.ExifJSON) > DigestLimit {
		t.Fatalf("digest size = %d", len(md.ExifJSON))
	}
	var digest map[string]any
	if err := json.Unmarshal([]byte(md.ExifJSON), &digest); err != nil {
		t.Fatalf("digest is not JSON: %v", err)
	}
	if digest["EXIF:ISO"] == nil || digest["EXIF:FNumber"] == nil {
		t.Fatalf("digest lost camera tags: %v", digest)
	}
}

func TestParseFallsBackToExtensionAndImageSize(t *testing.T) {
	doc := `[{"File:FileTypeExtension": "jpg", "Composite:ImageSize": "4032x3024",
		"EXIF:Make": "FUJIFILM", "EXIF:Model": "X-T5",
		"EXIF:DateTimeOriginal": "2026:10:30 08:00:00"}]`
	md, err := Parse([]byte(doc), "DSCF0001.JPG")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if md.MIME != "image/jpeg" || md.Width != 4032 || md.Height != 3024 {
		t.Fatalf("fallbacks failed: %+v", md)
	}
	if md.Camera != "FUJIFILM X-T5" || md.Lens != "" {
		t.Fatalf("camera/lens = %q/%q", md.Camera, md.Lens)
	}
	if md.CapturedAt == nil || !md.CapturedAt.Equal(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("captured = %v", md.CapturedAt)
	}
}

func TestParseColonDateWithOffset(t *testing.T) {
	// ExifTool -n keeps the camera's colon format even for DateTimeOriginal;
	// this is what a real Canon JPEG returns.
	doc := `[{"EXIF:DateTimeOriginal": "2026:10:29 10:15:00",
		"EXIF:OffsetTimeOriginal": "+08:00"}]`
	md, err := Parse([]byte(doc), "IMG_0001.JPG")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := time.Date(2026, 10, 29, 10, 15, 0, 0, time.FixedZone("", 8*3600))
	if md.CapturedAt == nil || !md.CapturedAt.Equal(want) {
		t.Fatalf("captured = %v, want %v", md.CapturedAt, want)
	}
}

func TestDigestCapDropsLargestValues(t *testing.T) {
	huge := strings.Repeat("x", 6000)
	doc := map[string]any{
		"EXIF:Make":             "Canon",
		"EXIF:Model":            "EOS R5",
		"EXIF:DateTimeOriginal": "2026-10-30T12:34:56",
		"EXIF:LensModel":        huge,
	}
	raw, err := json.Marshal([]map[string]any{doc})
	if err != nil {
		t.Fatal(err)
	}
	md, err := Parse(raw, "IMG.CR3")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(md.ExifJSON) > DigestLimit {
		t.Fatalf("digest = %d bytes, limit %d", len(md.ExifJSON), DigestLimit)
	}
	var kept map[string]any
	if err := json.Unmarshal([]byte(md.ExifJSON), &kept); err != nil {
		t.Fatalf("digest invalid: %v", err)
	}
	if kept["EXIF:Make"] != "Canon" || kept["EXIF:Model"] != "EOS R5" {
		t.Fatalf("small tags were dropped: %v", kept)
	}
	if _, ok := kept["EXIF:LensModel"]; ok {
		t.Fatalf("oversized lens tag should be dropped")
	}
}

func TestIsJPEG(t *testing.T) {
	if !IsJPEG([]byte{0xFF, 0xD8, 0x01, 0x02, 0xFF, 0xD9}) {
		t.Fatal("valid JPEG rejected")
	}
	for _, bad := range [][]byte{
		nil,
		{0xFF, 0xD8},
		{0x00, 0xD8, 0x00, 0xD9},
		{0xFF, 0xD8, 0x00, 0x00},
	} {
		if IsJPEG(bad) {
			t.Fatalf("invalid JPEG accepted: %v", bad)
		}
	}
}

type fakeReader struct {
	json    []byte
	binary  []byte
	binErr  error
	binTags []string
}

func (f *fakeReader) JSON(context.Context, string) ([]byte, error) { return f.json, nil }
func (f *fakeReader) Binary(_ context.Context, _ string, tags []string) ([]byte, error) {
	f.binTags = tags
	return f.binary, f.binErr
}
func (f *fakeReader) Close() error { return nil }

func TestEmbeddedPreviewValidatesTail(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xAA, 0xBB, 0xFF, 0xD9}
	r := &fakeReader{binary: jpeg}
	got, err := EmbeddedPreview(context.Background(), r, "IMG.CR3", nil)
	if err != nil || len(got) != len(jpeg) {
		t.Fatalf("preview: %v len=%d", err, len(got))
	}
	if len(r.binTags) != 3 || r.binTags[0] != "JpgFromRaw" {
		t.Fatalf("tags = %v", r.binTags)
	}
	r = &fakeReader{binary: []byte{0x00, 0x01}}
	if _, err := EmbeddedPreview(context.Background(), r, "IMG.CR3", nil); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid tail err = %v", err)
	}
	r = &fakeReader{binErr: ErrNoPreview}
	if _, err := EmbeddedPreview(context.Background(), r, "IMG.CR3", nil); !errors.Is(err, ErrNoPreview) {
		t.Fatalf("no preview err = %v", err)
	}
}

func TestExtractRequiresJSON(t *testing.T) {
	r := &fakeReader{json: []byte(`[]`)}
	if _, err := Extract(context.Background(), r, "IMG.CR3"); err == nil {
		t.Fatal("empty document should fail")
	}
	r = &fakeReader{json: []byte(`{}`)}
	if _, err := Extract(context.Background(), r, "IMG.CR3"); err == nil {
		t.Fatal("object document should fail")
	}
}
