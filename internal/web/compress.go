package web

import (
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Responses smaller than this are sent as-is; gzip framing would outweigh the
// savings.
const gzipMinBytes = 1024

var gzipWriterPool = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return writer
}}

// gzipMiddleware compresses JSON and text-like static responses. The gateway
// is exposed without a compressing proxy, and the fleet snapshot polled every
// few seconds plus the UI bundle are mostly compressible text.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || r.Header.Get("Range") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		writer := &gzipResponseWriter{ResponseWriter: w}
		defer writer.finish()
		next.ServeHTTP(writer, r)
	})
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		for _, param := range strings.Split(params, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func compressibleContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json", mediaType == "application/javascript",
		mediaType == "application/manifest+json", mediaType == "image/svg+xml":
		return true
	}
	return false
}

// gzipResponseWriter buffers the first gzipMinBytes of a body to decide
// whether compression is worthwhile, then either streams through gzip or
// passes the response through unchanged.
type gzipResponseWriter struct {
	http.ResponseWriter
	status      int
	buffer      []byte
	gzip        *gzip.Writer
	passthrough bool
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

func (g *gzipResponseWriter) WriteHeader(status int) {
	if g.status == 0 {
		g.status = status
	}
}

func (g *gzipResponseWriter) Write(data []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	switch {
	case g.gzip != nil:
		return g.gzip.Write(data)
	case g.passthrough:
		return g.ResponseWriter.Write(data)
	case !g.compressible():
		if err := g.startPassthrough(); err != nil {
			return 0, err
		}
		return g.ResponseWriter.Write(data)
	}
	g.buffer = append(g.buffer, data...)
	if len(g.buffer) >= gzipMinBytes {
		if err := g.startGzip(); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (g *gzipResponseWriter) compressible() bool {
	header := g.Header()
	if g.status < http.StatusOK || g.status == http.StatusNoContent ||
		g.status == http.StatusNotModified || g.status == http.StatusPartialContent {
		return false
	}
	if header.Get("Content-Encoding") != "" || !compressibleContentType(header.Get("Content-Type")) {
		return false
	}
	if length, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && length < gzipMinBytes {
		return false
	}
	return true
}

func (g *gzipResponseWriter) startPassthrough() error {
	g.passthrough = true
	g.ResponseWriter.WriteHeader(g.status)
	if len(g.buffer) == 0 {
		return nil
	}
	_, err := g.ResponseWriter.Write(g.buffer)
	g.buffer = nil
	return err
}

func (g *gzipResponseWriter) startGzip() error {
	header := g.Header()
	header.Del("Content-Length")
	header.Set("Content-Encoding", "gzip")
	g.ResponseWriter.WriteHeader(g.status)
	g.gzip = gzipWriterPool.Get().(*gzip.Writer)
	g.gzip.Reset(g.ResponseWriter)
	_, err := g.gzip.Write(g.buffer)
	g.buffer = nil
	return err
}

func (g *gzipResponseWriter) finish() {
	if g.gzip != nil {
		_ = g.gzip.Close()
		g.gzip.Reset(io.Discard)
		gzipWriterPool.Put(g.gzip)
		g.gzip = nil
		return
	}
	if g.passthrough || g.status == 0 {
		return
	}
	_ = g.startPassthrough()
}
