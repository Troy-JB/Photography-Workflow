package binder

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/settings"
)

type capture struct {
	mu     sync.Mutex
	events []model.Event
	names  []string
}

func (c *capture) emit(name string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
	if e, ok := data.(model.Event); ok {
		c.events = append(c.events, e)
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitIdle(t *testing.T, app *App) Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := app.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		if !snap.Running {
			return snap
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("import did not finish within 15s")
	return Snapshot{}
}

func newTestApp(t *testing.T) (*App, *capture) {
	t.Helper()
	dir := t.TempDir()
	cap := &capture{}
	app := New(filepath.Join(dir, "state.db"), filepath.Join(dir, "settings.json"), cap.emit)
	app.Startup(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		app.Shutdown(ctx)
	})
	if h := app.Health(); !h.OK {
		t.Fatalf("app not healthy: %+v", h)
	}
	return app, cap
}

func TestImportLifecycleReportsLocalComplete(t *testing.T) {
	app, cap := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD01")
	destA := filepath.Join(base, "A")
	destB := filepath.Join(base, "B")
	for _, d := range []string{destA, destB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "DCIM", "100CANON", "IMG_0001.CR3"), []byte("raw-bytes"))
	writeFile(t, filepath.Join(src, "DCIM", "100CANON", "IMG_0001.JPG"), []byte("jpeg-bytes"))
	writeFile(t, filepath.Join(src, "MISC", "notes.txt"), []byte("notes"))

	res, err := app.StartImport(StartRequest{
		Source: src, ProjectCode: "WK-2026", ShootDate: "2026-10-30",
		DestA: destA, DestB: destB, RequireBackup: true,
	})
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if res.Added != 3 || res.Existing {
		t.Fatalf("unexpected start result: %+v", res)
	}

	snap := waitIdle(t, app)
	if snap.Import == nil || snap.Import.Status != model.ImportLocalComplete {
		t.Fatalf("import not local_complete: %+v", snap.Import)
	}
	if snap.Counts.Total != 3 || snap.Counts.LocalVerified != 3 || snap.Counts.Failed != 0 {
		t.Fatalf("unexpected counts: %+v", snap.Counts)
	}

	layout := filepath.Join("WK-2026", "2026-10-30", "CARD01")
	gotA, err := os.ReadFile(filepath.Join(destA, layout, "DCIM", "100CANON", "IMG_0001.CR3"))
	if err != nil {
		t.Fatalf("read copy A: %v", err)
	}
	if string(gotA) != "raw-bytes" {
		t.Fatalf("copy A content = %q", gotA)
	}
	gotB, err := os.ReadFile(filepath.Join(destB, layout, "MISC", "notes.txt"))
	if err != nil {
		t.Fatalf("read copy B: %v", err)
	}
	if string(gotB) != "notes" {
		t.Fatalf("copy B content = %q", gotB)
	}

	detail, err := app.ImportDetail(res.Import.ID, 100)
	if err != nil {
		t.Fatalf("ImportDetail: %v", err)
	}
	if len(detail.Files) != 3 || detail.Total != 3 {
		t.Fatalf("detail files = %d total = %d", len(detail.Files), detail.Total)
	}
	for _, row := range detail.Files {
		if len(row.Copies) != 2 {
			t.Fatalf("%s has %d copies, want 2", row.File.RelPath, len(row.Copies))
		}
		if row.File.State != model.StateLocalVerified {
			t.Fatalf("%s state = %s", row.File.RelPath, row.File.State)
		}
	}

	cap.mu.Lock()
	sawFinished := false
	for _, n := range cap.names {
		if n == "import:finished" {
			sawFinished = true
		}
	}
	cap.mu.Unlock()
	if !sawFinished {
		t.Fatal("no import:finished event emitted")
	}
}

// TestWireArraysAreNeverNull guards the promise the frontend relies on: empty
// collections arrive as [] so templates can call .length/.find unconditionally.
func TestWireArraysAreNeverNull(t *testing.T) {
	app, _ := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD")
	destA := filepath.Join(base, "A")
	for _, d := range []string{src, destA} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "DCIM", "IMG_0001.JPG"), []byte("jpeg"))

	res, err := app.StartImport(StartRequest{
		Source: src, ProjectCode: "WK-2026", ShootDate: "2026-10-30", DestA: destA,
	})
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	waitIdle(t, app)

	snap, err := app.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Alerts == nil {
		t.Fatal("snapshot alerts is nil")
	}
	rawSnap, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(rawSnap), `"alerts":null`) {
		t.Fatalf("snapshot serializes a null array: %s", rawSnap)
	}

	detail, err := app.ImportDetail(res.Import.ID, 10)
	if err != nil {
		t.Fatalf("ImportDetail: %v", err)
	}
	rawDetail, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	for _, field := range []string{"files", "unacknowledged_events"} {
		if strings.Contains(string(rawDetail), `"`+field+`":null`) {
			t.Fatalf("%s serializes as null: %s", field, rawDetail)
		}
	}

	alerts, err := app.Alerts()
	if err != nil {
		t.Fatalf("Alerts: %v", err)
	}
	if alerts == nil {
		t.Fatal("Alerts returned nil slice")
	}
	events, err := app.RecentEvents(10)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if events == nil {
		t.Fatal("RecentEvents returned nil slice")
	}
}

func TestPreviewFileReturnsDataURL(t *testing.T) {
	app, _ := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD")
	destA := filepath.Join(base, "A")
	destB := filepath.Join(base, "B")
	for _, d := range []string{src, destA, destB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 1200, 800)), nil); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "DCIM", "IMG_0001.JPG"), jpg.Bytes())
	writeFile(t, filepath.Join(src, "DCIM", "notes.txt"), []byte("notes"))

	res, err := app.StartImport(StartRequest{
		Source: src, ProjectCode: "P", ShootDate: "2026-10-30",
		DestA: destA, DestB: destB, RequireBackup: true,
	})
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	waitIdle(t, app)

	detail, err := app.ImportDetail(res.Import.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var photoID, textID string
	for _, row := range detail.Files {
		if strings.HasSuffix(row.File.RelPath, ".JPG") {
			photoID = row.File.ID
		} else {
			textID = row.File.ID
		}
	}
	if photoID == "" || textID == "" {
		t.Fatalf("missing fixture files: %+v", detail.Files)
	}

	got, err := app.PreviewFile(photoID)
	if err != nil {
		t.Fatalf("PreviewFile: %v", err)
	}
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(got.DataURL, prefix) {
		t.Fatalf("data URL prefix = %.40q", got.DataURL)
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got.DataURL, prefix))
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("payload is not JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 341 {
		t.Fatalf("preview = %dx%d, want 512x341", b.Dx(), b.Dy())
	}
	if got.Size != len(data) {
		t.Fatalf("size = %d, payload = %d", got.Size, len(data))
	}

	if _, err := app.PreviewFile(textID); err == nil {
		t.Fatal("preview of a sidecar file must fail")
	}
}

func TestReinsertingCardReusesActiveImport(t *testing.T) {
	app, _ := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD")
	destA := filepath.Join(base, "A")
	destB := filepath.Join(base, "B")
	for _, d := range []string{src, destA, destB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "one.jpg"), []byte("x"))

	req := StartRequest{
		Source: src, ProjectCode: "P", ShootDate: "2026-10-30",
		DestA: destA, DestB: destB, RequireBackup: true,
	}
	first, err := app.StartImport(req)
	if err != nil {
		t.Fatalf("first StartImport: %v", err)
	}
	waitIdle(t, app)

	second, err := app.StartImport(req)
	if err != nil {
		t.Fatalf("second StartImport: %v", err)
	}
	if !second.Existing || second.Import.ID != first.Import.ID {
		t.Fatalf("expected existing import %s, got %+v", first.Import.ID, second)
	}

	rows, err := app.Imports(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d imports, want 1 (re-insertion must not duplicate)", len(rows))
	}
}

func TestSingleDestinationKeepsPersistentWarning(t *testing.T) {
	app, _ := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD")
	destA := filepath.Join(base, "A")
	for _, d := range []string{src, destA} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "one.jpg"), []byte("x"))

	if _, err := app.StartImport(StartRequest{
		Source: src, ProjectCode: "P", ShootDate: "2026-10-30",
		DestA: destA, RequireBackup: false,
	}); err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	snap := waitIdle(t, app)
	if snap.Import == nil || snap.Import.RequireBackup {
		t.Fatalf("import should be single destination: %+v", snap.Import)
	}
	found := false
	for _, a := range snap.Alerts {
		if a.Code == "backup_disabled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("backup_disabled alert missing: %+v", snap.Alerts)
	}
	if len(snap.Alerts) == 0 || snap.UnackedTotal == 0 {
		t.Fatalf("single-destination import must keep an unacknowledged alert")
	}

	if _, err := app.AckAllAlerts(); err != nil {
		t.Fatalf("AckAllAlerts: %v", err)
	}
	snap, err := app.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.UnackedTotal != 0 {
		t.Fatalf("alerts not acknowledged: %d left", snap.UnackedTotal)
	}
}

func TestResumeRejectsCompletedAndPauseRequiresRun(t *testing.T) {
	app, _ := newTestApp(t)
	base := t.TempDir()
	src := filepath.Join(base, "CARD")
	destA := filepath.Join(base, "A")
	destB := filepath.Join(base, "B")
	for _, d := range []string{src, destA, destB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(src, "one.jpg"), []byte("x"))
	res, err := app.StartImport(StartRequest{
		Source: src, ProjectCode: "P", ShootDate: "2026-10-30",
		DestA: destA, DestB: destB, RequireBackup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, app)

	if err := app.RunImport(res.Import.ID); err == nil {
		t.Fatal("RunImport on a completed import should fail")
	}
	if err := app.PauseImport(); err == nil {
		t.Fatal("PauseImport without a running import should fail")
	}
}

func TestSettingsAndDestinationChecks(t *testing.T) {
	app, _ := newTestApp(t)
	got, err := app.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if !got.RequireBackup || got.Workers != 2 {
		t.Fatalf("unexpected defaults: %+v", got)
	}

	dir := t.TempDir()
	want := settings.Settings{DestA: dir, DestB: filepath.Join(dir, "b"), RequireBackup: true, Workers: 3, ProjectCode: "WK"}
	if err := app.SaveSettings(want); err != nil {
		t.Fatal(err)
	}
	got, err = app.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.Workers != 3 || got.ProjectCode != "WK" {
		t.Fatalf("settings not persisted: %+v", got)
	}

	info := app.CheckDestination(dir)
	if !info.Exists || !info.Writable || info.Error != "" {
		t.Fatalf("destination check failed: %+v", info)
	}
	if info.FreeBytes == 0 {
		t.Fatal("expected a non-zero free space reading")
	}
	missing := app.CheckDestination(filepath.Join(dir, "missing"))
	if missing.Exists || missing.Error == "" {
		t.Fatalf("missing directory should report an error: %+v", missing)
	}
}

func TestSelftestThroughBinder(t *testing.T) {
	app, _ := newTestApp(t)
	report := app.Selftest()
	if !report.OK {
		t.Fatalf("selftest failed: %+v", report)
	}
}
