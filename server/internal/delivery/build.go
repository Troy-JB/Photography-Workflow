package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/alert"
	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/db"
	"photography-workflow/server/internal/jobs"
	"photography-workflow/server/internal/media"
	"photography-workflow/server/internal/outbox"
	"photography-workflow/server/internal/storage"
)

const (
	// maxWebSource bounds how much of an original a render job reads into
	// memory. It matches media.maxSourceBytes; larger files are skipped for the
	// web package and still travel in the original package.
	maxWebSource = 80 << 20
	// tempPrefix names delivery spools so the worker can clean up crashes.
	tempPrefix = "pwf-delivery-"
	tempMaxAge = 24 * time.Hour
)

// AlertBuildFailed is the alert code raised when a build stops permanently.
const AlertBuildFailed = "delivery_build_failed"

// BuildPayload is the job payload shared by build, render and package jobs.
// Only the fields relevant to the kind are set.
type BuildPayload struct {
	DeliveryID   uuid.UUID `json:"delivery_id"`
	BuildVersion int       `json:"build_version"`
	ItemID       uuid.UUID `json:"item_id,omitempty"`
	PackageID    uuid.UUID `json:"package_id,omitempty"`
	Spec         string    `json:"spec,omitempty"`
}

// BuildResult is the job result recorded on success.
type BuildResult struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
	Version    int       `json:"build_version"`
	Status     string    `json:"status"`
	Items      int       `json:"items,omitempty"`
	Packages   int       `json:"packages,omitempty"`
}

// RenderResult reports one item's web render outcome.
type RenderResult struct {
	DeliveryID   uuid.UUID `json:"delivery_id"`
	ItemID       uuid.UUID `json:"item_id"`
	BuildVersion int       `json:"build_version"`
	Status       string    `json:"status"`
	WarningCode  string    `json:"web_warning_code,omitempty"`
}

// PackageResult reports one built ZIP.
type PackageResult struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
	PackageID  uuid.UUID `json:"package_id"`
	Version    int       `json:"build_version"`
	Spec       string    `json:"spec"`
	Status     string    `json:"status"`
	Size       int64     `json:"size_bytes,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	Entries    int       `json:"entry_count,omitempty"`
}

// PlanBuild snapshots the approved round's items, creates the two package rows
// and queues one render job per item. It is safe to retry: deleted item ids make
// stale render jobs no-ops, and package rows are upserted.
func (s *Service) PlanBuild(ctx context.Context, tenantID, deliveryID uuid.UUID, version int) (*BuildResult, error) {
	var (
		res     *BuildResult
		oldKeys []string
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			status        string
			buildVersion  int
			proofRoundID  uuid.UUID
			projectCode   string
			projectState  string
			paymentStatus string
			roundStatus   string
		)
		err := tx.QueryRow(ctx, `
			SELECT d.status, d.build_version, d.proof_round_id, p.code,
			       CASE WHEN p.state NOT IN ('cancelled','archived')
			         AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
			         AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
			         THEN 'cancelled' ELSE p.state END,
			       p.payment_status, r.status
			FROM deliveries d
			JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
			JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
			WHERE d.tenant_id = $1 AND d.id = $2
			FOR UPDATE OF d`, tenantID, deliveryID).
			Scan(&status, &buildVersion, &proofRoundID, &projectCode, &projectState,
				&paymentStatus, &roundStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("load delivery for build: %w", err)
		}
		res = &BuildResult{DeliveryID: deliveryID, Version: version, Status: status}
		if buildVersion != version || status != StatusBuilding {
			res.Status = "stale"
			return nil
		}
		if paymentStatus != "paid" {
			res.Status = StatusBlocked
			if _, err := tx.Exec(ctx, `
				UPDATE deliveries SET status = 'blocked', blocked_reason = $3, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenantID, deliveryID, BlockedReasonPayment); err != nil {
				return fmt.Errorf("block delivery: %w", err)
			}
			return nil
		}
		if err := checkProjectState(projectState); err != nil {
			res.Status = StatusFailed
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version, err.Error())
		}
		if roundStatus != "approved" {
			res.Status = StatusFailed
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
				"proof round is no longer approved")
		}

		oldKeys, err = s.prepareRebuild(ctx, tx, tenantID, deliveryID, version)
		if err != nil {
			return err
		}

		type assetRow struct {
			AssetID     uuid.UUID
			Position    int
			Filename    string
			Mime        string
			OriginalKey string
			SizeBytes   int64
			ContentHash string
			RevisionID  *uuid.UUID
			RevisionNo  int
			RevisionKey string
			RevisionSHA string
			RevisionLen int64
		}
		rows, err := tx.Query(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (asset_id) asset_id, id, revision_no, source_key, sha256, size_bytes
				FROM asset_revisions
				WHERE tenant_id = $1 AND status = 'derivative_ready'
				ORDER BY asset_id, revision_no DESC
			)
			SELECT i.asset_id, i.position, a.filename, a.mime, a.original_key,
			       a.size_bytes, a.content_hash,
			       latest.id, coalesce(latest.revision_no, 0), coalesce(latest.source_key, ''),
			       coalesce(latest.sha256, ''), coalesce(latest.size_bytes, 0)
			FROM proof_round_items i
			JOIN assets a ON a.tenant_id = i.tenant_id AND a.id = i.asset_id
			LEFT JOIN latest ON latest.asset_id = i.asset_id
			WHERE i.tenant_id = $1 AND i.proof_round_id = $2
			ORDER BY i.position`, tenantID, proofRoundID)
		if err != nil {
			return fmt.Errorf("load delivery items: %w", err)
		}
		var assets []assetRow
		for rows.Next() {
			var a assetRow
			if err := rows.Scan(&a.AssetID, &a.Position, &a.Filename, &a.Mime, &a.OriginalKey,
				&a.SizeBytes, &a.ContentHash, &a.RevisionID, &a.RevisionNo, &a.RevisionKey,
				&a.RevisionSHA, &a.RevisionLen); err != nil {
				rows.Close()
				return fmt.Errorf("scan delivery item: %w", err)
			}
			assets = append(assets, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("load delivery items: %w", err)
		}
		if len(assets) == 0 {
			res.Status = StatusFailed
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
				"proof round has no assets to deliver")
		}
		if len(assets) > s.cfg.MaxAssets {
			res.Status = StatusFailed
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
				fmt.Sprintf("delivery has %d assets; the limit is %d", len(assets), s.cfg.MaxAssets))
		}

		for _, a := range assets {
			name := a.Filename
			mimeType := a.Mime
			sourceKey := a.OriginalKey
			sourceSHA := a.ContentHash
			sourceSize := a.SizeBytes
			if a.RevisionID != nil {
				if strings.TrimSpace(a.RevisionKey) == "" || a.RevisionSHA == "" || a.RevisionLen <= 0 {
					res.Status = StatusFailed
					return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
						fmt.Sprintf("revision %d of %s has no stored source", a.RevisionNo, a.Filename))
				}
				sourceKey = a.RevisionKey
				sourceSHA = a.RevisionSHA
				sourceSize = a.RevisionLen
				mimeType = revisionMime(a.RevisionKey)
				if ext := path.Ext(a.RevisionKey); ext != "" {
					if stem := fileStem(a.Filename); stem != "" {
						name = stem + ext
					} else {
						name = "file" + ext
					}
				}
			}
			if sourceKey == "" {
				res.Status = StatusFailed
				return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
					fmt.Sprintf("asset %s has no stored original", a.Filename))
			}
			var itemID uuid.UUID
			if err := tx.QueryRow(ctx, `
				INSERT INTO delivery_items
					(tenant_id, delivery_id, asset_id, position, original_object_key, original_name,
					 original_mime, original_size, original_sha256, revision_id, revision_no)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
				RETURNING id`,
				tenantID, deliveryID, a.AssetID, a.Position, sourceKey, name, mimeType,
				sourceSize, sourceSHA, a.RevisionID, a.RevisionNo).Scan(&itemID); err != nil {
				return fmt.Errorf("insert delivery item: %w", err)
			}
			if _, err := jobs.Enqueue(ctx, tx, tenantID, JobRenderWeb, ItemSubject, &itemID,
				BuildPayload{DeliveryID: deliveryID, BuildVersion: version, ItemID: itemID}); err != nil {
				return err
			}
		}

		for _, spec := range []string{SpecWeb, SpecOriginal} {
			if _, err := tx.Exec(ctx, `
				INSERT INTO delivery_packages (tenant_id, delivery_id, spec, version, status)
				VALUES ($1,$2,$3,$4,'pending')
				ON CONFLICT (delivery_id, spec, version) DO NOTHING`,
				tenantID, deliveryID, spec, version); err != nil {
				return fmt.Errorf("insert delivery package: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deliveries SET item_count = $3, total_bytes = 0, updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, deliveryID, len(assets)); err != nil {
			return fmt.Errorf("update delivery summary: %w", err)
		}
		res.Items = len(assets)
		res.Packages = 2
		res.Status = StatusBuilding
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.deleteObjects(ctx, tenantID, oldKeys)
	return res, nil
}

// prepareRebuild removes item snapshots and package rows older than the keep
// window (current version minus one). The previous version stays on disk for
// support; anything older is garbage. It returns object keys to delete after
// the transaction commits.
func (s *Service) prepareRebuild(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID, version int) ([]string, error) {
	keepFrom := version - 1
	if keepFrom < 1 {
		keepFrom = 1
	}
	var keys []string
	if version > 2 {
		rows, err := tx.Query(ctx, `
			SELECT object_key FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2 AND version < $3
			  AND object_key <> ''`, tenantID, deliveryID, keepFrom)
		if err != nil {
			return nil, fmt.Errorf("list stale packages: %w", err)
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan stale package: %w", err)
			}
			keys = append(keys, key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2 AND version < $3`,
			tenantID, deliveryID, keepFrom); err != nil {
			return nil, fmt.Errorf("delete stale packages: %w", err)
		}
	}

	rows, err := tx.Query(ctx, `
		SELECT web_object_key FROM delivery_items
		WHERE tenant_id = $1 AND delivery_id = $2 AND web_object_key <> ''`,
		tenantID, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("list stale web objects: %w", err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stale web object: %w", err)
		}
		if v := keyVersion(key, deliveryID); v > 0 && v < keepFrom {
			keys = append(keys, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM delivery_items WHERE tenant_id = $1 AND delivery_id = $2`,
		tenantID, deliveryID); err != nil {
		return nil, fmt.Errorf("reset delivery items: %w", err)
	}
	return keys, nil
}

// RenderWebItem renders the web spec of one item. Skipped assets (RAW/HEIC,
// undecodable) keep the delivery alive unless the deployment requires a web
// version for every asset.
func (s *Service) RenderWebItem(ctx context.Context, tenantID, deliveryID, itemID uuid.UUID, version int) (*RenderResult, error) {
	res := &RenderResult{DeliveryID: deliveryID, ItemID: itemID, BuildVersion: version}
	var (
		status     string
		buildVer   int
		position   int
		sourceKey  string
		sourceName string
		sourceMime string
		revisionID *uuid.UUID
		webStatus  string
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT d.status, d.build_version, i.position, i.original_object_key,
			       i.original_name, i.original_mime, i.revision_id, i.web_status
			FROM deliveries d
			JOIN delivery_items i ON i.tenant_id = d.tenant_id AND i.delivery_id = d.id
			WHERE d.tenant_id = $1 AND d.id = $2 AND i.id = $3`,
			tenantID, deliveryID, itemID).
			Scan(&status, &buildVer, &position, &sourceKey, &sourceName, &sourceMime,
				&revisionID, &webStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Status = "stale"
			return nil
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("load delivery item: %w", err)
	}
	if res.Status == "stale" {
		return res, nil
	}
	if buildVer != version || status != StatusBuilding || webStatus != WebPending {
		res.Status = "stale"
		return res, nil
	}

	if revisionID == nil && media.WebSpecUnsupported(sourceMime, sourceName) {
		if s.cfg.RequireWebSpec {
			return nil, fmt.Errorf("%w: %s (%s)", media.ErrWebSpecDecode, sourceName, sourceMime)
		}
		return s.skipWebItem(ctx, tenantID, deliveryID, itemID, version, WarningUnsupported,
			"web version is not supported for this format")
	}

	data, err := s.readObject(ctx, sourceKey, maxWebSource)
	if err != nil {
		// A missing source would also break the original package; retry and let
		// the dead-letter path fail the build.
		return nil, fmt.Errorf("read delivery source: %w", err)
	}
	spec, err := media.RenderWebSpec(data, s.cfg.WebEdge)
	if err != nil {
		switch {
		case errors.Is(err, media.ErrWebSpecDecode):
			if s.cfg.RequireWebSpec {
				return nil, err
			}
			return s.skipWebItem(ctx, tenantID, deliveryID, itemID, version, WarningDecode,
				"source image could not be decoded")
		default:
			return nil, fmt.Errorf("render web spec: %w", err)
		}
	}

	key := WebItemKey(tenantID, deliveryID, version, position)
	if err := s.store.Put(ctx, key, bytes.NewReader(spec.Bytes), int64(len(spec.Bytes)), "image/jpeg"); err != nil {
		return nil, fmt.Errorf("store web spec: %w", err)
	}
	webName := WebFileName(sourceName)

	updated := false
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE delivery_items SET
				web_status = 'ready', web_object_key = $4, web_name = $5, web_size = $6,
				web_sha256 = $7, web_width = $8, web_height = $9,
				web_warning_code = '', web_warning = '', updated_at = now()
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3 AND web_status = 'pending'
			  AND EXISTS (
				SELECT 1 FROM deliveries d
				WHERE d.tenant_id = $1 AND d.id = $2 AND d.build_version = $10 AND d.status = 'building')`,
			tenantID, deliveryID, itemID, key, webName, int64(len(spec.Bytes)),
			spec.SHA256, spec.Width, spec.Height, version)
		if err != nil {
			return fmt.Errorf("mark web ready: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		updated = true
		return s.enqueuePackages(ctx, tx, tenantID, deliveryID, version)
	})
	if err != nil {
		s.deleteObjects(ctx, tenantID, []string{key})
		return nil, err
	}
	if !updated {
		s.deleteObjects(ctx, tenantID, []string{key})
		res.Status = "stale"
		return res, nil
	}
	res.Status = WebReady
	return res, nil
}

func (s *Service) skipWebItem(ctx context.Context, tenantID, deliveryID, itemID uuid.UUID,
	version int, code, message string) (*RenderResult, error) {
	res := &RenderResult{DeliveryID: deliveryID, ItemID: itemID, BuildVersion: version,
		Status: WebSkipped, WarningCode: code}
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE delivery_items SET
				web_status = 'skipped', web_warning_code = $4, web_warning = $5,
				web_object_key = '', web_name = '', web_size = 0, web_sha256 = '',
				web_width = 0, web_height = 0, updated_at = now()
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3 AND web_status = 'pending'
			  AND EXISTS (
				SELECT 1 FROM deliveries d
				WHERE d.tenant_id = $1 AND d.id = $2 AND d.build_version = $6 AND d.status = 'building')`,
			tenantID, deliveryID, itemID, code, message, version)
		if err != nil {
			return fmt.Errorf("mark web skipped: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return s.enqueuePackages(ctx, tx, tenantID, deliveryID, version)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// enqueuePackages starts the package jobs as soon as the last web render is
// terminal. jobs.Enqueue dedupes by subject, so two racing renderers enqueue one
// package build each.
func (s *Service) enqueuePackages(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID, version int) error {
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM delivery_items
		WHERE tenant_id = $1 AND delivery_id = $2 AND web_status = 'pending'`,
		tenantID, deliveryID).Scan(&pending); err != nil {
		return fmt.Errorf("count pending web renders: %w", err)
	}
	if pending > 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `
		SELECT id, spec FROM delivery_packages
		WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3
		ORDER BY spec`, tenantID, deliveryID, version)
	if err != nil {
		return fmt.Errorf("list packages to build: %w", err)
	}
	defer rows.Close()
	type pkg struct {
		ID   uuid.UUID
		Spec string
	}
	var pkgs []pkg
	for rows.Next() {
		var p pkg
		if err := rows.Scan(&p.ID, &p.Spec); err != nil {
			return fmt.Errorf("scan package: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(pkgs) != 2 {
		return fmt.Errorf("delivery %s has %d package rows, want 2", deliveryID, len(pkgs))
	}
	for _, p := range pkgs {
		if _, err := jobs.Enqueue(ctx, tx, tenantID, JobPackage, PackSubject, &p.ID,
			BuildPayload{DeliveryID: deliveryID, BuildVersion: version, PackageID: p.ID, Spec: p.Spec}); err != nil {
			return err
		}
	}
	return nil
}

// BuildPackage renders one ZIP from the item snapshots. The archive stores
// original bytes uncompressed and a manifest; sources are hashed against the
// snapshot, so a silently replaced object fails the build instead of being
// delivered.
func (s *Service) BuildPackage(ctx context.Context, tenantID, deliveryID, packageID uuid.UUID, version int) (*PackageResult, error) {
	var (
		status      string
		buildVer    int
		projectCode string
	)
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT d.status, d.build_version, p.code
			FROM deliveries d
			JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
			WHERE d.tenant_id = $1 AND d.id = $2`, tenantID, deliveryID).
			Scan(&status, &buildVer, &projectCode)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load delivery for package: %w", err)
	}
	if buildVer != version || status != StatusBuilding {
		return &PackageResult{DeliveryID: deliveryID, PackageID: packageID, Version: version, Status: "stale"}, nil
	}

	var (
		pkgStatus string
		spec      string
	)
	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT status, spec FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3 AND version = $4`,
			tenantID, deliveryID, packageID, version).Scan(&pkgStatus, &spec); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPackageNotFound
			}
			return fmt.Errorf("load delivery package: %w", err)
		}
		if pkgStatus == PackageReady {
			return nil
		}
		_, err := tx.Exec(ctx, `
			UPDATE delivery_packages SET status = 'building', last_error = '', updated_at = now()
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3 AND version = $4
			  AND status <> 'ready'`, tenantID, deliveryID, packageID, version)
		return err
	})
	if err != nil {
		return nil, err
	}
	if pkgStatus == PackageReady {
		// An earlier attempt stored this ZIP but may have failed before the
		// delivery was published; finalize is idempotent, so a retried job still
		// converges instead of leaving the delivery stuck in building.
		if err := s.finalize(ctx, tenantID, deliveryID, version); err != nil {
			return nil, err
		}
		return &PackageResult{DeliveryID: deliveryID, PackageID: packageID, Version: version,
			Spec: spec, Status: PackageReady}, nil
	}

	items, err := s.loadBuildItems(ctx, tenantID, deliveryID)
	if err != nil {
		return nil, err
	}

	entries, metas, warnings := s.buildEntries(projectCode, spec, items)
	if len(items) == 0 {
		return nil, errors.New("delivery has no items")
	}

	temp, err := os.CreateTemp(s.cfg.TempDir, fmt.Sprintf("%s%s-%s-", tempPrefix, deliveryID, spec))
	if err != nil {
		return nil, fmt.Errorf("create delivery spool: %w", err)
	}
	defer func() {
		_ = temp.Close()
		_ = os.Remove(temp.Name())
	}()

	manifestAt := time.Now().UTC()
	var manifestJSON []byte
	zipRes, err := Write(ctx, temp, entries, func(results []ZipEntryResult) ([]byte, error) {
		m := Manifest{
			SchemaVersion: ManifestSchema,
			DeliveryID:    deliveryID,
			ProjectCode:   projectCode,
			Spec:          spec,
			BuildVersion:  version,
			GeneratedAt:   manifestAt,
			Warnings:      warnings,
			Items:         make([]ManifestItem, len(metas)),
		}
		copy(m.Items, metas)
		for i := range results {
			if i >= len(m.Items) {
				break
			}
			m.Items[i].Entry = results[i].Name
			m.Items[i].SHA256 = results[i].SHA256
			m.Items[i].Size = results[i].Size
		}
		body, err := m.JSON()
		if err != nil {
			return nil, err
		}
		manifestJSON = body
		return body, nil
	})
	if err != nil {
		return nil, fmt.Errorf("write %s package: %w", spec, err)
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync package: %w", err)
	}
	stat, err := temp.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat package: %w", err)
	}
	if _, err := temp.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind package: %w", err)
	}
	key := PackageKey(tenantID, deliveryID, version, spec)
	fileName := PackageName(projectCode, spec, manifestAt, deliveryID)
	if err := s.store.Put(ctx, key, temp, stat.Size(), "application/zip"); err != nil {
		return nil, fmt.Errorf("store %s package: %w", spec, err)
	}

	err = db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE delivery_packages SET
				status = 'ready', object_key = $4, file_name = $5, size_bytes = $6,
				sha256 = $7, entry_count = $8, manifest = $9, last_error = '',
				built_at = now(), updated_at = now()
			WHERE tenant_id = $1 AND delivery_id = $2 AND id = $3 AND version = $10
			  AND status <> 'ready'
			  AND EXISTS (
				SELECT 1 FROM deliveries d
				WHERE d.tenant_id = $1 AND d.id = $2 AND d.build_version = $10 AND d.status = 'building')`,
			tenantID, deliveryID, packageID, key, fileName, stat.Size(), zipRes.SHA256,
			len(zipRes.Entries), manifestJSON, version)
		if err != nil {
			return fmt.Errorf("mark package ready: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.finalize(ctx, tenantID, deliveryID, version); err != nil {
		return nil, err
	}
	return &PackageResult{DeliveryID: deliveryID, PackageID: packageID, Version: version,
		Spec: spec, Status: PackageReady, Size: stat.Size(), SHA256: zipRes.SHA256,
		Entries: len(zipRes.Entries)}, nil
}

type buildItem struct {
	ID             uuid.UUID
	AssetID        uuid.UUID
	Position       int
	OriginalName   string
	OriginalKey    string
	OriginalSize   int64
	OriginalSHA256 string
	RevisionID     *uuid.UUID
	RevisionNo     int
	WebKey         string
	WebSize        int64
	WebSHA256      string
	WebWidth       int
	WebHeight      int
	WebStatus      string
	WebWarningCode string
}

func (s *Service) loadBuildItems(ctx context.Context, tenantID, deliveryID uuid.UUID) ([]buildItem, error) {
	var out []buildItem
	err := db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, asset_id, position, original_name, original_object_key, original_size,
			       original_sha256, revision_id, revision_no, web_object_key, web_size,
			       web_sha256, web_width, web_height, web_status, web_warning_code
			FROM delivery_items
			WHERE tenant_id = $1 AND delivery_id = $2
			ORDER BY position`, tenantID, deliveryID)
		if err != nil {
			return fmt.Errorf("load package items: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var it buildItem
			if err := rows.Scan(&it.ID, &it.AssetID, &it.Position, &it.OriginalName,
				&it.OriginalKey, &it.OriginalSize, &it.OriginalSHA256, &it.RevisionID,
				&it.RevisionNo, &it.WebKey, &it.WebSize, &it.WebSHA256, &it.WebWidth,
				&it.WebHeight, &it.WebStatus, &it.WebWarningCode); err != nil {
				return fmt.Errorf("scan package item: %w", err)
			}
			out = append(out, it)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) buildEntries(projectCode, spec string, items []buildItem) ([]ZipEntry, []ManifestItem, []string) {
	entries := make([]ZipEntry, 0, len(items))
	metas := make([]ManifestItem, 0, len(items))
	warnings := []string{}
	for _, it := range items {
		if spec == SpecWeb {
			if it.WebStatus != WebReady {
				if it.WebWarningCode != "" {
					warnings = append(warnings, fmt.Sprintf("position %d (%s): %s",
						it.Position, it.OriginalName, it.WebWarningCode))
				}
				continue
			}
			name := EntryName(projectCode, it.Position, it.OriginalName, ".jpg")
			entries = append(entries, s.packEntry(name, it.WebKey, it.WebSHA256, it.WebSize))
			metas = append(metas, manifestMeta(name, it, it.WebSHA256, it.WebSize, it.WebWidth, it.WebHeight, ""))
			continue
		}
		name := EntryName(projectCode, it.Position, it.OriginalName, path.Ext(it.OriginalName))
		entries = append(entries, s.packEntry(name, it.OriginalKey, it.OriginalSHA256, it.OriginalSize))
		warning := ""
		if it.WebStatus == WebSkipped {
			warning = it.WebWarningCode
			warnings = append(warnings, fmt.Sprintf("position %d (%s): %s",
				it.Position, it.OriginalName, it.WebWarningCode))
		}
		metas = append(metas, manifestMeta(name, it, it.OriginalSHA256, it.OriginalSize,
			it.WebWidth, it.WebHeight, warning))
	}
	return entries, metas, warnings
}

func manifestMeta(entry string, it buildItem, sha string, size int64, width, height int, warning string) ManifestItem {
	m := ManifestItem{
		Entry:        entry,
		AssetID:      it.AssetID,
		OriginalName: it.OriginalName,
		Source:       "original",
		RevisionID:   it.RevisionID,
		RevisionNo:   it.RevisionNo,
		SHA256:       sha,
		Size:         size,
		Width:        width,
		Height:       height,
		Warning:      warning,
	}
	if it.RevisionID != nil {
		m.Source = "revision"
	}
	return m
}

// openSource is the zip entry opener. The service method is bound below because
// entries outlive the loop; the store comes from the service.
func (s *Service) packEntry(name, key, sha string, size int64) ZipEntry {
	return ZipEntry{
		Name:           name,
		ExpectedSHA256: sha,
		ExpectedSize:   size,
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			return s.store.Open(ctx, key)
		},
	}
}

// finalize publishes the delivery once both packages are ready. Payment and the
// project state are re-read under locks: a refund between build and publish
// blocks the delivery instead of exposing a paid-only download.
func (s *Service) finalize(ctx context.Context, tenantID, deliveryID uuid.UUID, version int) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var (
			status        string
			buildVersion  int
			projectTitle  string
			paymentStatus string
			projectState  string
			roundStatus   string
		)
		err := tx.QueryRow(ctx, `
			SELECT d.status, d.build_version, p.title, p.payment_status,
			       CASE WHEN p.state NOT IN ('cancelled','archived')
			         AND EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status = 'cancelled')
			         AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.tenant_id = p.tenant_id AND b.project_id = p.id AND b.status IN ('held','confirmed'))
			         THEN 'cancelled' ELSE p.state END,
			       r.status
			FROM deliveries d
			JOIN projects p ON p.tenant_id = d.tenant_id AND p.id = d.project_id
			JOIN proof_rounds r ON r.tenant_id = d.tenant_id AND r.id = d.proof_round_id
			WHERE d.tenant_id = $1 AND d.id = $2
			FOR UPDATE OF d
			FOR SHARE OF p, r`, tenantID, deliveryID).
			Scan(&status, &buildVersion, &projectTitle, &paymentStatus, &projectState, &roundStatus)
		if err != nil {
			return fmt.Errorf("lock delivery for finalize: %w", err)
		}
		if buildVersion != version || status != StatusBuilding {
			return nil
		}
		if paymentStatus != "paid" {
			_, err := tx.Exec(ctx, `
				UPDATE deliveries SET status = 'blocked', blocked_reason = $3, updated_at = now()
				WHERE tenant_id = $1 AND id = $2`, tenantID, deliveryID, BlockedReasonPayment)
			return err
		}
		if err := checkProjectState(projectState); err != nil {
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version, err.Error())
		}
		if roundStatus != "approved" {
			return s.markFailedTx(ctx, tx, tenantID, deliveryID, version,
				"proof round is no longer approved")
		}

		var (
			packages int
			total    int64
		)
		if err := tx.QueryRow(ctx, `
			SELECT count(*), coalesce(sum(size_bytes), 0)
			FROM delivery_packages
			WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3 AND status = 'ready'
			  AND spec IN ('web','original')`, tenantID, deliveryID, version).
			Scan(&packages, &total); err != nil {
			return fmt.Errorf("sum ready packages: %w", err)
		}
		if packages < 2 {
			return nil
		}
		var items int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM delivery_items WHERE tenant_id = $1 AND delivery_id = $2`,
			tenantID, deliveryID).Scan(&items); err != nil {
			return fmt.Errorf("count delivery items: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deliveries SET status = 'ready', item_count = $3, total_bytes = $4,
				ready_at = now(), blocked_reason = '', last_error = '', updated_at = now()
			WHERE tenant_id = $1 AND id = $2`, tenantID, deliveryID, items, total); err != nil {
			return fmt.Errorf("publish delivery: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE assets a SET status = 'delivered', updated_at = now()
			FROM delivery_items i
			WHERE i.tenant_id = $1 AND i.delivery_id = $2
			  AND a.tenant_id = i.tenant_id AND a.id = i.asset_id
			  AND a.status <> 'archived'`, tenantID, deliveryID); err != nil {
			return fmt.Errorf("mark assets delivered: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"delivery_id": deliveryID, "build_version": version, "item_count": items,
		})
		if err != nil {
			return fmt.Errorf("marshal delivery notification payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (tenant_id, type, subject_type, subject_id, dedupe_key,
				title, body, payload, due_at)
			VALUES ($1,$2,'delivery',$3,$4,$5,$6,$7,now())
			ON CONFLICT DO NOTHING`,
			tenantID, ReadyNotice, deliveryID,
			fmt.Sprintf("delivery_ready:%s:v%d", deliveryID, version),
			"交付包已生成",
			fmt.Sprintf("《%s》的交付包已生成，共 %d 张，可签发下载链接。", projectTitle, items),
			payload); err != nil {
			return fmt.Errorf("queue delivery ready notification: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, nil, "delivery.ready", "delivery",
			deliveryID.String(), "", map[string]any{
				"build_version": version, "item_count": items, "total_bytes": total,
			}); err != nil {
			return err
		}
		return outbox.Emit(ctx, tx, tenantID, "delivery", deliveryID.String(), "delivery.ready",
			map[string]any{"build_version": version, "item_count": items, "total_bytes": total})
	})
}

// FailBuild is the worker dead-letter hook. It only touches the build that
// failed; a delivery already rebuilt has a different version and is left alone.
func (s *Service) FailBuild(ctx context.Context, tenantID, deliveryID uuid.UUID, version int, reason string) error {
	return db.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return s.markFailedTx(ctx, tx, tenantID, deliveryID, version, reason)
	})
}

func (s *Service) markFailedTx(ctx context.Context, tx pgx.Tx, tenantID, deliveryID uuid.UUID,
	version int, reason string) error {
	reason = truncateText(reason, 500)
	tag, err := tx.Exec(ctx, `
		UPDATE deliveries SET status = 'failed', last_error = $4, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND build_version = $3 AND status = 'building'`,
		tenantID, deliveryID, version, reason)
	if err != nil {
		return fmt.Errorf("mark delivery failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE delivery_packages SET status = 'failed', last_error = $4, updated_at = now()
		WHERE tenant_id = $1 AND delivery_id = $2 AND version = $3 AND status <> 'ready'`,
		tenantID, deliveryID, version, reason); err != nil {
		return fmt.Errorf("mark packages failed: %w", err)
	}
	if err := audit.Write(ctx, tx, tenantID, nil, "delivery.build_failed", "delivery",
		deliveryID.String(), "", map[string]any{"build_version": version, "reason": reason}); err != nil {
		return err
	}
	if err := outbox.Emit(ctx, tx, tenantID, "delivery", deliveryID.String(), "delivery.build_failed",
		map[string]any{"build_version": version, "reason": reason}); err != nil {
		return err
	}
	if s.alerts != nil {
		if _, err := s.alerts.Raise(ctx, tx, tenantID, alert.Input{
			SubjectType: "delivery",
			SubjectID:   deliveryID.String(),
			Code:        AlertBuildFailed,
			Severity:    alert.SeverityWarning,
			DedupeKey:   fmt.Sprintf("%s:%s:v%d", AlertBuildFailed, deliveryID, version),
			Title:       "交付构建失败",
			Detail:      map[string]any{"delivery_id": deliveryID, "build_version": version, "reason": reason},
		}); err != nil {
			return err
		}
	}
	return nil
}

// CleanupTemp removes delivery spools left behind by a crashed worker.
func (s *Service) CleanupTemp(now time.Time) (int, error) {
	dir := s.cfg.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("list delivery temp dir: %w", err)
	}
	cutoff := now.Add(-tempMaxAge)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Service) readObject(ctx context.Context, key string, limit int64) ([]byte, error) {
	size, err := s.store.Stat(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, fmt.Errorf("%w: object %s is missing", storage.ErrNotFound, key)
		}
		return nil, fmt.Errorf("stat delivery source: %w", err)
	}
	if size > limit {
		return nil, fmt.Errorf("source object %s is %d bytes, limit is %d", key, size, limit)
	}
	rc, err := s.store.Open(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("open delivery source: %w", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read delivery source: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("source object %s exceeds %d bytes", key, limit)
	}
	return data, nil
}

// deleteObjects removes superseded objects best-effort. The transaction is
// already committed, so a failed cleanup is logged by the caller's retry sweep
// rather than failing the build.
func (s *Service) deleteObjects(ctx context.Context, tenantID uuid.UUID, keys []string) {
	if len(keys) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("t/%s/deliveries/", tenantID)
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if err := s.store.Delete(cleanupCtx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			s.log.Warn("delete superseded delivery object", "key", key, "err", err)
		}
	}
}

// keyVersion extracts the build version from a delivery object key
// (t/<tenant>/deliveries/<id>/v<N>/...). It returns 0 for foreign keys.
func keyVersion(key string, deliveryID uuid.UUID) int {
	marker := "/deliveries/" + deliveryID.String() + "/v"
	i := strings.Index(key, marker)
	if i < 0 {
		return 0
	}
	rest := key[i+len(marker):]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return 0
	}
	v, err := strconv.Atoi(rest[:j])
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// revisionMime maps the stored revision extension to an image type. Revisions
// are restricted to JPEG/PNG/WebP at import time.
func revisionMime(key string) string {
	switch strings.ToLower(path.Ext(key)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

func truncateText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
