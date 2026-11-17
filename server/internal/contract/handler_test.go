package contract

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDisabledContractAPI(t *testing.T) {
	h := &Handler{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/contracts", nil)
	h.disabled(recorder, request)

	if recorder.Code != http.StatusGone {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusGone)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "feature_disabled") {
		t.Fatalf("body = %q, want feature_disabled problem", got)
	}
}
