package prep

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"photography-workflow/server/internal/audit"
	"photography-workflow/server/internal/outbox"
)

const callSheetSchema = "call_sheet.v1"

//go:embed call_sheet.html
var callSheetTemplateSource string

var callSheetTemplate = template.Must(template.New("call_sheet").Parse(callSheetTemplateSource))

// CallSheetSnapshot is serialized with json.Marshal and stored byte-for-byte.
// Do not re-serialize a stored snapshot: the SHA-256 covers the exact bytes.
type CallSheetSnapshot struct {
	Schema         string                  `json:"schema"`
	Version        int                     `json:"version"`
	FrozenAt       time.Time               `json:"frozen_at"`
	Timezone       string                  `json:"timezone"`
	Booking        SnapshotBooking         `json:"booking"`
	Project        SnapshotProject         `json:"project"`
	Client         SnapshotClient          `json:"client"`
	Brief          *SnapshotBrief          `json:"brief,omitempty"`
	ShotList       *SnapshotShotList       `json:"shot_list,omitempty"`
	Gear           []SnapshotGear          `json:"gear"`
	Authorizations []SnapshotAuthorization `json:"authorizations"`
}

type SnapshotBooking struct {
	ID        uuid.UUID          `json:"id"`
	StartsAt  time.Time          `json:"starts_at"`
	EndsAt    time.Time          `json:"ends_at"`
	Note      string             `json:"note"`
	Resources []SnapshotResource `json:"resources"`
}

type SnapshotResource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type SnapshotProject struct {
	ID    uuid.UUID `json:"id"`
	Code  string    `json:"code"`
	Title string    `json:"title"`
}

type SnapshotClient struct {
	Name    string `json:"name"`
	Company string `json:"company"`
	Phone   string `json:"phone"`
	Wechat  string `json:"wechat"`
}

type SnapshotBrief struct {
	Venue        string `json:"venue"`
	Style        string `json:"style"`
	ShotCount    int    `json:"shot_count"`
	Requirements string `json:"requirements"`
}

type SnapshotShotList struct {
	Title string             `json:"title"`
	Note  string             `json:"note"`
	Items []SnapshotShotItem `json:"items"`
}

type SnapshotShotItem struct {
	Category     string `json:"category"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	ReferenceURL string `json:"reference_url"`
	Priority     string `json:"priority"`
	Done         bool   `json:"done"`
}

type SnapshotGear struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Quantity int    `json:"quantity"`
	Checked  bool   `json:"checked"`
	Resource string `json:"resource"`
}

type SnapshotAuthorization struct {
	Kind     string     `json:"kind"`
	Subject  string     `json:"subject"`
	Status   string     `json:"status"`
	SignedAt *time.Time `json:"signed_at,omitempty"`
}

type CallSheet struct {
	ID             uuid.UUID       `json:"id"`
	BookingID      uuid.UUID       `json:"booking_id"`
	Version        int             `json:"version"`
	Snapshot       json.RawMessage `json:"snapshot"`
	SnapshotSHA256 string          `json:"snapshot_sha256"`
	CreatedBy      *uuid.UUID      `json:"created_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

type CallSheetSummary struct {
	ID             uuid.UUID  `json:"id"`
	BookingID      uuid.UUID  `json:"booking_id"`
	Version        int        `json:"version"`
	SnapshotSHA256 string     `json:"snapshot_sha256"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

func snapshotSHA256(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) FreezeCallSheet(ctx context.Context, tenantID, actor, bookingID uuid.UUID, ip string) (*CallSheet, error) {
	return withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*CallSheet, error) {
		meta, err := s.lockBooking(ctx, tx, tenantID, bookingID, false)
		if err != nil {
			return nil, err
		}
		if meta.Status != StatusConfirmed {
			return nil, fmt.Errorf("%w: only confirmed bookings can freeze a call sheet", ErrIllegalState)
		}
		var next int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(version), 0) + 1 FROM call_sheets
			WHERE tenant_id = $1 AND booking_id = $2`, tenantID, bookingID).Scan(&next); err != nil {
			return nil, fmt.Errorf("next call sheet version: %w", err)
		}
		snapshot, err := s.buildCallSheetSnapshot(ctx, tx, tenantID, meta.ProjectID, bookingID, next)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return nil, fmt.Errorf("marshal call sheet snapshot: %w", err)
		}
		sum := snapshotSHA256(raw)
		var (
			id        uuid.UUID
			createdAt time.Time
		)
		if err := tx.QueryRow(ctx, `
			INSERT INTO call_sheets (tenant_id, booking_id, version, snapshot, snapshot_sha256, created_by)
			VALUES ($1,$2,$3,$4,$5,$6)
			RETURNING id, created_at`,
			tenantID, bookingID, next, string(raw), sum, actor).Scan(&id, &createdAt); err != nil {
			return nil, fmt.Errorf("insert call sheet: %w", err)
		}
		if err := audit.Write(ctx, tx, tenantID, &actor, "call_sheet.frozen", "call_sheet",
			id.String(), ip, map[string]any{
				"booking_id": bookingID, "version": next, "snapshot_sha256": sum,
			}); err != nil {
			return nil, err
		}
		if err := outbox.Emit(ctx, tx, tenantID, "booking", bookingID.String(), "call_sheet.frozen",
			map[string]any{"call_sheet_id": id, "version": next, "snapshot_sha256": sum}); err != nil {
			return nil, err
		}
		return &CallSheet{
			ID: id, BookingID: bookingID, Version: next,
			Snapshot: json.RawMessage(raw), SnapshotSHA256: sum,
			CreatedBy: &actor, CreatedAt: createdAt,
		}, nil
	})
}

func (s *Service) ListCallSheets(ctx context.Context, tenantID, bookingID uuid.UUID) ([]CallSheetSummary, error) {
	items, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) ([]CallSheetSummary, error) {
		if err := requireBooking(ctx, tx, tenantID, bookingID); err != nil {
			return nil, err
		}
		rows, err := tx.Query(ctx, `
			SELECT id, booking_id, version, snapshot_sha256, created_by, created_at
			FROM call_sheets WHERE tenant_id = $1 AND booking_id = $2
			ORDER BY version DESC`, tenantID, bookingID)
		if err != nil {
			return nil, fmt.Errorf("list call sheets: %w", err)
		}
		defer rows.Close()
		out := []CallSheetSummary{}
		for rows.Next() {
			var c CallSheetSummary
			if err := rows.Scan(&c.ID, &c.BookingID, &c.Version, &c.SnapshotSHA256,
				&c.CreatedBy, &c.CreatedAt); err != nil {
				return nil, fmt.Errorf("scan call sheet: %w", err)
			}
			out = append(out, c)
		}
		return out, rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []CallSheetSummary{}
	}
	return items, nil
}

func (s *Service) GetCallSheet(ctx context.Context, tenantID, callSheetID uuid.UUID) (*CallSheet, error) {
	sheet, err := withTenantResult(ctx, s.pool, tenantID, func(tx pgx.Tx) (*CallSheet, error) {
		var (
			c        CallSheet
			snapshot string
		)
		err := tx.QueryRow(ctx, `
			SELECT id, booking_id, version, snapshot, snapshot_sha256, created_by, created_at
			FROM call_sheets WHERE id = $1 AND tenant_id = $2`,
			callSheetID, tenantID).Scan(&c.ID, &c.BookingID, &c.Version, &snapshot,
			&c.SnapshotSHA256, &c.CreatedBy, &c.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCallSheetNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get call sheet: %w", err)
		}
		if snapshotSHA256([]byte(snapshot)) != c.SnapshotSHA256 {
			return nil, ErrSnapshotCorrupt
		}
		c.Snapshot = json.RawMessage(snapshot)
		return &c, nil
	})
	return sheet, err
}

func (s *Service) RenderCallSheetHTML(ctx context.Context, tenantID, callSheetID uuid.UUID) ([]byte, error) {
	sheet, err := s.GetCallSheet(ctx, tenantID, callSheetID)
	if err != nil {
		return nil, err
	}
	return renderCallSheetHTML(sheet.Snapshot, sheet.SnapshotSHA256, s.loc)
}

func (s *Service) buildCallSheetSnapshot(ctx context.Context, tx pgx.Tx, tenantID, projectID, bookingID uuid.UUID, version int) (*CallSheetSnapshot, error) {
	now := time.Now().UTC()
	snap := &CallSheetSnapshot{
		Schema:         callSheetSchema,
		Version:        version,
		FrozenAt:       now,
		Timezone:       s.loc.String(),
		Booking:        SnapshotBooking{ID: bookingID, Resources: []SnapshotResource{}},
		Project:        SnapshotProject{ID: projectID},
		Gear:           []SnapshotGear{},
		Authorizations: []SnapshotAuthorization{},
	}
	if err := tx.QueryRow(ctx, `
		SELECT starts_at, ends_at, note FROM bookings WHERE id = $1 AND tenant_id = $2`,
		bookingID, tenantID).Scan(&snap.Booking.StartsAt, &snap.Booking.EndsAt, &snap.Booking.Note); err != nil {
		return nil, fmt.Errorf("load booking for snapshot: %w", err)
	}
	snap.Booking.StartsAt = snap.Booking.StartsAt.UTC()
	snap.Booking.EndsAt = snap.Booking.EndsAt.UTC()

	if err := tx.QueryRow(ctx, `
		SELECT p.code, p.title, coalesce(c.name, ''), coalesce(c.company, ''),
			coalesce(c.phone, ''), coalesce(c.wechat, '')
		FROM projects p
		LEFT JOIN clients c ON c.id = p.client_id AND c.tenant_id = p.tenant_id
		WHERE p.id = $1 AND p.tenant_id = $2`, projectID, tenantID).Scan(
		&snap.Project.Code, &snap.Project.Title, &snap.Client.Name,
		&snap.Client.Company, &snap.Client.Phone, &snap.Client.Wechat); err != nil {
		return nil, fmt.Errorf("load project for snapshot: %w", err)
	}

	resources, err := tx.Query(ctx, `
		SELECT r.kind, r.name FROM booking_resources br
		JOIN resources r ON r.id = br.resource_id AND r.tenant_id = br.tenant_id
		WHERE br.tenant_id = $1 AND br.booking_id = $2
		ORDER BY r.kind, r.name`, tenantID, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot resources: %w", err)
	}
	for resources.Next() {
		var res SnapshotResource
		if err := resources.Scan(&res.Kind, &res.Name); err != nil {
			resources.Close()
			return nil, fmt.Errorf("scan snapshot resource: %w", err)
		}
		snap.Booking.Resources = append(snap.Booking.Resources, res)
	}
	resources.Close()
	if err := resources.Err(); err != nil {
		return nil, err
	}

	var brief SnapshotBrief
	err = tx.QueryRow(ctx, `
		SELECT venue, style, shot_count, requirements FROM project_briefs
		WHERE tenant_id = $1 AND project_id = $2`, tenantID, projectID).Scan(
		&brief.Venue, &brief.Style, &brief.ShotCount, &brief.Requirements)
	switch {
	case err == nil:
		snap.Brief = &brief
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("load snapshot brief: %w", err)
	}

	var (
		listID    uuid.UUID
		listTitle string
		listNote  string
	)
	err = tx.QueryRow(ctx, `
		SELECT id, title, note FROM shot_lists WHERE tenant_id = $1 AND booking_id = $2`,
		tenantID, bookingID).Scan(&listID, &listTitle, &listNote)
	switch {
	case err == nil:
		list := &SnapshotShotList{Title: listTitle, Note: listNote, Items: []SnapshotShotItem{}}
		items, err := tx.Query(ctx, `
			SELECT category, title, description, reference_url, priority, done
			FROM shot_items WHERE tenant_id = $1 AND shot_list_id = $2
			ORDER BY sort_order, id`, tenantID, listID)
		if err != nil {
			return nil, fmt.Errorf("load snapshot shot items: %w", err)
		}
		for items.Next() {
			var it SnapshotShotItem
			if err := items.Scan(&it.Category, &it.Title, &it.Description, &it.ReferenceURL,
				&it.Priority, &it.Done); err != nil {
				items.Close()
				return nil, fmt.Errorf("scan snapshot shot item: %w", err)
			}
			list.Items = append(list.Items, it)
		}
		items.Close()
		if err := items.Err(); err != nil {
			return nil, err
		}
		snap.ShotList = list
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("load snapshot shot list: %w", err)
	}

	gearRows, err := tx.Query(ctx, `
		SELECT g.name, g.category, g.quantity, g.checked, coalesce(r.name, '')
		FROM shoot_gear g
		LEFT JOIN resources r ON r.id = g.resource_id AND r.tenant_id = g.tenant_id
		WHERE g.tenant_id = $1 AND g.booking_id = $2
		ORDER BY g.name, g.id`, tenantID, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot gear: %w", err)
	}
	for gearRows.Next() {
		var g SnapshotGear
		if err := gearRows.Scan(&g.Name, &g.Category, &g.Quantity, &g.Checked, &g.Resource); err != nil {
			gearRows.Close()
			return nil, fmt.Errorf("scan snapshot gear: %w", err)
		}
		snap.Gear = append(snap.Gear, g)
	}
	gearRows.Close()
	if err := gearRows.Err(); err != nil {
		return nil, err
	}

	authRows, err := tx.Query(ctx, `
		SELECT kind, subject, status, signed_at FROM booking_authorizations
		WHERE tenant_id = $1 AND booking_id = $2
		ORDER BY kind, subject, id`, tenantID, bookingID)
	if err != nil {
		return nil, fmt.Errorf("load snapshot authorizations: %w", err)
	}
	for authRows.Next() {
		var a SnapshotAuthorization
		if err := authRows.Scan(&a.Kind, &a.Subject, &a.Status, &a.SignedAt); err != nil {
			authRows.Close()
			return nil, fmt.Errorf("scan snapshot authorization: %w", err)
		}
		if a.SignedAt != nil {
			at := a.SignedAt.UTC()
			a.SignedAt = &at
		}
		snap.Authorizations = append(snap.Authorizations, a)
	}
	authRows.Close()
	if err := authRows.Err(); err != nil {
		return nil, err
	}
	return snap, nil
}

type callSheetView struct {
	ProjectCode  string
	ProjectTitle string
	Version      int
	FrozenAt     string
	Timezone     string
	StartsAt     string
	EndsAt       string
	BookingID    string
	Note         string
	Resources    []string
	ClientName   string
	ClientInfo   string
	HasBrief     bool
	BriefVenue   string
	BriefStyle   string
	BriefShots   int
	BriefNeeds   string
	HasShotList  bool
	ShotListName string
	ShotListNote string
	ShotTotal    int
	ShotDone     int
	Shots        []callSheetShot
	Gear         []callSheetGear
	Auths        []callSheetAuth
	SHA256       string
}

type callSheetShot struct {
	Index       int
	Category    string
	Title       string
	Description string
	Reference   string
	Priority    string
	Done        string
}

type callSheetGear struct {
	Name     string
	Category string
	Quantity int
	Checked  string
	Resource string
}

type callSheetAuth struct {
	Kind     string
	Subject  string
	Status   string
	SignedAt string
}

func renderCallSheetHTML(snapshot []byte, sha string, fallback *time.Location) ([]byte, error) {
	var snap CallSheetSnapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return nil, fmt.Errorf("%w: decode snapshot: %v", ErrSnapshotCorrupt, err)
	}
	if snap.Schema != callSheetSchema {
		return nil, fmt.Errorf("%w: unsupported schema %q", ErrSnapshotCorrupt, snap.Schema)
	}
	loc := fallback
	if loc == nil {
		loc = time.UTC
	}
	if loaded, err := time.LoadLocation(snap.Timezone); err == nil {
		loc = loaded
	}
	var buf bytes.Buffer
	if err := callSheetTemplate.Execute(&buf, buildCallSheetView(&snap, sha, loc)); err != nil {
		return nil, fmt.Errorf("render call sheet: %w", err)
	}
	return buf.Bytes(), nil
}

func buildCallSheetView(snap *CallSheetSnapshot, sha string, loc *time.Location) callSheetView {
	format := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04 MST") }
	v := callSheetView{
		ProjectCode:  snap.Project.Code,
		ProjectTitle: snap.Project.Title,
		Version:      snap.Version,
		FrozenAt:     format(snap.FrozenAt),
		Timezone:     snap.Timezone,
		StartsAt:     format(snap.Booking.StartsAt),
		EndsAt:       format(snap.Booking.EndsAt),
		BookingID:    snap.Booking.ID.String(),
		Note:         snap.Booking.Note,
		ClientName:   snap.Client.Name,
		SHA256:       sha,
	}
	for _, res := range snap.Booking.Resources {
		v.Resources = append(v.Resources, res.Kind+" / "+res.Name)
	}
	clientParts := make([]string, 0, 3)
	for _, part := range []string{snap.Client.Company, snap.Client.Phone, snap.Client.Wechat} {
		if part != "" {
			clientParts = append(clientParts, part)
		}
	}
	v.ClientInfo = strings.Join(clientParts, " · ")
	if snap.Brief != nil {
		v.HasBrief = true
		v.BriefVenue = snap.Brief.Venue
		v.BriefStyle = snap.Brief.Style
		v.BriefShots = snap.Brief.ShotCount
		v.BriefNeeds = snap.Brief.Requirements
	}
	if snap.ShotList != nil {
		v.HasShotList = true
		v.ShotListName = snap.ShotList.Title
		v.ShotListNote = snap.ShotList.Note
		for i, item := range snap.ShotList.Items {
			v.ShotTotal++
			done := "未完成"
			if item.Done {
				done = "已完成"
				v.ShotDone++
			}
			v.Shots = append(v.Shots, callSheetShot{
				Index:       i + 1,
				Category:    item.Category,
				Title:       item.Title,
				Description: item.Description,
				Reference:   item.ReferenceURL,
				Priority:    priorityLabel(item.Priority),
				Done:        done,
			})
		}
	}
	for _, g := range snap.Gear {
		checked := "未核对"
		if g.Checked {
			checked = "已核对"
		}
		resource := g.Resource
		if resource == "" {
			resource = "—"
		}
		v.Gear = append(v.Gear, callSheetGear{
			Name: g.Name, Category: g.Category, Quantity: g.Quantity,
			Checked: checked, Resource: resource,
		})
	}
	for _, a := range snap.Authorizations {
		signed := "—"
		if a.SignedAt != nil {
			signed = format(*a.SignedAt)
		}
		v.Auths = append(v.Auths, callSheetAuth{
			Kind: authorizationKindLabel(a.Kind), Subject: a.Subject,
			Status: authorizationStatusLabel(a.Status), SignedAt: signed,
		})
	}
	return v
}

func priorityLabel(v string) string {
	switch v {
	case "high":
		return "高"
	case "low":
		return "低"
	default:
		return "普通"
	}
}

func authorizationKindLabel(v string) string {
	switch v {
	case "model_release":
		return "肖像授权"
	case "venue_permit":
		return "场地许可"
	case "usage_license":
		return "使用授权"
	default:
		return "其他授权"
	}
}

func authorizationStatusLabel(v string) string {
	switch v {
	case "signed":
		return "已签署"
	case "waived":
		return "已豁免"
	default:
		return "待完成"
	}
}
