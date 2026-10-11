package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/krishna2206/zefile/internal/quota"
	"github.com/krishna2206/zefile/internal/upload"
)

func TestWriteUploadErrorMapsQuotaExceeded(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	writeUploadError(rec, req, fmt.Errorf("wrap: %w", quota.ErrQuotaExceeded))

	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusInsufficientStorage {
		t.Errorf("status = %d, want 507", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type = %q, want problem+json", ct)
	}
}

func TestWriteUploadErrorKeepsNeighbours(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{upload.ErrNotFound, http.StatusNotFound},
		{upload.ErrTooLarge, http.StatusRequestEntityTooLarge},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
		writeUploadError(rec, req, tc.err)
		if res := rec.Result(); res.StatusCode != tc.want {
			t.Errorf("status for %v = %d, want %d", tc.err, res.StatusCode, tc.want)
			res.Body.Close()
		} else {
			res.Body.Close()
		}
	}
}
