// Package meta extracts camera metadata through ExifTool. The process runs in
// stay-open mode so importing thousands of frames costs one process, not
// thousands. Only a compact digest of the tags the workflow needs is kept.
package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"photography-workflow/agent/internal/model"
)

// Reader is the slice of ExifTool the agent needs. Tests substitute a fake.
type Reader interface {
	// JSON returns the `-json -G -n` document for one file.
	JSON(ctx context.Context, path string) ([]byte, error)
	// Binary returns the first embedded preview tag that carries data.
	Binary(ctx context.Context, path string, tags []string) ([]byte, error)
	Close() error
}

var (
	ErrNoPreview    = errors.New("meta: no embedded preview")
	ErrInvalidImage = errors.New("meta: embedded preview is not a JPEG")
)

// PreviewTags are tried in order for RAW files. Some bodies only write a
// thumbnail, others a full-size preview.
var PreviewTags = []string{"JpgFromRaw", "PreviewImage", "ThumbnailImage"}

// DigestLimit is the maximum size of the exif_json column.
const DigestLimit = 4096

// digestTags is the allow-list persisted with the file. Keeping raw ExifTool
// output out of SQLite avoids multi-megabyte XMP blobs.
var digestTags = []string{
	"EXIF:Make", "EXIF:Model", "EXIF:LensModel", "EXIF:LensID",
	"EXIF:DateTimeOriginal", "EXIF:OffsetTimeOriginal", "EXIF:CreateDate",
	"EXIF:Orientation", "EXIF:ISO", "EXIF:ExposureTime", "EXIF:FNumber",
	"EXIF:FocalLength", "EXIF:FocalLengthIn35mmFormat",
	"Composite:GPSLatitude", "Composite:GPSLongitude",
	"QuickTime:Duration", "RIFF:Duration", "Composite:Duration",
	"QuickTime:CreateDate", "File:MIMEType",
}

// Extract reads ExifTool JSON and maps the tags the workflow stores.
func Extract(ctx context.Context, r Reader, path string) (model.FileMetadata, error) {
	data, err := r.JSON(ctx, path)
	if err != nil {
		return model.FileMetadata{}, err
	}
	info, err := Parse(data, path)
	if err != nil {
		return model.FileMetadata{}, err
	}
	return info, nil
}

// Parse turns an ExifTool JSON document into the persisted metadata.
func Parse(data []byte, path string) (model.FileMetadata, error) {
	var docs []map[string]any
	if err := json.Unmarshal(data, &docs); err != nil {
		return model.FileMetadata{}, fmt.Errorf("meta: parse exiftool json: %w", err)
	}
	if len(docs) == 0 {
		return model.FileMetadata{}, errors.New("meta: exiftool returned no documents")
	}
	raw := docs[0]

	md := model.FileMetadata{
		MIME: firstString(raw, "File:MIMEType", "File:FileTypeExtension"),
		Width: firstInt(raw, "File:ImageWidth", "EXIF:ExifImageWidth", "EXIF:ImageWidth",
			"QuickTime:ImageWidth", "PNG:ImageWidth", "RIFF:ImageWidth"),
		Height: firstInt(raw, "File:ImageHeight", "EXIF:ExifImageHeight", "EXIF:ImageHeight",
			"QuickTime:ImageHeight", "PNG:ImageHeight", "RIFF:ImageHeight"),
		Camera: cameraName(raw),
		Lens:   firstString(raw, "EXIF:LensModel", "EXIF:LensID", "Composite:LensID", "EXIF:Lens"),
	}
	if md.MIME != "" && !strings.Contains(md.MIME, "/") {
		// Some builds report the extension rather than the media type.
		md.MIME = ""
	}
	if md.MIME == "" {
		md.MIME = mimeForPath(path)
	}
	if md.Width == 0 || md.Height == 0 {
		w, h := parseImageSize(firstString(raw, "Composite:ImageSize", "File:ImageSize"))
		if md.Width == 0 {
			md.Width = w
		}
		if md.Height == 0 {
			md.Height = h
		}
	}
	md.CapturedAt = captureTime(raw)
	md.ExifJSON = digest(raw, DigestLimit)
	return md, nil
}

// EmbeddedPreview returns the first embedded JPEG that passes a SOI/EOI check.
func EmbeddedPreview(ctx context.Context, r Reader, path string, tags []string) ([]byte, error) {
	if len(tags) == 0 {
		tags = PreviewTags
	}
	data, err := r.Binary(ctx, path, tags)
	if err != nil {
		return nil, err
	}
	if !IsJPEG(data) {
		return nil, ErrInvalidImage
	}
	return data, nil
}

// IsJPEG reports whether data starts with SOI and ends with EOI.
func IsJPEG(data []byte) bool {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return false
	}
	for i := len(data) - 2; i >= 0; i-- {
		if data[i] == 0xFF && data[i+1] == 0xD9 {
			return true
		}
	}
	return false
}

func cameraName(raw map[string]any) string {
	makeName := strings.TrimSpace(firstString(raw, "EXIF:Make"))
	modelName := strings.TrimSpace(firstString(raw, "EXIF:Model"))
	switch {
	case makeName == "":
		return modelName
	case modelName == "":
		return makeName
	case strings.HasPrefix(strings.ToLower(modelName), strings.ToLower(makeName)):
		return modelName
	default:
		return makeName + " " + modelName
	}
}

func captureTime(raw map[string]any) *time.Time {
	value := firstString(raw, "EXIF:DateTimeOriginal", "EXIF:CreateDate",
		"QuickTime:CreateDate", "XMP:CreateDate")
	if value == "" {
		return nil
	}
	offset := firstString(raw, "EXIF:OffsetTimeOriginal", "EXIF:OffsetTime", "QuickTime:TimeZone")
	if offset != "" && !hasZone(value) {
		value += normalizeOffset(offset)
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05",
		// With -n ExifTool keeps the camera's colon format, so the offset is
		// appended in that form too.
		"2006:01:02 15:04:05-07:00",
		"2006:01:02 15:04:05Z07:00",
		"2006:01:02 15:04:05",
	} {
		if t, err := time.Parse(layout, value); err == nil {
			return &t
		}
	}
	return nil
}

func hasZone(value string) bool {
	if strings.HasSuffix(value, "Z") {
		return true
	}
	if n := len(value); n >= 6 {
		tail := value[n-6:]
		if (tail[0] == '+' || tail[0] == '-') && tail[3] == ':' && digits(tail[1:3]) && digits(tail[4:]) {
			return true
		}
	}
	if n := len(value); n >= 5 {
		tail := value[n-5:]
		if (tail[0] == '+' || tail[0] == '-') && digits(tail[1:]) {
			return true
		}
	}
	return false
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func normalizeOffset(offset string) string {
	offset = strings.TrimSpace(offset)
	if offset == "" {
		return ""
	}
	if offset[0] != '+' && offset[0] != '-' {
		offset = "+" + offset
	}
	if len(offset) == 5 { // +0800 -> +08:00
		offset = offset[:3] + ":" + offset[3:]
	}
	return offset
}

func parseImageSize(value string) (int, int) {
	w, h, ok := strings.Cut(strings.ToLower(value), "x")
	if !ok {
		return 0, 0
	}
	return atoi(w), atoi(h)
}

func atoi(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			if s := valueString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func valueString(v any) string {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%v", value))
	case json.Number:
		return value.String()
	}
	return ""
}

func firstInt(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			switch value := v.(type) {
			case float64:
				return int(value)
			case json.Number:
				if n, err := value.Int64(); err == nil {
					return int(n)
				}
			case string:
				if n := atoi(value); n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

// digest keeps the allow-listed tags, dropping the largest values until the
// document fits SQLite's budget. The result is deterministic for a given map.
func digest(raw map[string]any, limit int) string {
	m := make(map[string]any, len(digestTags))
	for _, key := range digestTags {
		if v, ok := raw[key]; ok && v != nil {
			m[key] = v
		}
	}
	for len(m) > 0 {
		data, err := json.Marshal(m)
		if err != nil {
			return "{}"
		}
		if len(data) <= limit {
			return string(data)
		}
		drop := ""
		dropSize := -1
		for key, value := range m {
			encoded, _ := json.Marshal(value)
			size := len(encoded)
			if size > dropSize || (size == dropSize && key < drop) {
				drop, dropSize = key, size
			}
		}
		delete(m, drop)
	}
	return "{}"
}

func mimeForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".heic", ".heif":
		return "image/heic"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".cr2":
		return "image/x-canon-cr2"
	case ".cr3":
		return "image/x-canon-cr3"
	case ".nef":
		return "image/x-nikon-nef"
	case ".arw":
		return "image/x-sony-arw"
	case ".dng":
		return "image/x-adobe-dng"
	case ".raf":
		return "image/x-fuji-raf"
	case ".orf":
		return "image/x-olympus-orf"
	case ".rw2":
		return "image/x-panasonic-rw2"
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".mts", ".m2ts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	}
	return ""
}
