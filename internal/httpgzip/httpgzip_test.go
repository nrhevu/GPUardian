package httpgzip

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareCompressesOnlyWorthwhileResponses(t *testing.T) {
	large := `{"data":"` + strings.Repeat("gpuardian", 400) + `"}`
	tests := []struct {
		name           string
		acceptEncoding string
		contentType    string
		status         int
		body           string
		wantGzip       bool
	}{
		{name: "large json", acceptEncoding: "gzip, deflate, br", contentType: "application/json", status: http.StatusOK, body: large, wantGzip: true},
		{name: "small json", acceptEncoding: "gzip", contentType: "application/json", status: http.StatusOK, body: `{"ok":true}`},
		{name: "client without gzip", acceptEncoding: "br", contentType: "application/json", status: http.StatusOK, body: large},
		{name: "gzip refused", acceptEncoding: "gzip;q=0, br", contentType: "application/json", status: http.StatusOK, body: large},
		{name: "binary content", acceptEncoding: "gzip", contentType: "image/png", status: http.StatusOK, body: large},
		{name: "event stream", acceptEncoding: "gzip", contentType: "text/event-stream", status: http.StatusOK, body: large},
		{name: "not modified", acceptEncoding: "gzip", contentType: "text/html; charset=utf-8", status: http.StatusNotModified},
		{name: "large error", acceptEncoding: "*", contentType: "application/json", status: http.StatusBadRequest, body: large, wantGzip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.WriteHeader(tt.status)
				// Write in small chunks to exercise the buffering threshold.
				for chunk := range slicesChunk([]byte(tt.body), 100) {
					_, _ = w.Write(chunk)
				}
			}))
			req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
			req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			gotGzip := recorder.Header().Get("Content-Encoding") == "gzip"
			if gotGzip != tt.wantGzip {
				t.Fatalf("gzip = %v, want %v", gotGzip, tt.wantGzip)
			}
			body := recorder.Body.Bytes()
			if gotGzip {
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if body, err = io.ReadAll(reader); err != nil {
					t.Fatal(err)
				}
			}
			if string(body) != tt.body {
				t.Fatalf("body length = %d, want %d", len(body), len(tt.body))
			}
			if !strings.Contains(strings.Join(recorder.Header().Values("Vary"), ","), "Accept-Encoding") {
				t.Fatalf("Vary = %v, want Accept-Encoding", recorder.Header().Values("Vary"))
			}
		})
	}
}

func slicesChunk(data []byte, size int) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		for len(data) > 0 {
			n := min(size, len(data))
			if !yield(data[:n]) {
				return
			}
			data = data[n:]
		}
	}
}
