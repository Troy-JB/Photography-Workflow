package invoice

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"photography-workflow/server/internal/auth"
	"photography-workflow/server/internal/httpx"
)

type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func (h *Handler) Register(r chi.Router) {
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/invoices", h.create)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/invoices", h.list)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/invoices/{invoiceID}", h.get)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/invoices/{invoiceID}/file-url", h.fileURL)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/invoices/{invoiceID}/issue", h.issue)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/invoices/{invoiceID}/send", h.send)
	r.With(auth.RequireAnyRole("owner", "staff")).Get("/invoices/{invoiceID}/red-flushes", h.listRedFlushes)
	r.With(auth.RequireAnyRole("owner", "staff")).Post("/invoices/{invoiceID}/red-flushes/{redFlushID}/issue", h.issueRedFlush)
	// Review decisions and voids are finance controls reserved for the owner.
	r.With(auth.RequireAnyRole("owner")).Post("/invoices/{invoiceID}/approve", h.approve)
	r.With(auth.RequireAnyRole("owner")).Post("/invoices/{invoiceID}/reject", h.reject)
	r.With(auth.RequireAnyRole("owner")).Post("/invoices/{invoiceID}/void", h.void)
}

type createRequest struct {
	PaymentID   uuid.UUID `json:"payment_id"`
	Type        string    `json:"type"`
	Title       string    `json:"title"`
	TaxNo       string    `json:"tax_no"`
	Email       string    `json:"email"`
	Address     string    `json:"address"`
	Phone       string    `json:"phone"`
	BankName    string    `json:"bank_name"`
	BankAccount string    `json:"bank_account"`
	Remark      string    `json:"remark"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	var req createRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	inv, err := h.svc.Create(r.Context(), p.TenantID, p.UserID, r.RemoteAddr, CreateInput{
		PaymentID: req.PaymentID, Type: req.Type, Title: req.Title, TaxNo: req.TaxNo,
		Email: req.Email, Address: req.Address, Phone: req.Phone, BankName: req.BankName,
		BankAccount: req.BankAccount, Remark: req.Remark,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, inv)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	projectID, err := optionalID(w, r, "project_id")
	if err != nil {
		return
	}
	paymentID, err := optionalID(w, r, "payment_id")
	if err != nil {
		return
	}
	limit := httpx.IntQuery(r, "limit", 50, 1, 200)
	offset := httpx.IntQuery(r, "offset", 0, 0, 1<<30)
	items, total, err := h.svc.List(r.Context(), p.TenantID, projectID, paymentID,
		r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": total, "limit": limit, "offset": offset,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	inv, err := h.svc.Get(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

type versionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason"`
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	h.reviewAction(w, r, false)
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	h.reviewAction(w, r, true)
}

func (h *Handler) reviewAction(w http.ResponseWriter, r *http.Request, reject bool) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	var inv *Invoice
	if reject {
		inv, err = h.svc.Reject(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, req.Reason)
	} else {
		inv, err = h.svc.Approve(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

func (h *Handler) void(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	inv, err := h.svc.Void(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion, req.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

type fileURLRequest struct {
	RedFlushID *uuid.UUID `json:"red_flush_id"`
}

func (h *Handler) fileURL(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	var req fileURLRequest
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(w, r, &req); err != nil {
			httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
			return
		}
	}
	upload, err := h.svc.FileURL(r.Context(), p.TenantID, id, req.RedFlushID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, upload)
}

type issueRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	InvoiceNo       string `json:"invoice_no"`
	InvoiceCode     string `json:"invoice_code"`
	FileKey         string `json:"file_key"`
	FileHash        string `json:"file_hash"`
}

func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	var req issueRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	inv, err := h.svc.Issue(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, IssueInput{
		ExpectedVersion: req.ExpectedVersion, InvoiceNo: req.InvoiceNo, InvoiceCode: req.InvoiceCode,
		FileKey: req.FileKey, FileHash: req.FileHash,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	var req versionRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	inv, err := h.svc.Send(r.Context(), p.TenantID, p.UserID, id, r.RemoteAddr, req.ExpectedVersion)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inv)
}

func (h *Handler) listRedFlushes(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	id, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	items, err := h.svc.ListRedFlushes(r.Context(), p.TenantID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

type redFlushIssueRequest struct {
	RedFlushNo   string `json:"red_flush_no"`
	RedFlushCode string `json:"red_flush_code"`
	FileKey      string `json:"file_key"`
	FileHash     string `json:"file_hash"`
}

func (h *Handler) issueRedFlush(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		httpx.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", "Authentication required", "")
		return
	}
	invoiceIDValue, err := invoiceID(r)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid invoice id", "")
		return
	}
	redFlushID, err := uuid.Parse(chi.URLParam(r, "redFlushID"))
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_id", "Invalid red flush id", "")
		return
	}
	var req redFlushIssueRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.WriteProblem(w, r, http.StatusBadRequest, "invalid_body", "Invalid request body", err.Error())
		return
	}
	rf, err := h.svc.IssueRedFlush(r.Context(), p.TenantID, p.UserID, invoiceIDValue, redFlushID, r.RemoteAddr, RedFlushIssueInput{
		RedFlushNo: req.RedFlushNo, RedFlushCode: req.RedFlushCode, FileKey: req.FileKey, FileHash: req.FileHash,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rf)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "invoice_not_found", "Invoice not found", "")
	case errors.Is(err, ErrRedFlushNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "red_flush_not_found", "Red flush not found", "")
	case errors.Is(err, ErrPaymentNotFound):
		httpx.WriteProblem(w, r, http.StatusNotFound, "payment_not_found", "Payment not found", "")
	case errors.Is(err, ErrVersionConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "version_conflict", "Version conflict", "invoice was modified, reload and retry")
	case errors.Is(err, ErrIllegalState):
		httpx.WriteProblem(w, r, http.StatusConflict, "illegal_state", "Illegal invoice state", err.Error())
	case errors.Is(err, ErrActiveInvoice):
		httpx.WriteProblem(w, r, http.StatusConflict, "active_invoice", "Payment already has an active invoice", "")
	case errors.Is(err, ErrInvoiceNoConflict):
		httpx.WriteProblem(w, r, http.StatusConflict, "invoice_no_conflict", "Invoice number already used", "")
	case errors.Is(err, ErrFileMismatch):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	case errors.Is(err, ErrProvider):
		httpx.WriteProblem(w, r, http.StatusBadGateway, "invoice_provider_error", "Invoice provider error", err.Error())
	case errors.Is(err, ErrInvalidInput):
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", err.Error())
	default:
		httpx.Internal(w, r, h.log, err)
	}
}

func invoiceID(r *http.Request) (uuid.UUID, error) {
	return uuid.Parse(chi.URLParam(r, "invoiceID"))
}

func optionalID(w http.ResponseWriter, r *http.Request, name string) (*uuid.UUID, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		httpx.WriteProblem(w, r, http.StatusUnprocessableEntity, "validation_failed", "Validation failed", "invalid "+name)
		return nil, err
	}
	return &id, nil
}
