package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"photography-workflow/agent/internal/model"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Fatalf("close store: %v", err)
		}
	})
	return s
}

func seed(t *testing.T, s *Store) (model.Card, model.Import, model.File) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 10, 0, 0, 123456789, time.UTC)
	card, err := s.UpsertCard(ctx, model.Card{
		ID: "card-1", Fingerprint: "fp-1", VolumeGUID: "vol-1",
		Label: "CARD_A", Capacity: 128_000_000_000, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatalf("upsert card: %v", err)
	}
	imp := model.Import{
		ID: "imp-1", ProjectID: "prj-1", ProjectCode: "PRJ-2026-001",
		CardID: card.ID, SourceRoot: "/cards/CARD_A", ShootDate: "2026-10-29",
		DestA: "/vol/a", DestB: "/vol/b", RequireBackup: true,
		Status: model.ImportOpen, CreatedAt: now,
	}
	if err := s.CreateImport(ctx, imp); err != nil {
		t.Fatalf("create import: %v", err)
	}
	f := model.File{
		ID: "file-1", ImportID: imp.ID, RelPath: "DCIM/100CANON/IMG_0001.CR3",
		Size: 25_000_000, MtimeNS: now.UnixNano(), Kind: model.KindPhoto,
		Role: model.RoleRAW, State: model.StateDiscovered,
	}
	if n, err := s.InsertFiles(ctx, []model.File{f}); err != nil || n != 1 {
		t.Fatalf("insert file: n=%d err=%v", n, err)
	}
	return *card, imp, f
}

func TestMigrationsUpDown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seed(t, s)

	if err := s.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n != 0 {
		t.Fatalf("files table still present after down")
	}
	if err := s.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n != 1 {
		t.Fatalf("files table missing after up")
	}
}

func TestOpenEnforcesPragmas(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"synchronous":  "2",
		"foreign_keys": "1",
	} {
		var got string
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("pragma %s: %v", pragma, err)
		}
		if got != want {
			t.Fatalf("pragma %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestCardUpsertPreservesFirstSeen(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	card, _, _ := seed(t, s)
	later := card.FirstSeen.Add(48 * time.Hour)
	updated, err := s.UpsertCard(ctx, model.Card{
		ID: "another-random-id", Fingerprint: card.Fingerprint,
		Label: "CARD_RENAMED", Capacity: card.Capacity, LastSeen: later,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if updated.ID != card.ID {
		t.Fatalf("id changed on conflict: %q != %q", updated.ID, card.ID)
	}
	if !updated.FirstSeen.Equal(card.FirstSeen) {
		t.Fatalf("first_seen changed: %v != %v", updated.FirstSeen, card.FirstSeen)
	}
	if updated.Label != "CARD_RENAMED" {
		t.Fatalf("label = %q", updated.Label)
	}
	if updated.VolumeGUID != card.VolumeGUID {
		t.Fatalf("volume guid lost: %q", updated.VolumeGUID)
	}
	if _, err := s.CardByFingerprint(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing card err = %v", err)
	}
}

func TestImportStatusCAS(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, imp, _ := seed(t, s)

	ok, err := s.SetImportStatus(ctx, imp.ID, []model.ImportStatus{model.ImportUploading}, model.ImportLocalComplete)
	if err != nil {
		t.Fatalf("cas: %v", err)
	}
	if ok {
		t.Fatal("cas should not match a different previous status")
	}
	ok, err = s.SetImportStatus(ctx, imp.ID, []model.ImportStatus{model.ImportOpen}, model.ImportLocalComplete)
	if err != nil || !ok {
		t.Fatalf("cas open -> local_complete: ok=%v err=%v", ok, err)
	}
	got, err := s.Import(ctx, imp.ID)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	if got.Status != model.ImportLocalComplete {
		t.Fatalf("status = %q", got.Status)
	}
	if got.CompletedAt != nil {
		t.Fatalf("completed_at set early: %v", got.CompletedAt)
	}
	ok, err = s.SetImportStatus(ctx, imp.ID, []model.ImportStatus{model.ImportLocalComplete}, model.ImportComplete)
	if err != nil || !ok {
		t.Fatalf("cas local_complete -> complete: ok=%v err=%v", ok, err)
	}
	got, err = s.Import(ctx, imp.ID)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatal("completed_at not set on complete")
	}
	if err := s.SetPauseReason(ctx, imp.ID, "disk_full"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	got, _ = s.Import(ctx, imp.ID)
	if got.PauseReason != "disk_full" {
		t.Fatalf("pause reason = %q", got.PauseReason)
	}
}

func TestInsertFilesIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, imp, f := seed(t, s)
	if n, err := s.InsertFiles(ctx, []model.File{f}); err != nil || n != 0 {
		t.Fatalf("duplicate insert: n=%d err=%v", n, err)
	}
	f2 := f
	f2.ID = "file-2"
	f2.RelPath = "DCIM/100CANON/IMG_0001.JPG"
	f2.Role = model.RoleJPEG
	f2.Kind = model.KindPhoto
	if n, err := s.InsertFiles(ctx, []model.File{f2}); err != nil || n != 1 {
		t.Fatalf("second insert: n=%d err=%v", n, err)
	}
	files, err := s.Files(ctx, imp.ID)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
}

func TestFileStateCASAndCounts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, imp, f := seed(t, s)

	ok, err := s.SetFileState(ctx, f.ID, []model.FileState{model.StateCopying}, model.StateCopying, "")
	if err != nil || ok {
		t.Fatalf("wrong-state cas: ok=%v err=%v", ok, err)
	}
	ok, err = s.SetFileState(ctx, f.ID, []model.FileState{model.StateDiscovered}, model.StateCopying, "")
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ok, err = s.FinishLocalMove(ctx, f.ID, []model.FileState{model.StateCopying},
		model.StateLocalVerified, "abc123", "")
	if err != nil || !ok {
		t.Fatalf("finish: ok=%v err=%v", ok, err)
	}
	got, err := s.File(ctx, f.ID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if got.State != model.StateLocalVerified || got.SHA256 != "abc123" {
		t.Fatalf("file = %q sha=%q", got.State, got.SHA256)
	}
	if !got.State.LocallyDone() {
		t.Fatal("local_verified should be locally done")
	}

	counts, err := s.FileCounts(ctx, imp.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Total != 1 || counts.LocalVerified != 1 || counts.RemoteVerified != 0 || counts.Failed != 0 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestCopiesAndResetInterrupted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, _, f := seed(t, s)

	if err := s.SaveCopy(ctx, model.Copy{
		FileID: f.ID, Dest: model.DestA, FinalPath: `C:\dest\IMG_0001.CR3`,
		TempPath: `C:\dest\.pwf-abc.part`, State: model.CopyCopying,
	}); err != nil {
		t.Fatalf("save copy: %v", err)
	}
	if err := s.SaveCopy(ctx, model.Copy{
		FileID: f.ID, Dest: model.DestB, FinalPath: `C:\backup\IMG_0001.CR3`,
		State: model.CopyPending,
	}); err != nil {
		t.Fatalf("save copy: %v", err)
	}
	if ok, err := s.SetFileState(ctx, f.ID, []model.FileState{model.StateDiscovered}, model.StateCopying, ""); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}

	temps, err := s.ResetInterrupted(ctx)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(temps) != 1 || temps[0] != `C:\dest\.pwf-abc.part` {
		t.Fatalf("temps = %v", temps)
	}
	if again, err := s.ResetInterrupted(ctx); err != nil || len(again) != 0 {
		t.Fatalf("second reset: %v err=%v", again, err)
	}

	got, err := s.File(ctx, f.ID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if got.State != model.StateDiscovered {
		t.Fatalf("file state = %q", got.State)
	}
	copies, err := s.Copies(ctx, f.ID)
	if err != nil {
		t.Fatalf("copies: %v", err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d", len(copies))
	}
	if copies[0].State != model.CopyPending || copies[0].TempPath != "" {
		t.Fatalf("copy A = %+v", copies[0])
	}
	if copies[0].FinalPath == "" {
		t.Fatal("final path should be preserved for adoption on resume")
	}
}

func TestEvents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, err := s.AddEvent(ctx, model.Event{
		Level: "warning", Code: "disk_full", ImportID: "imp-1",
		Message: "disk full on B",
	})
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	if id <= 0 {
		t.Fatalf("event id = %d", id)
	}
	events, err := s.UnacknowledgedEvents(ctx, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("unacked: %v err=%v", events, err)
	}
	if ok, err := s.AckEvent(ctx, id); err != nil || !ok {
		t.Fatalf("ack: ok=%v err=%v", ok, err)
	}
	if ok, err := s.AckEvent(ctx, id); err != nil || ok {
		t.Fatalf("double ack: ok=%v err=%v", ok, err)
	}
	events, err = s.UnacknowledgedEvents(ctx, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("unacked after ack: %v err=%v", events, err)
	}
	all, err := s.Events(ctx, 0, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("events since 0: %v err=%v", all, err)
	}
}

func TestRecentEventsNewestFirst(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, msg := range []string{"first", "second", "third"} {
		if _, err := s.AddEvent(ctx, model.Event{Code: "test", Message: msg}); err != nil {
			t.Fatal(err)
		}
	}
	recent, err := s.RecentEvents(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 || recent[0].Message != "third" || recent[1].Message != "second" {
		t.Fatalf("recent events = %+v", recent)
	}
}

func TestReopenPersistsState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, imp, _ := seed(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.Import(ctx, imp.ID)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	if !got.CreatedAt.Equal(imp.CreatedAt) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, imp.CreatedAt)
	}
	if got.RequireBackup != true {
		t.Fatal("require_backup lost")
	}
	active, err := s.ActiveImportForCard(ctx, imp.CardID)
	if err != nil {
		t.Fatalf("active import: %v", err)
	}
	if active.ID != imp.ID {
		t.Fatalf("active import = %q", active.ID)
	}
}

func TestTimeLayoutOrdering(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, frac := range []int{100_000_000, 150_000_000, 90_000_000} {
		if _, err := s.AddEvent(ctx, model.Event{
			Code: "order", Message: "x", CreatedAt: base.Add(time.Duration(frac)),
		}); err != nil {
			t.Fatalf("event: %v", err)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT created_at FROM events ORDER BY created_at`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var got []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ts, err := parseTime(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		got = append(got, ts)
	}
	want := []time.Duration{90 * time.Millisecond, 100 * time.Millisecond, 150 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("rows = %d", len(got))
	}
	for i, d := range want {
		if !got[i].Equal(base.Add(d)) {
			t.Fatalf("row %d = %v, want %v (text ordering must match time)", i, got[i], base.Add(d))
		}
	}
}

func TestSetFileMetadata(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, _, f := seed(t, s)
	captured := time.Date(2026, 10, 30, 12, 34, 56, 0, time.UTC)
	err := s.SetFileMetadata(ctx, f.ID, model.FileMetadata{
		MIME: "image/x-canon-cr3", Width: 6000, Height: 4000,
		CapturedAt: &captured, Camera: "Canon EOS R5", Lens: "RF24-70mm F2.8",
		ExifJSON: `{"ISO":100}`,
	})
	if err != nil {
		t.Fatalf("set metadata: %v", err)
	}
	got, err := s.File(ctx, f.ID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if got.Mime != "image/x-canon-cr3" || got.Width != 6000 || got.Height != 4000 {
		t.Fatalf("unexpected metadata: %+v", got)
	}
	if got.CapturedAt == nil || !got.CapturedAt.Equal(captured) {
		t.Fatalf("captured_at = %v", got.CapturedAt)
	}
	if got.Camera != "Canon EOS R5" || got.Lens != "RF24-70mm F2.8" || got.ExifJSON != `{"ISO":100}` {
		t.Fatalf("unexpected text fields: %+v", got)
	}
	if err := s.SetFileMetadata(ctx, "missing", model.FileMetadata{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file err = %v", err)
	}
}

func TestForeignKeyEnforced(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.AddEvent(ctx, model.Event{Code: "x", Message: "x"})
	if err != nil {
		t.Fatalf("events may reference no import: %v", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO files (id, import_id, rel_path, size, mtime_ns, kind, role)
		 VALUES ('orphan', 'nope', 'x', 1, 1, 'other', 'other')`)
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("foreign key not enforced: %v", err)
	}
}

func TestUploadPlanAndPartsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, imp, file := seed(t, s)

	if err := s.SetImportServerBatchID(ctx, imp.ID, "batch-1"); err != nil {
		t.Fatalf("set batch id: %v", err)
	}
	gotImp, err := s.Import(ctx, imp.ID)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	if gotImp.ServerBatchID != "batch-1" {
		t.Fatalf("server batch id = %q", gotImp.ServerBatchID)
	}
	if err := s.SetImportProject(ctx, imp.ID, " project-1 "); err != nil {
		t.Fatalf("set project: %v", err)
	}
	gotImp, err = s.Import(ctx, imp.ID)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	if gotImp.ProjectID != "project-1" {
		t.Fatalf("project id = %q", gotImp.ProjectID)
	}
	if err := s.SetImportProject(ctx, "missing-import", "project-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing import project err = %v", err)
	}

	if err := s.SetFileAsset(ctx, file.ID, "asset-1"); err != nil {
		t.Fatalf("set asset: %v", err)
	}
	if err := s.SetFileRemoteBatch(ctx, file.ID, "batch-1"); err != nil {
		t.Fatalf("set remote batch: %v", err)
	}
	gotFile, err := s.File(ctx, file.ID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if gotFile.ServerAssetID != "asset-1" {
		t.Fatalf("server asset id = %q", gotFile.ServerAssetID)
	}
	if gotFile.RemoteBatchID != "batch-1" {
		t.Fatalf("remote batch id = %q", gotFile.RemoteBatchID)
	}
	if err := s.SetFileRemoteBatch(ctx, "missing-file", "batch-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file remote batch err = %v", err)
	}

	expires := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	plan := model.Upload{
		FileID: file.ID, AssetID: "asset-1", Mode: "multipart",
		ObjectKey: "t/1/key", UploadID: "up-1", PartSize: 5 << 20,
		TotalParts: 3, Epoch: 2, ExpiresAt: &expires, State: model.UploadActive,
	}
	if err := s.SaveUpload(ctx, plan); err != nil {
		t.Fatalf("save upload: %v", err)
	}
	got, err := s.Upload(ctx, file.ID)
	if err != nil {
		t.Fatalf("load upload: %v", err)
	}
	if got.UploadID != "up-1" || got.PartSize != 5<<20 || got.TotalParts != 3 ||
		got.Epoch != 2 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("upload round trip mismatch: %+v", got)
	}
	if _, err := s.Upload(ctx, "missing-file"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing upload error = %v", err)
	}

	if err := s.SaveUploadPart(ctx, model.UploadPart{
		FileID: file.ID, Number: 1, Offset: 0, Size: 5 << 20, ETag: "etag-1",
	}); err != nil {
		t.Fatalf("save part 1: %v", err)
	}
	if err := s.SaveUploadPart(ctx, model.UploadPart{
		FileID: file.ID, Number: 2, Offset: 5 << 20, Size: 5 << 20, ETag: "etag-2",
		SHA256: "abc", State: model.PartUploaded,
	}); err != nil {
		t.Fatalf("save part 2: %v", err)
	}
	parts, err := s.UploadParts(ctx, file.ID)
	if err != nil {
		t.Fatalf("list parts: %v", err)
	}
	if len(parts) != 2 || parts[0].Number != 1 || parts[1].ETag != "etag-2" {
		t.Fatalf("parts = %+v", parts)
	}

	if err := s.ResetUploadParts(ctx, file.ID); err != nil {
		t.Fatalf("reset parts: %v", err)
	}
	parts, err = s.UploadParts(ctx, file.ID)
	if err != nil {
		t.Fatalf("list parts after reset: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("parts after reset = %+v", parts)
	}

	// ResetUpload forgets both the session and its parts: a repaired asset must
	// start from a clean plan instead of reusing stale ETags.
	if err := s.SaveUploadPart(ctx, model.UploadPart{
		FileID: file.ID, Number: 1, Offset: 0, Size: 5 << 20, ETag: "etag-9",
	}); err != nil {
		t.Fatalf("save part after reset: %v", err)
	}
	if err := s.ResetUpload(ctx, file.ID); err != nil {
		t.Fatalf("reset upload: %v", err)
	}
	if _, err := s.Upload(ctx, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upload after reset error = %v", err)
	}
	parts, err = s.UploadParts(ctx, file.ID)
	if err != nil {
		t.Fatalf("list parts after upload reset: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("parts after upload reset = %+v", parts)
	}
}

func TestDerivativeResumeState(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	_, _, f := seed(t, s)

	if _, err := s.Derivative(ctx, f.ID, model.DerivativeKindGallery); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing derivative err = %v", err)
	}
	d := model.Derivative{FileID: f.ID, Kind: model.DerivativeKindGallery,
		State: model.DerivativePending}
	if err := s.SaveDerivative(ctx, d); err != nil {
		t.Fatalf("save: %v", err)
	}
	ok, err := s.SetDerivativeState(ctx, f.ID, model.DerivativeKindGallery,
		[]string{model.DerivativePending}, model.DerivativeGenerated, "")
	if err != nil || !ok {
		t.Fatalf("transition to generating: ok=%v err=%v", ok, err)
	}
	if ok, err := s.SetDerivativeState(ctx, f.ID, model.DerivativeKindGallery,
		[]string{model.DerivativePending}, model.DerivativeUploaded, ""); err != nil || ok {
		t.Fatalf("stale transition must fail: ok=%v err=%v", ok, err)
	}

	if err := s.SaveDerivative(ctx, model.Derivative{FileID: f.ID, Kind: model.DerivativeKindGallery,
		Path: "/cache/gallery.jpg", SHA256: strings.Repeat("a", 64), Size: 1234,
		Width: 2560, Height: 1707, State: model.DerivativeUploaded}); err != nil {
		t.Fatalf("save uploaded: %v", err)
	}
	got, err := s.Derivative(ctx, f.ID, model.DerivativeKindGallery)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != model.DerivativeUploaded || got.Width != 2560 || got.Size != 1234 || got.Path == "" {
		t.Fatalf("derivative = %+v", got)
	}
	list, err := s.Derivatives(ctx, f.ImportID)
	if err != nil || len(list) != 1 || list[0].FileID != f.ID {
		t.Fatalf("list = %+v err=%v", list, err)
	}
}
