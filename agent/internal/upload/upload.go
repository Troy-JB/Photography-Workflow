// Package upload pushes a locally verified import to the server: it creates the
// ingest batch, registers assets in size-bounded chunks, transfers each file
// through a presigned URL (single PUT or resumable multipart), finalizes the
// batch and reconciles the verification result back onto every file.
//
// Every mutating call carries a deterministic idempotency key, so a crash at
// any point resumes without creating a second batch, asset or verification job.
// Pauses are explicit: an expired session or an unreachable server stops the
// run and keeps the state machine where it is instead of degrading silently.
package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"photography-workflow/agent/internal/api"
	"photography-workflow/agent/internal/auth"
	"photography-workflow/agent/internal/fsx"
	"photography-workflow/agent/internal/model"
	"photography-workflow/agent/internal/preview"
	"photography-workflow/agent/internal/store"
)

var (
	ErrNotLocalComplete = errors.New("upload: 本地拷贝尚未全部校验完成")
	ErrProjectRequired  = errors.New("upload: 请先为该导入关联服务器项目")
	ErrNoDevice         = errors.New("upload: 设备尚未注册")
)

// Limits mirror the server contract (server/internal/ingest).
const (
	maxFilesPerRequest = 500
	maxRegisterBytes   = 1 << 20 // half the server's 2 MiB JSON limit
	maxRepairRounds    = 2
	presignAttempts    = 3
)

var (
	errSealed         = errors.New("upload: batch sealed")
	errPresignExpired = errors.New("upload: presigned url expired")
	errBatchImmutable = errors.New("upload: asset already stored")
)

// Previewer renders the uploadable gallery JPEG for a file. derive.Derive
// implements it, so RAW embedded previews and ffmpeg stills stay behind the
// same helper lifecycle the UI already uses.
type Previewer interface {
	Gallery(ctx context.Context, f model.File) (*preview.GalleryPreview, error)
}

// Options configures an Uploader.
type Options struct {
	DeviceID string
	OnEvent  func(model.Event)
	// SourcePath resolves the local file backing an upload. The default reads
	// the verified A copy, falling back to B.
	SourcePath func(ctx context.Context, f model.File) (string, error)
	// Previews supplies the gallery JPEG uploaded after remote verification.
	// Nil disables derivative uploads entirely.
	Previews Previewer
	Workers  int
	// PollBase and PollMax bound the finalize job poll interval.
	PollBase, PollMax time.Duration
	// PollTimeout bounds how long one verification may take.
	PollTimeout time.Duration
}

type Uploader struct {
	store    *store.Store
	client   *api.Client
	deviceID string
	onEvent  func(model.Event)
	source   func(ctx context.Context, f model.File) (string, error)
	previews Previewer
	workers  int
	pollBase time.Duration
	pollMax  time.Duration
	pollTTL  time.Duration
	mu       sync.Mutex
}

func New(st *store.Store, client *api.Client, opts Options) *Uploader {
	u := &Uploader{
		store:    st,
		client:   client,
		deviceID: opts.DeviceID,
		onEvent:  opts.OnEvent,
		source:   opts.SourcePath,
		previews: opts.Previews,
		workers:  opts.Workers,
		pollBase: opts.PollBase,
		pollMax:  opts.PollMax,
		pollTTL:  opts.PollTimeout,
	}
	if u.source == nil {
		u.source = u.defaultSource
	}
	if u.workers <= 0 {
		u.workers = 2
	}
	if u.workers > 4 {
		u.workers = 4
	}
	if u.pollBase <= 0 {
		u.pollBase = 2 * time.Second
	}
	if u.pollMax < u.pollBase {
		u.pollMax = 30 * time.Second
	}
	if u.pollTTL <= 0 {
		u.pollTTL = 30 * time.Minute
	}
	return u
}

// Result is the outcome of one upload pass.
type Result struct {
	ImportID       string `json:"import_id"`
	BatchID        string `json:"batch_id,omitempty"`
	Paused         bool   `json:"paused"`
	Reason         string `json:"reason,omitempty"`
	Message        string `json:"message,omitempty"`
	Total          int    `json:"total"`
	Uploaded       int    `json:"uploaded"`
	RemoteVerified int    `json:"remote_verified"`
	Failed         int    `json:"failed"`
	Pending        int    `json:"pending"`
	// Derivatives counts gallery previews the server accepted (including ones
	// uploaded by an earlier pass); DerivativesFailed counts files whose
	// preview could not be produced or pushed. A preview failure never blocks
	// the original import.
	Derivatives       int `json:"derivatives"`
	DerivativesFailed int `json:"derivatives_failed"`
}

// stop carries a condition that pauses the whole run.
type stop struct {
	reason fsx.Reason
	msg    string
}

// Run drives one import to remote verification. It is safe to call again after
// a pause or a crash: registration is idempotent, uploaded parts are skipped by
// ETag and a finalized batch is only re-polled, never re-created.
func (u *Uploader) Run(ctx context.Context, importID string) (Result, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	res := Result{ImportID: importID}
	im, err := u.store.Import(ctx, importID)
	if err != nil {
		return res, err
	}
	res.BatchID = im.ServerBatchID
	if im.Status == model.ImportComplete {
		// The import itself is done, but a preview backfill after an agent
		// upgrade still has work to do.
		return u.finish(ctx, im, res)
	}
	if im.Status != model.ImportLocalComplete && im.Status != model.ImportUploading {
		return res, ErrNotLocalComplete
	}
	if strings.TrimSpace(im.ProjectID) == "" {
		return res, ErrProjectRequired
	}
	if u.deviceID == "" {
		return res, ErrNoDevice
	}
	card, err := u.store.Card(ctx, im.CardID)
	if err != nil {
		return res, fmt.Errorf("upload: 读取存储卡信息: %w", err)
	}
	if _, err := u.store.SetImportStatus(ctx, im.ID,
		[]model.ImportStatus{model.ImportLocalComplete, model.ImportUploading},
		model.ImportUploading); err != nil {
		return res, err
	}
	if err := u.store.SetPauseReason(ctx, im.ID, ""); err != nil {
		return res, err
	}

	if im.ServerBatchID == "" {
		id, err := u.createBatch(ctx, im, card.Fingerprint, "")
		if err != nil {
			return u.fatal(ctx, im, err, res)
		}
		im.ServerBatchID = id
		res.BatchID = id
		u.emit(im.ID, "info", "upload.batch_created", "已创建服务器采集批次，开始登记资产。")
	}

	repairRounds := 0
	for {
		// Phase A: register and transfer everything that still needs bytes on
		// the server. A sealed batch is replaced by a repair batch only after it
		// refuses a change, so healthy finalized batches are never duplicated.
		sealed := false
		for {
			files, err := u.store.Files(ctx, im.ID)
			if err != nil {
				return res, err
			}
			need := needingRemote(files, repairRounds < maxRepairRounds)
			if len(need) == 0 {
				break
			}
			if sealed {
				if repairRounds >= maxRepairRounds {
					return u.finish(ctx, im, res)
				}
				id, err := u.createBatch(ctx, im, card.Fingerprint, im.ServerBatchID)
				if err != nil {
					return u.fatal(ctx, im, err, res)
				}
				im.ServerBatchID = id
				res.BatchID = id
				repairRounds++
				sealed = false
				u.emit(im.ID, "warning", "upload.repair_batch",
					fmt.Sprintf("上一批次已封存，缺少或损坏的文件将通过新批次 %s 重新登记。", id))
			}
			batchID := im.ServerBatchID
			if err := u.register(ctx, im, batchID, need); err != nil {
				if errors.Is(err, errSealed) {
					sealed = true
					continue
				}
				return u.fatal(ctx, im, err, res)
			}

			files, err = u.store.Files(ctx, im.ID)
			if err != nil {
				return res, err
			}
			targets := uploadTargets(files, batchID)
			if len(targets) == 0 {
				continue
			}
			st, stopped, sealedNow := u.transferAll(ctx, im, targets)
			if sealedNow {
				sealed = true
				continue
			}
			if stopped != nil {
				return u.pause(ctx, im, stopped.reason, stopped.msg)
			}
			if st.failed > 0 {
				u.emit(im.ID, "warning", "upload.incomplete",
					fmt.Sprintf("本轮上传：%d 个文件成功，%d 个失败，可修复后重试。", st.ok, st.failed))
				return u.finish(ctx, im, res)
			}
		}

		// Phase B: seal the batch and wait for the server-side verification.
		files, err := u.store.Files(ctx, im.ID)
		if err != nil {
			return res, err
		}
		if len(candidatesInBatch(files, im.ServerBatchID)) == 0 {
			break
		}
		job, err := u.finalize(ctx, im.ServerBatchID)
		if err != nil {
			if errors.Is(err, errSealed) {
				u.emit(im.ID, "warning", "batch_closed",
					"服务器批次已封存且无法复核，请重新运行导入以创建新批次。")
				return u.finish(ctx, im, res)
			}
			return u.fatal(ctx, im, err, res)
		}
		if job == nil || job.Status != "succeeded" {
			u.emit(im.ID, "warning", "verify_failed",
				"服务端复核任务未成功完成，稍后可重试；已上传的对象不会重复上传。")
			return u.finish(ctx, im, res)
		}
		corrupt, err := u.applyVerify(ctx, im, im.ServerBatchID, job)
		if err != nil {
			return u.fatal(ctx, im, err, res)
		}
		if corrupt == 0 {
			break
		}
		if repairRounds >= maxRepairRounds {
			u.emit(im.ID, "error", "remote_corrupt",
				fmt.Sprintf("%d 个文件在服务端复核后仍不一致，已标记为需人工处理。", corrupt))
			break
		}
		repairRounds++
		id, err := u.createBatch(ctx, im, card.Fingerprint, im.ServerBatchID)
		if err != nil {
			return u.fatal(ctx, im, err, res)
		}
		im.ServerBatchID = id
		res.BatchID = id
		u.emit(im.ID, "warning", "upload.repair_batch",
			fmt.Sprintf("%d 个文件未通过服务端复核，已创建修复批次 %s。", corrupt, id))
	}

	counts, err := u.store.FileCounts(ctx, im.ID)
	if err != nil {
		return res, err
	}
	if counts.Total > 0 && counts.RemoteVerified == counts.Total {
		ok, err := u.store.SetImportStatus(ctx, im.ID,
			[]model.ImportStatus{model.ImportUploading}, model.ImportComplete)
		if err != nil {
			return res, err
		}
		if ok {
			u.emit(im.ID, "info", "import.complete",
				fmt.Sprintf("云端确认完成：%d 个文件的 SHA-256 已由服务端复核通过。", counts.RemoteVerified))
		}
	} else if counts.Uploaded > counts.RemoteVerified {
		res.Pending = counts.Uploaded - counts.RemoteVerified
		u.emit(im.ID, "warning", "remote_pending",
			fmt.Sprintf("%d 个文件已上传，但尚未取得服务端复核结果；下次运行会继续核对。", res.Pending))
	}
	return u.finish(ctx, im, res)
}

// finish uploads gallery previews for the assets verified so far, then reports
// the import counts. Preview failures are recorded per file and never turn a
// verified import into a failed one.
func (u *Uploader) finish(ctx context.Context, im *model.Import, res Result) (Result, error) {
	if u.previews != nil && ctx.Err() == nil {
		stats := u.uploadPreviews(ctx, im)
		res.Derivatives = stats.ok
		res.DerivativesFailed = stats.failed
	}
	return u.result(ctx, im, res)
}

// ---------- phases ----------

func (u *Uploader) createBatch(ctx context.Context, im *model.Import, cardFP, prevBatch string) (string, error) {
	key := "agent:batch:" + im.ID
	if prevBatch != "" {
		key += ":" + prevBatch
	}
	b, err := u.client.CreateBatch(ctx, key, api.CreateBatchRequest{
		ProjectID: im.ProjectID, DeviceID: u.deviceID, CardFingerprint: cardFP,
	})
	if err != nil {
		return "", err
	}
	if b.ID == "" {
		return "", errors.New("upload: 服务器未返回批次编号")
	}
	if err := u.store.SetImportServerBatchID(ctx, im.ID, b.ID); err != nil {
		return "", err
	}
	return b.ID, nil
}

// register sends every file that still needs remote work through the batch
// registration endpoint in chunks bounded by both the file count and the JSON
// body size. Registration doubles as reconciliation: the server reports each
// asset's current state, including duplicates verified by another import.
func (u *Uploader) register(ctx context.Context, im *model.Import, batchID string, files []model.File) error {
	offset := 0
	for offset < len(files) {
		chunk, inputs, n := chunkInputs(files[offset:], maxFilesPerRequest, maxRegisterBytes)
		if n == 0 {
			return fmt.Errorf("upload: 文件 %s 的元数据过大，无法登记", files[offset].RelPath)
		}
		err := u.registerChunk(ctx, im, batchID, chunk, inputs)
		if err != nil {
			if errors.Is(err, errSealed) {
				return err
			}
			if api.Is(err, 422, "invalid_file") && n > 1 {
				// Isolate the rejected entry so one bad row cannot block a
				// multi-thousand file import.
				for i := range chunk {
					one := chunk[i : i+1]
					if rerr := u.registerChunk(ctx, im, batchID, one, inputs[i:i+1]); rerr != nil {
						if errors.Is(rerr, errSealed) {
							return rerr
						}
						u.failFile(ctx, im, one[0], rerr)
					}
				}
				offset += n
				continue
			}
			if api.Is(err, 422, "invalid_file") {
				u.failFile(ctx, im, chunk[0], err)
				offset += n
				continue
			}
			return err
		}
		offset += n
	}
	return nil
}

func (u *Uploader) registerChunk(ctx context.Context, im *model.Import, batchID string,
	files []model.File, inputs []api.FileInput) error {
	result, err := u.client.RegisterFiles(ctx, batchID,
		fmt.Sprintf("agent:reg:%s:%s", batchID, chunkHash(files)), inputs)
	if err != nil {
		if api.Is(err, 409, "batch_closed") {
			return errSealed
		}
		return err
	}
	return u.applyRegistrations(ctx, batchID, files, result)
}

func (u *Uploader) applyRegistrations(ctx context.Context, batchID string,
	files []model.File, result *api.RegisterResult) error {
	byPath := make(map[string]model.File, len(files))
	for _, f := range files {
		byPath[f.RelPath] = f
	}
	from := []model.FileState{model.StateLocalVerified, model.StateRegistered,
		model.StateUploading, model.StateUploadFailed, model.StateRemoteCorrupt}
	for _, a := range result.Assets {
		f, ok := byPath[a.RelPath]
		if !ok || a.AssetID == "" {
			continue
		}
		if err := u.store.SetFileAsset(ctx, f.ID, a.AssetID); err != nil {
			return err
		}
		if a.Duplicated && (a.Status == "uploaded" || a.Status == "verified") {
			// The asset lives in another batch. Do not relink it and do not
			// claim verification that this batch will never perform.
			if a.Status == "verified" {
				if _, err := u.store.SetFileState(ctx, f.ID, from,
					model.StateRemoteVerified, ""); err != nil {
					return err
				}
				continue
			}
			if _, err := u.store.SetFileState(ctx, f.ID,
				append(from, model.StateUploaded), model.StateUploaded, ""); err != nil {
				return err
			}
			continue
		}
		if err := u.store.SetFileRemoteBatch(ctx, f.ID, batchID); err != nil {
			return err
		}
		if _, err := u.store.SetFileState(ctx, f.ID, from, model.StateRegistered, ""); err != nil {
			return err
		}
	}
	return nil
}

type transferStats struct {
	ok     int
	failed int
}

// transferAll uploads the given files with a small worker pool. A pause
// condition cancels the pass; a sealed batch and per-file failures are reported
// separately so the caller can react to each.
func (u *Uploader) transferAll(ctx context.Context, im *model.Import, files []model.File) (transferStats, *stop, bool) {
	var (
		stats  transferStats
		mu     sync.Mutex
		first  *stop
		sealed bool
	)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan model.File)
	var wg sync.WaitGroup
	for i := 0; i < u.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if runCtx.Err() != nil {
					continue
				}
				err := u.transferOne(runCtx, f)
				if err == nil {
					mu.Lock()
					stats.ok++
					mu.Unlock()
					continue
				}
				if runCtx.Err() != nil {
					continue
				}
				switch {
				case errors.Is(err, errBatchImmutable):
					// Another writer stored the bytes; verification will judge it.
					_, _ = u.store.SetFileState(runCtx, f.ID,
						[]model.FileState{model.StateRegistered, model.StateUploading, model.StateUploadFailed},
						model.StateUploaded, "")
					mu.Lock()
					stats.ok++
					mu.Unlock()
				case errors.Is(err, errSealed):
					mu.Lock()
					sealed = true
					mu.Unlock()
					cancel()
				default:
					if s := stopFor(err); s != nil {
						mu.Lock()
						if first == nil {
							first = s
						}
						mu.Unlock()
						cancel()
						continue
					}
					mu.Lock()
					stats.failed++
					mu.Unlock()
					u.failFile(ctx, im, f, err)
				}
			}
		}()
	}

feed:
	for _, f := range files {
		select {
		case jobs <- f:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return stats, first, sealed
}

func (u *Uploader) transferOne(ctx context.Context, f model.File) error {
	if f.ServerAssetID == "" {
		return permanentError("缺少服务器资产编号，请重新登记")
	}
	if f.SHA256 == "" {
		return permanentError("缺少本地校验哈希，不能上传")
	}
	path, err := u.source(ctx, f)
	if err != nil {
		return permanentError(err.Error())
	}
	plan, err := u.store.Upload(ctx, f.ID)
	if errors.Is(err, store.ErrNotFound) {
		plan = nil
	} else if err != nil {
		return err
	}
	if plan != nil && plan.State == model.UploadCompleted {
		_, err := u.store.SetFileState(ctx, f.ID,
			[]model.FileState{model.StateRegistered, model.StateUploading, model.StateUploadFailed},
			model.StateUploaded, "")
		return err
	}

	epoch := 0
	if plan != nil {
		epoch = plan.Epoch
	}
	for attempt := 1; attempt <= presignAttempts; attempt++ {
		epoch++
		p, err := u.presign(ctx, f, epoch)
		if err != nil {
			return err
		}
		if err := u.transfer(ctx, f, path, p, epoch); err != nil {
			if errors.Is(err, errPresignExpired) {
				continue
			}
			return err
		}
		if err := u.store.SaveUpload(ctx, model.Upload{
			FileID: f.ID, AssetID: p.AssetID, Mode: p.Mode, ObjectKey: p.ObjectKey,
			UploadID: p.UploadID, PartSize: p.PartSize, TotalParts: totalParts(p),
			Epoch: epoch, ExpiresAt: &p.ExpiresAt, State: model.UploadCompleted,
		}); err != nil {
			return err
		}
		_, err = u.store.SetFileState(ctx, f.ID,
			[]model.FileState{model.StateRegistered, model.StateUploading, model.StateUploadFailed},
			model.StateUploaded, "")
		return err
	}
	return permanentError("预签名上传地址多次失效，请稍后重试")
}

func (u *Uploader) presign(ctx context.Context, f model.File, epoch int) (*api.UploadPlan, error) {
	plan, err := u.client.Presign(ctx,
		fmt.Sprintf("agent:presign:%s:%d", f.ServerAssetID, epoch),
		api.PresignRequest{AssetID: f.ServerAssetID, Size: f.Size})
	if err != nil {
		switch {
		case api.Is(err, 409, "batch_closed"):
			return nil, errSealed
		case api.Is(err, 409, "asset_immutable"):
			return nil, errBatchImmutable
		}
		return nil, err
	}
	prev, perr := u.store.Upload(ctx, f.ID)
	if perr != nil && !errors.Is(perr, store.ErrNotFound) {
		return nil, perr
	}
	if prev != nil && (prev.UploadID != plan.UploadID || prev.PartSize != plan.PartSize ||
		prev.Mode != plan.Mode) {
		// A fresh multipart session invalidates every ETag we kept.
		if err := u.store.ResetUploadParts(ctx, f.ID); err != nil {
			return nil, err
		}
	}
	if err := u.store.SaveUpload(ctx, model.Upload{
		FileID: f.ID, AssetID: plan.AssetID, Mode: plan.Mode, ObjectKey: plan.ObjectKey,
		UploadID: plan.UploadID, PartSize: plan.PartSize, TotalParts: totalParts(plan),
		Epoch: epoch, ExpiresAt: &plan.ExpiresAt, State: model.UploadActive,
	}); err != nil {
		return nil, err
	}
	return plan, nil
}

func totalParts(p *api.UploadPlan) int32 {
	if p.Mode != "multipart" || p.PartSize <= 0 {
		return 0
	}
	return int32(len(p.Parts))
}

func (u *Uploader) transfer(ctx context.Context, f model.File, path string, plan *api.UploadPlan, epoch int) error {
	if plan.Mode == "single" {
		if _, err := u.client.PutObject(ctx, plan.URL, f.Size, openFile(path)); err != nil {
			return classifyPut(err)
		}
		return u.complete(ctx, f, plan, epoch)
	}
	if err := u.transferParts(ctx, f, path, plan); err != nil {
		return err
	}
	return u.complete(ctx, f, plan, epoch)
}

func (u *Uploader) transferParts(ctx context.Context, f model.File, path string, plan *api.UploadPlan) error {
	if plan.UploadID == "" || plan.PartSize <= 0 || len(plan.Parts) == 0 {
		return permanentError("服务器返回的分片上传计划不完整")
	}
	urls := make(map[int32]string, len(plan.Parts))
	for _, p := range plan.Parts {
		urls[p.PartNumber] = p.URL
	}
	done, err := u.store.UploadParts(ctx, f.ID)
	if err != nil {
		return err
	}
	uploaded := make(map[int32]string, len(done))
	for _, p := range done {
		if p.State == model.PartUploaded && p.ETag != "" {
			uploaded[p.Number] = p.ETag
		}
	}
	total := totalParts(plan)
	for n := int32(1); n <= total; n++ {
		if _, ok := uploaded[n]; ok {
			continue
		}
		offset := int64(n-1) * plan.PartSize
		size := plan.PartSize
		if remain := f.Size - offset; remain < size {
			size = remain
		}
		if size <= 0 {
			return permanentError("分片计划与实际文件大小不一致")
		}
		rawURL, ok := urls[n]
		if !ok {
			return errPresignExpired
		}
		etag, err := u.client.PutObject(ctx, rawURL, size, openSection(path, offset, size))
		if err != nil {
			return classifyPut(err)
		}
		uploaded[n] = etag
		if err := u.store.SaveUploadPart(ctx, model.UploadPart{
			FileID: f.ID, Number: n, Offset: offset, Size: size,
			ETag: etag, State: model.PartUploaded,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (u *Uploader) complete(ctx context.Context, f model.File, plan *api.UploadPlan, epoch int) error {
	req := api.CompleteUploadRequest{}
	if plan.Mode == "multipart" {
		parts, err := u.store.UploadParts(ctx, f.ID)
		if err != nil {
			return err
		}
		list := make([]api.CompletePart, 0, len(parts))
		for _, p := range parts {
			if p.State != model.PartUploaded || p.ETag == "" {
				return errPresignExpired
			}
			list = append(list, api.CompletePart{PartNumber: p.Number, ETag: p.ETag})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].PartNumber < list[j].PartNumber })
		if len(list) == 0 || int32(len(list)) != totalParts(plan) {
			return errPresignExpired
		}
		req.UploadID = plan.UploadID
		req.Parts = list
	}
	_, err := u.client.CompleteUpload(ctx, f.ServerAssetID,
		fmt.Sprintf("agent:complete:%s:%d", f.ServerAssetID, epoch), req)
	if err != nil {
		switch {
		case api.Is(err, 409, "batch_closed"):
			return errSealed
		case api.Is(err, 422, "upload_missing"):
			// The object is not visible yet; re-presign and send it again.
			return errPresignExpired
		case api.Is(err, 409, "asset_immutable"):
			return nil
		}
		return err
	}
	return nil
}

// ---------- verification ----------

func (u *Uploader) finalize(ctx context.Context, batchID string) (*api.Job, error) {
	job, err := u.client.FinalizeBatch(ctx, batchID, "agent:finalize:"+batchID)
	if err != nil {
		if api.Is(err, 409, "batch_closed") {
			return nil, errSealed
		}
		return nil, err
	}
	if job.ID == "" {
		return nil, errors.New("upload: 服务器未返回复核任务编号")
	}
	deadline := time.Now().Add(u.pollTTL)
	wait := u.pollBase
	for job.Status != "succeeded" && job.Status != "failed" {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("upload: 服务端复核超时（%s），稍后可重试", u.pollTTL)
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
		if wait < u.pollMax {
			wait *= 2
			if wait > u.pollMax {
				wait = u.pollMax
			}
		}
		job, err = u.client.Job(ctx, job.ID)
		if err != nil {
			return nil, err
		}
	}
	return job, nil
}

// applyVerify maps the verification result back onto the files this batch
// covers. The job reports aggregate counts, so exact per-asset statuses are
// read from the project asset list and matched by asset id. When that list is
// unavailable nothing is promoted: a file is never marked verified on a guess.
func (u *Uploader) applyVerify(ctx context.Context, im *model.Import, batchID string, job *api.Job) (int, error) {
	files, err := u.store.Files(ctx, im.ID)
	if err != nil {
		return 0, err
	}
	candidates := candidatesInBatch(files, batchID)
	if len(candidates) == 0 {
		return 0, nil
	}
	res, ok := api.ParseVerifyResult(job)
	if ok && res.Verified == len(candidates) && res.Missing == 0 && res.Corrupt == 0 {
		verified := 0
		for _, f := range candidates {
			done, err := u.store.SetFileState(ctx, f.ID,
				[]model.FileState{model.StateUploaded, model.StateUploading},
				model.StateRemoteVerified, "")
			if err != nil {
				return 0, err
			}
			if done {
				verified++
			}
		}
		u.emit(im.ID, "info", "upload.verified",
			fmt.Sprintf("服务端复核通过：%d 个文件 SHA-256 一致。", verified))
		return 0, nil
	}

	statuses, err := u.assetStatuses(ctx, im.ProjectID, candidates)
	if err != nil {
		u.emit(im.ID, "warning", "verify_result_unknown",
			"服务端复核结果无法逐条对应，文件保持已上传状态，下次运行继续核对："+err.Error())
		return 0, nil
	}
	corrupt := 0
	for _, f := range candidates {
		switch statuses[f.ServerAssetID] {
		case "verified":
			if _, err := u.store.SetFileState(ctx, f.ID,
				[]model.FileState{model.StateUploaded, model.StateUploading},
				model.StateRemoteVerified, ""); err != nil {
				return corrupt, err
			}
		case "missing", "corrupt":
			if _, err := u.store.SetFileState(ctx, f.ID,
				[]model.FileState{model.StateUploaded, model.StateUploading},
				model.StateRemoteCorrupt, "服务端复核未通过（"+statuses[f.ServerAssetID]+"）"); err != nil {
				return corrupt, err
			}
			// The old transfer plan claims success; drop it so the repair round
			// sends the bytes again instead of trusting the stale plan.
			if err := u.store.ResetUpload(ctx, f.ID); err != nil {
				return corrupt, err
			}
			corrupt++
		}
	}
	if corrupt > 0 {
		u.emit(im.ID, "warning", "verify_partial",
			fmt.Sprintf("服务端复核：%d 个文件缺失或哈希不一致，将尝试重新登记上传。", corrupt))
	}
	return corrupt, nil
}

// assetStatuses pages the project asset list until every candidate asset id is
// resolved. The bound keeps a very large project from stalling the import.
func (u *Uploader) assetStatuses(ctx context.Context, projectID string, candidates []model.File) (map[string]string, error) {
	want := make(map[string]struct{}, len(candidates))
	for _, f := range candidates {
		if f.ServerAssetID != "" {
			want[f.ServerAssetID] = struct{}{}
		}
	}
	out := make(map[string]string, len(want))
	const pageSize, maxPages = 200, 250
	for page := 0; page < maxPages && len(out) < len(want); page++ {
		items, _, err := u.client.ListAssets(ctx, projectID, pageSize, page*pageSize)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for _, a := range items {
			if _, ok := want[a.ID]; ok {
				out[a.ID] = a.Status
			}
		}
		if len(items) < pageSize {
			break
		}
	}
	if len(out) < len(want) {
		return nil, fmt.Errorf("仅匹配到 %d/%d 个资产状态", len(out), len(want))
	}
	return out, nil
}

// ---------- gallery previews ----------

var derivativeRetryStates = []string{
	model.DerivativePending, model.DerivativeGenerated,
	model.DerivativeUploading, model.DerivativeFailed,
}

type previewStats struct {
	ok     int
	failed int
}

// uploadPreviews pushes the gallery JPEG of every remotely verified photo and
// video. The derivative rows make the pass resumable: accepted previews are
// skipped, failed ones are retried on the next run, and a preview problem never
// changes the import's own state machine.
func (u *Uploader) uploadPreviews(ctx context.Context, im *model.Import) previewStats {
	var stats previewStats
	files, err := u.store.Files(ctx, im.ID)
	if err != nil {
		u.emit(im.ID, "warning", "derivative_scan_failed", "无法读取预览任务："+err.Error())
		return stats
	}
	done := map[string]bool{}
	if rows, err := u.store.Derivatives(ctx, im.ID); err == nil {
		for _, d := range rows {
			if d.Kind == model.DerivativeKindGallery && d.State == model.DerivativeUploaded {
				done[d.FileID] = true
			}
		}
	}
	stats.ok = len(done)
	pending := make([]model.File, 0, len(files))
	for _, f := range files {
		if needsPreview(f) && f.ServerAssetID != "" && !done[f.ID] {
			pending = append(pending, f)
		}
	}
	if len(pending) == 0 {
		return stats
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := u.workers
	if workers > 2 {
		// Each worker decodes a 2560px JPEG; two keep the CPU and RAM bounded.
		workers = 2
	}
	var (
		mu      sync.Mutex
		stopHit bool
	)
	jobs := make(chan model.File)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if runCtx.Err() != nil {
					continue
				}
				err := u.uploadPreview(runCtx, f)
				if err == nil {
					mu.Lock()
					stats.ok++
					mu.Unlock()
					continue
				}
				if runCtx.Err() != nil {
					continue
				}
				if stopFor(err) != nil {
					mu.Lock()
					stopHit = true
					mu.Unlock()
					cancel()
					continue
				}
				mu.Lock()
				stats.failed++
				mu.Unlock()
				u.emit(im.ID, "warning", "derivative_upload_failed", f.RelPath+"："+err.Error())
			}
		}()
	}
feed:
	for _, f := range pending {
		select {
		case jobs <- f:
		case <-runCtx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()

	level := "info"
	if stats.failed > 0 || stopHit {
		level = "warning"
	}
	msg := fmt.Sprintf("图库预览：%d 个已就绪，%d 个失败。", stats.ok, stats.failed)
	if stopHit {
		msg = "图库预览上传因服务器不可达或登录失效而暂停，下次运行会从断点继续。" +
			fmt.Sprintf("已就绪 %d 个，失败 %d 个。", stats.ok, stats.failed)
	}
	u.emit(im.ID, level, "upload.derivatives", msg)
	return stats
}

// uploadPreview renders, uploads and confirms one gallery preview. The durable
// row walks pending -> generating -> uploading -> uploaded; a failure leaves a
// failed row with the reason so the next pass can retry it.
func (u *Uploader) uploadPreview(ctx context.Context, f model.File) error {
	if _, err := u.store.Derivative(ctx, f.ID, model.DerivativeKindGallery); errors.Is(err, store.ErrNotFound) {
		if err := u.store.SaveDerivative(ctx, model.Derivative{
			FileID: f.ID, Kind: model.DerivativeKindGallery, State: model.DerivativePending,
		}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	prev, err := u.previews.Gallery(ctx, f)
	if err != nil {
		u.markPreviewFailed(ctx, f, err)
		return err
	}
	if err := u.store.SaveDerivative(ctx, model.Derivative{
		FileID: f.ID, Kind: model.DerivativeKindGallery,
		Path: prev.Path, SHA256: prev.SHA256, Size: prev.Bytes,
		Width: prev.Width, Height: prev.Height,
		State: model.DerivativeGenerated,
	}); err != nil {
		return err
	}
	if _, err := u.store.SetDerivativeState(ctx, f.ID, model.DerivativeKindGallery,
		derivativeRetryStates, model.DerivativeUploading, ""); err != nil {
		return err
	}
	if err := u.pushPreview(ctx, f, prev); err != nil {
		u.markPreviewFailed(ctx, f, err)
		return err
	}
	_, err = u.store.SetDerivativeState(ctx, f.ID, model.DerivativeKindGallery,
		[]string{model.DerivativeUploading}, model.DerivativeUploaded, "")
	return err
}

// pushPreview PUTs the rendered JPEG to the content-addressed key and asks the
// server to re-hash it. The object key is derived from the preview hash, so
// repeating the upload is harmless; the idempotency keys carry a per-run nonce
// because a cached signature or error from an earlier run must never be
// replayed against a different attempt.
func (u *Uploader) pushPreview(ctx context.Context, f model.File, prev *preview.GalleryPreview) error {
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	base := fmt.Sprintf("agent:deriv:%s:%s:%s", f.ServerAssetID, prev.SHA256, run)
	for attempt := 1; attempt <= presignAttempts; attempt++ {
		plan, err := u.client.PresignDerivative(ctx,
			fmt.Sprintf("%s:presign:%d", base, attempt),
			api.DerivativePresignRequest{
				AssetID: f.ServerAssetID,
				Kind:    model.DerivativeKindGallery,
				Size:    prev.Bytes,
				SHA256:  prev.SHA256,
			})
		if err != nil {
			return err
		}
		if _, err := u.client.PutObject(ctx, plan.URL, prev.Bytes, openFile(prev.Path)); err != nil {
			putErr := classifyPut(err)
			if errors.Is(putErr, errPresignExpired) {
				continue
			}
			return putErr
		}
		_, err = u.client.CompleteDerivative(ctx, f.ServerAssetID,
			fmt.Sprintf("%s:complete:%d", base, attempt),
			api.DerivativeCompleteRequest{
				Kind:   model.DerivativeKindGallery,
				Size:   prev.Bytes,
				SHA256: prev.SHA256,
				Width:  prev.Width,
				Height: prev.Height,
			})
		if err != nil {
			if api.Is(err, 422, "derivative_missing") {
				// The object is not visible yet; sign again and PUT.
				continue
			}
			return err
		}
		return nil
	}
	return permanentError("图库预览的预签名地址多次失效，请稍后重试")
}

func (u *Uploader) markPreviewFailed(ctx context.Context, f model.File, err error) {
	if ctx.Err() != nil {
		// A pause leaves the row as-is so the next run resumes it.
		return
	}
	_, _ = u.store.SetDerivativeState(ctx, f.ID, model.DerivativeKindGallery,
		derivativeRetryStates, model.DerivativeFailed, err.Error())
}

// needsPreview reports whether a file feeds the customer gallery.
func needsPreview(f model.File) bool {
	if f.State != model.StateRemoteVerified {
		return false
	}
	if f.Kind != model.KindPhoto && f.Kind != model.KindVideo {
		return false
	}
	return f.Role != model.RoleSidecar
}

// ---------- pause and failure helpers ----------

func (u *Uploader) fatal(ctx context.Context, im *model.Import, err error, res Result) (Result, error) {
	if s := stopFor(err); s != nil {
		return u.pause(ctx, im, s.reason, s.msg)
	}
	return res, err
}

func (u *Uploader) pause(ctx context.Context, im *model.Import, reason fsx.Reason, msg string) (Result, error) {
	if err := u.store.SetPauseReason(ctx, im.ID, string(reason)); err != nil {
		return Result{ImportID: im.ID}, err
	}
	u.emit(im.ID, "error", "upload_paused",
		fmt.Sprintf("上传已暂停（%s）：%s", reason, msg))
	counts, err := u.store.FileCounts(ctx, im.ID)
	if err != nil {
		return Result{ImportID: im.ID}, err
	}
	return Result{
		ImportID: im.ID, Paused: true, Reason: string(reason), Message: msg,
		Total: counts.Total, Uploaded: counts.Uploaded,
		RemoteVerified: counts.RemoteVerified, Failed: counts.Failed,
	}, nil
}

func (u *Uploader) failFile(ctx context.Context, im *model.Import, f model.File, err error) {
	_, _ = u.store.SetFileState(ctx, f.ID,
		[]model.FileState{model.StateRegistered, model.StateUploading, model.StateUploadFailed},
		model.StateUploadFailed, err.Error())
	u.emit(im.ID, "warning", "file_upload_failed", f.RelPath+"："+err.Error())
}

func (u *Uploader) result(ctx context.Context, im *model.Import, res Result) (Result, error) {
	counts, err := u.store.FileCounts(ctx, im.ID)
	if err != nil {
		return res, err
	}
	res.Total = counts.Total
	res.Uploaded = counts.Uploaded
	res.RemoteVerified = counts.RemoteVerified
	res.Failed = counts.Failed
	if counts.Uploaded > counts.RemoteVerified {
		res.Pending = counts.Uploaded - counts.RemoteVerified
	}
	return res, nil
}

func (u *Uploader) emit(importID, level, code, msg string) {
	e := model.Event{Level: level, Code: code, ImportID: importID, Message: msg}
	_, _ = u.store.AddEvent(context.Background(), e)
	if u.onEvent != nil {
		u.onEvent(e)
	}
}

// ---------- file selection ----------

// needingRemote lists files that still require registration or transfer work.
// Remote-corrupt files are only retried while repair rounds remain.
func needingRemote(files []model.File, repairAllowed bool) []model.File {
	out := make([]model.File, 0, len(files))
	for _, f := range files {
		switch f.State {
		case model.StateLocalVerified, model.StateRegistered,
			model.StateUploading, model.StateUploadFailed:
			out = append(out, f)
		case model.StateRemoteCorrupt:
			if repairAllowed {
				out = append(out, f)
			}
		}
	}
	return out
}

func uploadTargets(files []model.File, batchID string) []model.File {
	out := make([]model.File, 0, len(files))
	for _, f := range files {
		if f.RemoteBatchID != batchID || f.ServerAssetID == "" {
			continue
		}
		switch f.State {
		case model.StateRegistered, model.StateUploading, model.StateUploadFailed:
			out = append(out, f)
		}
	}
	return out
}

func candidatesInBatch(files []model.File, batchID string) []model.File {
	out := make([]model.File, 0, len(files))
	for _, f := range files {
		if f.RemoteBatchID == batchID && f.ServerAssetID != "" &&
			(f.State == model.StateUploaded || f.State == model.StateUploading) {
			out = append(out, f)
		}
	}
	return out
}

// ---------- chunking ----------

func chunkInputs(files []model.File, maxCount, maxBytes int) ([]model.File, []api.FileInput, int) {
	var (
		chunk  []model.File
		inputs []api.FileInput
		size   int
	)
	for _, f := range files {
		in := fileInput(f)
		est := estimateInput(in)
		if len(chunk) >= maxCount || (len(chunk) > 0 && size+est > maxBytes) {
			break
		}
		chunk = append(chunk, f)
		inputs = append(inputs, in)
		size += est
	}
	return chunk, inputs, len(chunk)
}

func estimateInput(in api.FileInput) int {
	n := len(in.RelPath) + len(in.SHA256) + len(in.Mime) + len(in.Camera) + len(in.Lens) + len(in.Exif) + 160
	if n < 256 {
		n = 256
	}
	return n
}

func fileInput(f model.File) api.FileInput {
	in := api.FileInput{
		RelPath: f.RelPath, Size: f.Size, SHA256: f.SHA256,
		Mime: f.Mime, Width: f.Width, Height: f.Height,
		CapturedAt: f.CapturedAt, Camera: f.Camera, Lens: f.Lens,
		IsVideo: f.Kind == model.KindVideo,
	}
	if len(f.ExifJSON) > 0 && json.Valid([]byte(f.ExifJSON)) {
		in.Exif = json.RawMessage(f.ExifJSON)
	}
	return in
}

func chunkHash(files []model.File) string {
	var b bytes.Buffer
	for _, f := range files {
		b.WriteString(f.RelPath)
		b.WriteByte(0)
		b.WriteString(f.SHA256)
		b.WriteByte(0)
		fmt.Fprintf(&b, "%d", f.Size)
		b.WriteByte(0)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:16])
}

// ---------- error classification ----------

type permanent struct{ msg string }

func (p *permanent) Error() string { return p.msg }

func permanentError(msg string) error { return &permanent{msg: msg} }

func stopFor(err error) *stop {
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrExpired) || errors.Is(err, auth.ErrLoginRequired) ||
		api.IsUnauthorized(err) {
		return &stop{reason: fsx.ReasonAuthRequired, msg: "登录状态已失效，请重新登录后继续。"}
	}
	if isOffline(err) {
		return &stop{reason: fsx.ReasonOffline, msg: "无法连接服务器：" + err.Error()}
	}
	return nil
}

func isOffline(err error) bool {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status >= 500 || ae.Status == 429 || ae.Status == 408
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

func classifyPut(err error) error {
	if err == nil {
		return nil
	}
	if api.Is(err, 403, "presign_expired") || api.Is(err, 400, "presign_expired") ||
		api.Is(err, 404, "presign_expired") {
		return errPresignExpired
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if stopFor(err) != nil {
		// Keep the original error so the pass pauses (offline) instead of
		// condemning the file permanently.
		return err
	}
	var ae *api.Error
	if errors.As(err, &ae) && ae.Permanent() {
		return &permanent{msg: ae.Error()}
	}
	return &permanent{msg: "上传失败：" + err.Error()}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------- source files ----------

func (u *Uploader) defaultSource(ctx context.Context, f model.File) (string, error) {
	copies, err := u.store.Copies(ctx, f.ID)
	if err != nil {
		return "", err
	}
	var fallback string
	for _, c := range copies {
		if c.FinalPath == "" {
			continue
		}
		if c.Dest == model.DestA {
			if c.State == model.CopyVerified {
				return c.FinalPath, nil
			}
			fallback = c.FinalPath
		}
		if c.Dest == model.DestB && c.State == model.CopyVerified && fallback == "" {
			fallback = c.FinalPath
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", errors.New("找不到已校验的本地副本")
}

func openFile(path string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return os.Open(path) }
}

func openSection(path string, offset, size int64) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		fh, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{io.NewSectionReader(fh, offset, size), fh}, nil
	}
}
