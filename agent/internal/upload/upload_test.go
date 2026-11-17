package upload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/preview"
	"photography-workflow/agent/internal/store"
)

// ---------- fixture ----------

type fixture struct {
	st    *store.Store
	imp   *model.Import
	files []model.File
}

func newFixture(t *testing.T, contents map[string][]byte) *fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	card, err := st.UpsertCard(ctx, model.Card{
		ID: "card-1", Fingerprint: "fp-1", VolumeGUID: "vol-1",
		Label: "CARD_A", Capacity: 1 << 30, FirstSeen: now, LastSeen: now,
	})
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	imp := model.Import{
		ID: "imp-1", ProjectID: "prj-1", ProjectCode: "WK-1", CardID: card.ID,
		SourceRoot: "/cards/A", ShootDate: "2026-10-30",
		DestA: t.TempDir(), DestB: t.TempDir(), RequireBackup: true,
		Status: model.ImportOpen, CreatedAt: now,
	}
	if err := st.CreateImport(ctx, imp); err != nil {
		t.Fatalf("create import: %v", err)
	}
	var files []model.File
	i := 0
	for rel, data := range contents {
		i++
		f := model.File{
			ID: fmt.Sprintf("file-%d", i), ImportID: imp.ID, RelPath: rel,
			Size: int64(len(data)), MtimeNS: now.UnixNano(),
			Kind: model.KindPhoto, Role: model.RoleRAW, State: model.StateDiscovered,
			Mime: "image/x-canon-cr3",
		}
		if _, err := st.InsertFiles(ctx, []model.File{f}); err != nil {
			t.Fatalf("insert file: %v", err)
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		if _, err := st.FinishLocalMove(ctx, f.ID, []model.FileState{model.StateDiscovered},
			model.StateLocalVerified, sha, ""); err != nil {
			t.Fatalf("verify: %v", err)
		}
		path := filepath.Join(imp.DestA, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveCopy(ctx, model.Copy{
			FileID: f.ID, Dest: model.DestA, FinalPath: path,
			State: model.CopyVerified, Bytes: int64(len(data)), SHA256: sha,
		}); err != nil {
			t.Fatalf("copy A: %v", err)
		}
		f.SHA256 = sha
		f.State = model.StateLocalVerified
		files = append(files, f)
	}
	if _, err := st.SetImportStatus(ctx, imp.ID,
		[]model.ImportStatus{model.ImportOpen}, model.ImportLocalComplete); err != nil {
		t.Fatalf("local complete: %v", err)
	}
	return &fixture{st: st, imp: &imp, files: files}
}

// ---------- stub server ----------

type stubAsset struct {
	ID       string `json:"id"`
	Project  string `json:"project_id"`
	Hash     string `json:"content_hash"`
	Filename string `json:"filename"`
	Size     int64  `json:"size_bytes"`
	Status   string `json:"status"`
	Batch    string `json:"-"`
}

type stubConfig struct {
	singleLimit int64 // files at or above this use multipart
	partSize    int64
	// forcedDuplicate maps rel_path to the status a duplicate registration
	// should report ("verified", "uploaded"; empty means no duplicate).
	forcedDuplicate map[string]string
	// failPart maps part number to remaining 503 responses.
	failPart map[int32]int
	// expireFirstPresign makes the first single presign URL reject the PUT.
	expireFirstPresign bool
	// authFailure makes the next registration return 401.
	authFailure bool
	// offline makes presign return 503 for every call.
	offline bool
	// corruptOnce makes the first verification report one corrupt asset.
	corruptOnce bool
	// hideAssets makes the per-asset status endpoint fail.
	hideAssets bool
}

type stubServer struct {
	t      *testing.T
	cfg    stubConfig
	server *httptest.Server

	mu       sync.Mutex
	batches  map[string]string // batch id -> status (open/verifying/completed/partial/failed)
	assets   map[string]*stubAsset
	byHash   map[string]string
	sessions map[string]*stubSession // upload id -> session
	parts    map[string]map[int32][]byte
	partETag map[string]map[int32]string
	objects  map[string][]byte
	jobs     map[string]*api.Job
	batchSeq int
	jobSeq   int

	keys     []string // "<method> <path>|<Idempotency-Key>"
	putCalls []string // storage paths actually written
	corrupt  bool

	derivatives    map[string][]byte // asset id -> accepted gallery preview
	derivPresigns  int
	derivCompletes int
}

type stubSession struct {
	AssetID  string
	UploadID string
	Key      string
	PartSize int64
}

func newStub(t *testing.T, cfg stubConfig) *stubServer {
	if cfg.singleLimit == 0 {
		cfg.singleLimit = 1024
	}
	if cfg.partSize == 0 {
		cfg.partSize = 4
	}
	s := &stubServer{
		t: t, cfg: cfg,
		batches:     map[string]string{},
		assets:      map[string]*stubAsset{},
		byHash:      map[string]string{},
		sessions:    map[string]*stubSession{},
		parts:       map[string]map[int32][]byte{},
		partETag:    map[string]map[int32]string{},
		objects:     map[string][]byte{},
		jobs:        map[string]*api.Job{},
		derivatives: map[string][]byte{},
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubServer) URL() string { return s.server.URL }

func (s *stubServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	key := r.Method + " " + r.URL.Path
	if idem := r.Header.Get("Idempotency-Key"); idem != "" {
		key += "|" + idem
	}
	s.keys = append(s.keys, key)
	s.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/")
	segs := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodPost && path == "agent/v1/ingest-batches":
		s.createBatch(w, r)
	case r.Method == http.MethodPost && len(segs) == 6 && segs[0] == "agent" && segs[1] == "v1" &&
		segs[2] == "ingest-batches" && segs[4] == "assets" && segs[5] == "batch":
		s.register(w, r, segs[3])
	case r.Method == http.MethodPost && len(segs) == 5 && segs[0] == "agent" && segs[1] == "v1" &&
		segs[2] == "ingest-batches" && segs[4] == "finalize":
		s.finalize(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 4 && segs[0] == "agent" && segs[1] == "v1" && segs[2] == "jobs":
		s.getJob(w, segs[3])
	case r.Method == http.MethodPost && path == "agent/v1/uploads/presign":
		s.presign(w, r)
	case r.Method == http.MethodPost && len(segs) == 5 && segs[0] == "agent" && segs[1] == "v1" &&
		segs[2] == "uploads" && segs[4] == "complete":
		s.complete(w, r, segs[3])
	case r.Method == http.MethodGet && len(segs) == 5 && segs[0] == "api" && segs[1] == "v1" &&
		segs[2] == "projects" && segs[4] == "assets":
		s.listAssets(w, r, segs[3])
	case r.Method == http.MethodPost && path == "agent/v1/derivatives:presign":
		s.presignDerivative(w, r)
	case r.Method == http.MethodPost && len(segs) == 4 && segs[0] == "agent" && segs[1] == "v1" &&
		segs[2] == "derivatives" && strings.HasSuffix(segs[3], ":complete"):
		s.completeDerivative(w, r, strings.TrimSuffix(segs[3], ":complete"))
	case r.Method == http.MethodPut && segs[0] == "storage":
		s.put(w, r, segs)
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeProblem(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"code": code, "status": fmt.Sprint(status), "detail": code})
}

func (s *stubServer) createBatch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req api.CreateBatchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.batchSeq++
	id := fmt.Sprintf("batch-%d", s.batchSeq)
	s.batches[id] = "open"
	writeJSON(w, http.StatusCreated, api.Batch{ID: id, ProjectID: req.ProjectID, Status: "open"})
}

func (s *stubServer) register(w http.ResponseWriter, r *http.Request, batchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.authFailure {
		s.cfg.authFailure = false
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	status, ok := s.batches[batchID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	if status != "open" {
		writeProblem(w, http.StatusConflict, "batch_closed")
		return
	}
	var req struct {
		Files []api.FileInput `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body")
		return
	}
	out := api.RegisterResult{Accepted: len(req.Files)}
	for _, f := range req.Files {
		if forced, ok := s.cfg.forcedDuplicate[f.RelPath]; ok && forced != "" {
			id, exists := s.byHash[f.SHA256]
			if !exists {
				id = fmt.Sprintf("asset-dup-%d", len(s.assets)+1)
				s.byHash[f.SHA256] = id
				s.assets[id] = &stubAsset{ID: id, Project: "prj-1", Hash: f.SHA256, Size: f.Size, Status: forced, Batch: "other"}
			}
			out.Duplicated++
			out.Assets = append(out.Assets, api.AssetResult{
				RelPath: f.RelPath, AssetID: id, Duplicated: true,
				Status: s.assets[id].Status,
			})
			continue
		}
		if id, ok := s.byHash[f.SHA256]; ok {
			a := s.assets[id]
			if a.Status == "uploaded" || a.Status == "verified" {
				out.Duplicated++
				out.Assets = append(out.Assets, api.AssetResult{
					RelPath: f.RelPath, AssetID: id, Duplicated: true, Status: a.Status,
				})
				continue
			}
			// registered/missing/corrupt assets are relinked to this batch.
			a.Batch = batchID
			out.Duplicated++
			out.Assets = append(out.Assets, api.AssetResult{
				RelPath: f.RelPath, AssetID: id, Duplicated: true, Status: a.Status,
			})
			continue
		}
		id := fmt.Sprintf("asset-%d", len(s.assets)+1)
		s.byHash[f.SHA256] = id
		s.assets[id] = &stubAsset{
			ID: id, Project: "prj-1", Hash: f.SHA256, Filename: f.RelPath,
			Size: f.Size, Status: "registered", Batch: batchID,
		}
		out.Inserted++
		out.Assets = append(out.Assets, api.AssetResult{
			RelPath: f.RelPath, AssetID: id, Duplicated: false, Status: "registered",
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *stubServer) presign(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.offline {
		writeProblem(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	var req api.PresignRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	a, ok := s.assets[req.AssetID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	if a.Status == "uploaded" || a.Status == "verified" {
		writeProblem(w, http.StatusConflict, "asset_immutable")
		return
	}
	if s.batches[a.Batch] != "open" {
		writeProblem(w, http.StatusConflict, "batch_closed")
		return
	}
	expires := time.Now().Add(time.Hour)
	if req.Size < s.cfg.singleLimit {
		writeJSON(w, http.StatusOK, api.UploadPlan{
			Mode: "single", AssetID: a.ID, ObjectKey: a.ID + "/single",
			URL: s.URL() + "/storage/single/" + a.ID, ExpiresAt: expires,
		})
		return
	}
	var sess *stubSession
	for _, ss := range s.sessions {
		if ss.AssetID == a.ID {
			sess = ss
		}
	}
	if sess == nil {
		sess = &stubSession{AssetID: a.ID, UploadID: fmt.Sprintf("up-%d", len(s.sessions)+1), PartSize: s.cfg.partSize}
		s.sessions[sess.UploadID] = sess
		s.parts[sess.UploadID] = map[int32][]byte{}
		s.partETag[sess.UploadID] = map[int32]string{}
	}
	total := int32((req.Size + sess.PartSize - 1) / sess.PartSize)
	parts := make([]api.PresignedPart, 0, total)
	for n := int32(1); n <= total; n++ {
		parts = append(parts, api.PresignedPart{
			PartNumber: n,
			URL:        fmt.Sprintf("%s/storage/part/%s/%d", s.URL(), sess.UploadID, n),
		})
	}
	writeJSON(w, http.StatusOK, api.UploadPlan{
		Mode: "multipart", AssetID: a.ID, ObjectKey: a.ID + "/multi",
		UploadID: sess.UploadID, PartSize: sess.PartSize, Parts: parts, ExpiresAt: expires,
	})
}

func (s *stubServer) put(w http.ResponseWriter, r *http.Request, segs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putCalls = append(s.putCalls, strings.Join(segs, "/"))
	body := new(strings.Builder)
	buf := make([]byte, 64<<10)
	var data []byte
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	_ = body
	switch segs[1] {
	case "single":
		assetID := segs[2]
		if s.cfg.expireFirstPresign {
			s.cfg.expireFirstPresign = false
			writeProblem(w, http.StatusForbidden, "signature_expired")
			return
		}
		s.objects[assetID] = data
		etag := fmt.Sprintf("%x", sha256.Sum256(data))[:16]
		w.Header().Set("ETag", `"`+etag+`"`)
		w.WriteHeader(http.StatusOK)
	case "part":
		uploadID := segs[2]
		var n int32
		_, _ = fmt.Sscanf(segs[3], "%d", &n)
		if s.cfg.failPart != nil {
			if left := s.cfg.failPart[n]; left > 0 {
				s.cfg.failPart[n] = left - 1
				writeProblem(w, http.StatusServiceUnavailable, "unavailable")
				return
			}
		}
		s.parts[uploadID][n] = data
		etag := fmt.Sprintf("%x", sha256.Sum256(data))[:16]
		s.partETag[uploadID][n] = etag
		w.Header().Set("ETag", `"`+etag+`"`)
		w.WriteHeader(http.StatusOK)
	case "deriv":
		s.objects["deriv/"+segs[2]] = data
		etag := fmt.Sprintf("%x", sha256.Sum256(data))[:16]
		w.Header().Set("ETag", `"`+etag+`"`)
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (s *stubServer) complete(w http.ResponseWriter, r *http.Request, assetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req api.CompleteUploadRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	a, ok := s.assets[assetID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	if req.UploadID != "" {
		parts := s.parts[req.UploadID]
		var data []byte
		for _, p := range req.Parts {
			chunk, ok := parts[p.PartNumber]
			if !ok {
				writeProblem(w, http.StatusUnprocessableEntity, "upload_missing")
				return
			}
			if etag := s.partETag[req.UploadID][p.PartNumber]; etag != p.ETag {
				writeProblem(w, http.StatusUnprocessableEntity, "upload_missing")
				return
			}
			data = append(data, chunk...)
		}
		s.objects[assetID] = data
	}
	if _, ok := s.objects[assetID]; !ok {
		writeProblem(w, http.StatusUnprocessableEntity, "upload_missing")
		return
	}
	a.Status = "uploaded"
	a.Size = int64(len(s.objects[assetID]))
	writeJSON(w, http.StatusOK, api.Asset{ID: a.ID, Status: a.Status, SizeBytes: a.Size})
}

func (s *stubServer) presignDerivative(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req api.DerivativePresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body")
		return
	}
	a, ok := s.assets[req.AssetID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	if req.Kind != model.DerivativeKindGallery || len(req.SHA256) != 64 {
		writeProblem(w, http.StatusUnprocessableEntity, "unsupported_derivative")
		return
	}
	s.derivPresigns++
	writeJSON(w, http.StatusOK, api.DerivativePresign{
		Key:       "t/t1/p/prj-1/" + req.SHA256[:2] + "/" + req.SHA256 + "/gallery.jpg",
		URL:       s.URL() + "/storage/deriv/" + a.ID,
		ExpiresAt: time.Now().Add(time.Hour),
	})
}

func (s *stubServer) completeDerivative(w http.ResponseWriter, r *http.Request, assetID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req api.DerivativeCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_body")
		return
	}
	data, ok := s.objects["deriv/"+assetID]
	if !ok {
		writeProblem(w, http.StatusUnprocessableEntity, "derivative_missing")
		return
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != req.Size || hex.EncodeToString(sum[:]) != req.SHA256 {
		writeProblem(w, http.StatusUnprocessableEntity, "derivative_hash_mismatch")
		return
	}
	if req.Width <= 0 || req.Height <= 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_file")
		return
	}
	s.derivatives[assetID] = data
	s.derivCompletes++
	writeJSON(w, http.StatusOK, api.Derivative{
		AssetID: assetID, Kind: req.Kind, Width: req.Width, Height: req.Height,
		Bytes: req.Size, Status: "derivative_ready",
	})
}

func (s *stubServer) finalize(w http.ResponseWriter, r *http.Request, batchID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status, ok := s.batches[batchID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	if status == "open" {
		s.jobSeq++
		jobID := fmt.Sprintf("job-%d", s.jobSeq)
		var verified, corrupt int
		for _, a := range s.assets {
			if a.Batch != batchID {
				continue
			}
			switch {
			case s.cfg.corruptOnce && !s.corrupt:
				s.corrupt = true
				a.Status = "corrupt"
				corrupt++
			case a.Status == "uploaded" || a.Status == "verified":
				a.Status = "verified"
				verified++
			}
		}
		result := map[string]any{
			"file_count": verified + corrupt, "verified": verified, "corrupt": corrupt, "missing": 0,
		}
		s.jobs[jobID] = &api.Job{ID: jobID, Kind: "ingest.verify", Status: "succeeded",
			SubjectID: &batchID, Result: result}
		s.batches[batchID] = "completed"
		writeJSON(w, http.StatusAccepted, s.jobs[jobID])
		return
	}
	if job, ok := s.lastJobFor(batchID); ok {
		writeJSON(w, http.StatusAccepted, job)
		return
	}
	writeProblem(w, http.StatusConflict, "batch_closed")
}

func (s *stubServer) lastJobFor(batchID string) (*api.Job, bool) {
	for _, j := range s.jobs {
		if j.SubjectID != nil && *j.SubjectID == batchID {
			return j, true
		}
	}
	return nil, false
}

func (s *stubServer) getJob(w http.ResponseWriter, jobID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *stubServer) listAssets(w http.ResponseWriter, r *http.Request, projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.hideAssets {
		writeProblem(w, http.StatusInternalServerError, "internal")
		return
	}
	items := make([]api.Asset, 0, len(s.assets))
	for _, a := range s.assets {
		items = append(items, api.Asset{ID: a.ID, ProjectID: a.Project, ContentHash: a.Hash, Status: a.Status})
	}
	writeJSON(w, http.StatusOK, api.AssetList{Items: items, Total: len(items), Limit: 200})
}

func (s *stubServer) sawKey(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if strings.Contains(k, "|"+prefix) {
			return true
		}
	}
	return false
}

func (s *stubServer) putCount(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.putCalls {
		if strings.Contains(p, substr) {
			n++
		}
	}
	return n
}

// ---------- helpers ----------

func newUploader(t *testing.T, f *fixture, stub *stubServer, opts Options) *Uploader {
	t.Helper()
	client, err := api.New(stub.URL(), api.Options{
		Token: func(context.Context) (string, error) { return "tok", nil },
		Retry: api.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	if opts.DeviceID == "" {
		opts.DeviceID = "dev-1"
	}
	opts.PollBase = time.Millisecond
	opts.PollMax = 2 * time.Millisecond
	opts.PollTimeout = 2 * time.Second
	return New(f.st, client, opts)
}

func fileState(t *testing.T, st *store.Store, id string) model.File {
	t.Helper()
	f, err := st.File(context.Background(), id)
	if err != nil {
		t.Fatalf("load file %s: %v", id, err)
	}
	return *f
}

func importStatus(t *testing.T, st *store.Store, id string) model.Import {
	t.Helper()
	im, err := st.Import(context.Background(), id)
	if err != nil {
		t.Fatalf("load import: %v", err)
	}
	return *im
}

// ---------- tests ----------

func TestSingleUploadCompletesImport(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("hello raw")})
	stub := newStub(t, stubConfig{})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.RemoteVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	got := fileState(t, f.st, f.files[0].ID)
	if got.State != model.StateRemoteVerified || got.ServerAssetID == "" || got.RemoteBatchID == "" {
		t.Fatalf("file = %+v", got)
	}
	if im := importStatus(t, f.st, f.imp.ID); im.Status != model.ImportComplete || im.ServerBatchID == "" {
		t.Fatalf("import = %+v", im)
	}
	for _, prefix := range []string{"agent:batch:" + f.imp.ID, "agent:reg:", "agent:presign:", "agent:finalize:"} {
		if !stub.sawKey(prefix) {
			t.Fatalf("missing idempotency key prefix %q in %v", prefix, stub.keys)
		}
	}
	if !stub.sawKey("agent:complete:") {
		t.Fatalf("missing complete idempotency key: %v", stub.keys)
	}
}

func TestMultipartResumesAfterTransientPartFailure(t *testing.T) {
	content := []byte("0123456789") // 3 parts of 4 bytes
	f := newFixture(t, map[string][]byte{"DCIM/BIG.CR3": content})
	stub := newStub(t, stubConfig{
		singleLimit: 4, partSize: 4, // 10 bytes -> three parts
		failPart: map[int32]int{2: 2},
	})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !res.Paused || res.Reason != "offline" {
		t.Fatalf("first run should pause offline, got %+v", res)
	}
	if got := fileState(t, f.st, f.files[0].ID); got.State != model.StateRegistered && got.State != model.StateUploading {
		t.Fatalf("file state after transient failure = %q", got.State)
	}
	parts, err := f.st.UploadParts(context.Background(), f.files[0].ID)
	if err != nil || len(parts) < 1 {
		t.Fatalf("parts after first run = %v err=%v", parts, err)
	}
	before := stub.putCount("/part/")

	res, err = up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Paused || res.RemoteVerified != 1 {
		t.Fatalf("second run = %+v", res)
	}
	if got := fileState(t, f.st, f.files[0].ID); got.State != model.StateRemoteVerified {
		t.Fatalf("file state = %q", got.State)
	}
	if again := stub.putCount("/part/") - before; again >= 3 {
		t.Fatalf("resume re-uploaded every part: %d new puts (total before %d)", again, before)
	}
}

func TestDuplicateVerifiedSkipsUpload(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("dup")})
	stub := newStub(t, stubConfig{forcedDuplicate: map[string]string{"DCIM/IMG_0001.CR3": "verified"}})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RemoteVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	if n := stub.putCount("storage/"); n != 0 {
		t.Fatalf("duplicate verified file was uploaded: %d puts", n)
	}
}

func TestDuplicateUploadedElsewhereStaysPending(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("dup")})
	stub := newStub(t, stubConfig{forcedDuplicate: map[string]string{"DCIM/IMG_0001.CR3": "uploaded"}})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := fileState(t, f.st, f.files[0].ID)
	if got.State != model.StateUploaded {
		t.Fatalf("file state = %q", got.State)
	}
	if res.Pending != 1 {
		t.Fatalf("pending = %d, result = %+v", res.Pending, res)
	}
	if im := importStatus(t, f.st, f.imp.ID); im.Status != model.ImportUploading {
		t.Fatalf("import status = %q", im.Status)
	}
}

func TestPresignExpiryRetriesWithNewEpoch(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("short")})
	stub := newStub(t, stubConfig{expireFirstPresign: true})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RemoteVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !stub.sawKey(fmt.Sprintf("agent:presign:%s:%d", "asset-1", 2)) {
		t.Fatalf("expected a second presign epoch, keys = %v", stub.keys)
	}
	if !stub.sawKey(fmt.Sprintf("agent:complete:%s:%d", "asset-1", 2)) {
		t.Fatalf("complete did not use the fresh epoch, keys = %v", stub.keys)
	}
}

func TestAuthFailurePausesWithReason(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("x")})
	stub := newStub(t, stubConfig{authFailure: true})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Reason != "auth_required" {
		t.Fatalf("result = %+v", res)
	}
	if im := importStatus(t, f.st, f.imp.ID); im.PauseReason != "auth_required" {
		t.Fatalf("pause reason = %q", im.PauseReason)
	}
}

func TestOfflinePausesInsteadOfFailingFiles(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("x")})
	stub := newStub(t, stubConfig{offline: true})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Paused || res.Reason != "offline" {
		t.Fatalf("result = %+v", res)
	}
	if got := fileState(t, f.st, f.files[0].ID); got.State == model.StateUploadFailed {
		t.Fatalf("offline must not permanently fail a file: %+v", got)
	}
}

func TestCorruptVerificationRepairsThroughNewBatch(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("content")})
	stub := newStub(t, stubConfig{corruptOnce: true})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RemoteVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := fileState(t, f.st, f.files[0].ID); got.State != model.StateRemoteVerified {
		t.Fatalf("file = %+v", got)
	}
	stub.mu.Lock()
	batches := len(stub.batches)
	stub.mu.Unlock()
	if batches != 2 {
		t.Fatalf("expected a repair batch, batches = %d", batches)
	}
	if !stub.sawKey("agent:batch:" + f.imp.ID + ":batch-1") {
		t.Fatalf("repair batch key missing: %v", stub.keys)
	}
}

func TestVerifyResultWithoutAssetListStaysUploaded(t *testing.T) {
	f := newFixture(t, map[string][]byte{
		"DCIM/A.CR3": []byte("aaa"),
		"DCIM/B.CR3": []byte("bbb"),
	})
	// One asset is corrupt so the aggregate result cannot be trusted; the
	// per-asset list is also unavailable.
	stub := newStub(t, stubConfig{corruptOnce: true, hideAssets: true})
	up := newUploader(t, f, stub, Options{})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RemoteVerified != 0 {
		t.Fatalf("nothing may be verified on a guess: %+v", res)
	}
	for _, file := range f.files {
		if got := fileState(t, f.st, file.ID); got.State != model.StateUploaded {
			t.Fatalf("%s state = %q", file.RelPath, got.State)
		}
	}
}

func TestChunkInputsRespectsCountAndSize(t *testing.T) {
	files := make([]model.File, 0, 700)
	for i := 0; i < 700; i++ {
		files = append(files, model.File{
			ID: fmt.Sprintf("f%d", i), RelPath: fmt.Sprintf("DCIM/IMG_%04d.CR3", i),
			Size: 1000, SHA256: strings.Repeat("a", 64),
		})
	}
	offset, chunks := 0, 0
	for offset < len(files) {
		chunk, inputs, n := chunkInputs(files[offset:], maxFilesPerRequest, maxRegisterBytes)
		if n == 0 || n > maxFilesPerRequest || len(inputs) != n {
			t.Fatalf("chunk n=%d inputs=%d", n, len(inputs))
		}
		if len(chunk) != n {
			t.Fatalf("chunk len=%d n=%d", len(chunk), n)
		}
		offset += n
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("chunks = %d, want 2", chunks)
	}

	big := []model.File{{
		ID: "big", RelPath: "a", SHA256: strings.Repeat("b", 64),
		ExifJSON: `{"note":"` + strings.Repeat("x", maxRegisterBytes) + `"}`,
	}}
	if _, _, n := chunkInputs(big, maxFilesPerRequest, maxRegisterBytes); n != 1 {
		t.Fatalf("an oversized single file must still form its own chunk, n=%d", n)
	}
}

func TestFileInputDropsInvalidExif(t *testing.T) {
	in := fileInput(model.File{RelPath: "a", Size: 1, SHA256: "h", ExifJSON: `{not json`})
	if in.Exif != nil {
		t.Fatalf("invalid exif must be omitted")
	}
}

// ---------- gallery previews ----------

type fakePreviews struct {
	t     *testing.T
	dir   string
	fail  map[string]bool
	calls int
	mu    sync.Mutex
}

func (p *fakePreviews) Gallery(ctx context.Context, f model.File) (*preview.GalleryPreview, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.fail[f.RelPath] {
		return nil, errors.New("preview: unsupported media type")
	}
	data := []byte("gallery-jpeg:" + f.RelPath)
	path := filepath.Join(p.dir, strings.ReplaceAll(f.RelPath, "/", "_")+".jpg")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		p.t.Fatalf("write fake preview: %v", err)
	}
	sum := sha256.Sum256(data)
	return &preview.GalleryPreview{
		Path: path, Width: 2560, Height: 1707,
		Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func TestDerivativeUploadedAfterVerification(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("hello raw")})
	stub := newStub(t, stubConfig{})
	prev := &fakePreviews{t: t, dir: t.TempDir()}
	up := newUploader(t, f, stub, Options{Previews: prev})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Paused || res.RemoteVerified != 1 {
		t.Fatalf("result = %+v", res)
	}
	if res.Derivatives != 1 || res.DerivativesFailed != 0 {
		t.Fatalf("derivative counts = %d/%d", res.Derivatives, res.DerivativesFailed)
	}
	d, err := f.st.Derivative(context.Background(), f.files[0].ID, model.DerivativeKindGallery)
	if err != nil {
		t.Fatalf("load derivative: %v", err)
	}
	if d.State != model.DerivativeUploaded || d.SHA256 == "" || d.Size == 0 || d.Width != 2560 {
		t.Fatalf("derivative = %+v", d)
	}
	assetID := fileState(t, f.st, f.files[0].ID).ServerAssetID
	if assetID == "" || len(stub.derivatives[assetID]) == 0 {
		t.Fatalf("server did not accept the preview for asset %q", assetID)
	}
	if stub.derivCompletes != 1 {
		t.Fatalf("complete calls = %d", stub.derivCompletes)
	}
	if !stub.sawKey("agent:deriv:") {
		t.Fatalf("missing derivative idempotency key: %v", stub.keys)
	}
	if im := importStatus(t, f.st, f.imp.ID); im.Status != model.ImportComplete {
		t.Fatalf("import = %+v", im)
	}

	// A second pass over a completed import is a no-op: the uploadable preview
	// was already accepted and is not pushed again.
	before := stub.derivPresigns
	res, err = up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Derivatives != 1 || res.DerivativesFailed != 0 {
		t.Fatalf("second run derivative counts = %d/%d", res.Derivatives, res.DerivativesFailed)
	}
	if stub.derivPresigns != before {
		t.Fatalf("already uploaded preview was re-presigned: %d -> %d", before, stub.derivPresigns)
	}
}

func TestDerivativeFailureDoesNotBlockImport(t *testing.T) {
	f := newFixture(t, map[string][]byte{"DCIM/IMG_0001.CR3": []byte("hello raw")})
	stub := newStub(t, stubConfig{})
	prev := &fakePreviews{
		t: t, dir: t.TempDir(),
		fail: map[string]bool{"DCIM/IMG_0001.CR3": true},
	}
	var (
		mu     sync.Mutex
		events []model.Event
	)
	up := newUploader(t, f, stub, Options{
		Previews: prev,
		OnEvent: func(e model.Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})

	res, err := up.Run(context.Background(), f.imp.ID)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.RemoteVerified != 1 || res.Derivatives != 0 || res.DerivativesFailed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if im := importStatus(t, f.st, f.imp.ID); im.Status != model.ImportComplete {
		t.Fatalf("a preview failure must not change the import status: %+v", im)
	}
	d, err := f.st.Derivative(context.Background(), f.files[0].ID, model.DerivativeKindGallery)
	if err != nil {
		t.Fatalf("load derivative: %v", err)
	}
	if d.State != model.DerivativeFailed || d.Error == "" {
		t.Fatalf("derivative = %+v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, e := range events {
		if e.Code == "derivative_upload_failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing derivative_upload_failed event: %+v", events)
	}
}
