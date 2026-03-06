package middleware

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
)

type compressionEncoding string

const (
	compressionIdentity compressionEncoding = ""
	compressionGzip     compressionEncoding = "gzip"
	compressionBrotli   compressionEncoding = "br"
)

// JSONCompressionMiddleware compresses JSON management responses when the client
// explicitly advertises gzip or brotli support.
func JSONCompressionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		encoding := selectCompressionEncoding(c.GetHeader("Accept-Encoding"))
		if encoding == compressionIdentity || c.Request == nil || c.Request.Method == http.MethodHead {
			c.Next()
			return
		}

		writer := newJSONCompressionWriter(c.Writer, encoding)
		c.Writer = writer
		defer writer.Close()

		c.Next()
	}
}

type jsonCompressionWriter struct {
	gin.ResponseWriter
	encoding    compressionEncoding
	writer      io.Writer
	gzipWriter  *gzip.Writer
	brWriter    *brotli.Writer
	compressing bool
}

func newJSONCompressionWriter(w gin.ResponseWriter, encoding compressionEncoding) *jsonCompressionWriter {
	return &jsonCompressionWriter{
		ResponseWriter: w,
		encoding:       encoding,
	}
}

func (w *jsonCompressionWriter) WriteHeader(code int) {
	if !w.ResponseWriter.Written() && w.shouldCompress() {
		w.startCompression()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *jsonCompressionWriter) WriteHeaderNow() {
	if !w.ResponseWriter.Written() && w.shouldCompress() {
		w.startCompression()
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *jsonCompressionWriter) Write(data []byte) (int, error) {
	if !w.ResponseWriter.Written() {
		if w.shouldCompress() {
			w.startCompression()
		}
		w.ResponseWriter.WriteHeaderNow()
	}
	if !w.compressing {
		return w.ResponseWriter.Write(data)
	}
	return w.writer.Write(data)
}

func (w *jsonCompressionWriter) WriteString(s string) (int, error) {
	if !w.ResponseWriter.Written() {
		if w.shouldCompress() {
			w.startCompression()
		}
		w.ResponseWriter.WriteHeaderNow()
	}
	if !w.compressing {
		return w.ResponseWriter.WriteString(s)
	}
	return io.WriteString(w.writer, s)
}

func (w *jsonCompressionWriter) Flush() {
	if !w.ResponseWriter.Written() {
		if w.shouldCompress() {
			w.startCompression()
		}
		w.ResponseWriter.WriteHeaderNow()
	}
	if w.compressing {
		switch w.encoding {
		case compressionGzip:
			_ = w.gzipWriter.Flush()
		case compressionBrotli:
			_ = w.brWriter.Flush()
		}
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *jsonCompressionWriter) Close() {
	if w.compressing {
		switch w.encoding {
		case compressionGzip:
			_ = w.gzipWriter.Close()
		case compressionBrotli:
			_ = w.brWriter.Close()
		}
	}
	if !w.ResponseWriter.Written() {
		w.ResponseWriter.WriteHeaderNow()
	}
}

func (w *jsonCompressionWriter) shouldCompress() bool {
	if w == nil || w.compressing || w.encoding == compressionIdentity {
		return false
	}

	header := w.ResponseWriter.Header()
	if header.Get("Content-Encoding") != "" || header.Get("Content-Range") != "" {
		return false
	}
	if isAttachmentDisposition(header.Get("Content-Disposition")) {
		return false
	}
	return isJSONContentType(header.Get("Content-Type"))
}

func (w *jsonCompressionWriter) startCompression() {
	header := w.ResponseWriter.Header()
	header.Del("Content-Length")
	header.Set("Content-Encoding", string(w.encoding))
	addVaryHeader(header, "Accept-Encoding")

	switch w.encoding {
	case compressionBrotli:
		bw := brotli.NewWriterLevel(w.ResponseWriter, brotli.DefaultCompression)
		w.brWriter = bw
		w.writer = bw
	case compressionGzip:
		gw, _ := gzip.NewWriterLevel(w.ResponseWriter, gzip.DefaultCompression)
		w.gzipWriter = gw
		w.writer = gw
	default:
		return
	}
	w.compressing = true
}

func isJSONContentType(raw string) bool {
	contentType := strings.ToLower(strings.TrimSpace(raw))
	if contentType == "" {
		return false
	}
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	return contentType == "application/json" || strings.HasSuffix(contentType, "+json")
}

func isAttachmentDisposition(raw string) bool {
	return strings.Contains(strings.ToLower(raw), "attachment")
}

func addVaryHeader(header http.Header, value string) {
	if header == nil {
		return
	}
	existing := header.Values("Vary")
	if len(existing) == 0 {
		header.Set("Vary", value)
		return
	}

	seen := map[string]struct{}{}
	values := make([]string, 0, len(existing)+1)
	for _, entry := range existing {
		for _, part := range strings.Split(entry, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			key := strings.ToLower(trimmed)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			values = append(values, trimmed)
		}
	}
	key := strings.ToLower(value)
	if _, ok := seen[key]; !ok {
		values = append(values, value)
	}
	header.Set("Vary", strings.Join(values, ", "))
}

func selectCompressionEncoding(acceptEncoding string) compressionEncoding {
	type candidate struct {
		encoding compressionEncoding
		q        float64
		order    int
	}

	best := candidate{encoding: compressionIdentity}
	wildcardQ := -1.0

	for idx, part := range strings.Split(acceptEncoding, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}

		name := token
		q := 1.0
		if semi := strings.Index(token, ";"); semi >= 0 {
			name = strings.TrimSpace(token[:semi])
			params := strings.Split(token[semi+1:], ";")
			for _, param := range params {
				param = strings.TrimSpace(param)
				if !strings.HasPrefix(strings.ToLower(param), "q=") {
					continue
				}
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64); err == nil {
					q = parsed
				}
			}
		}

		switch strings.ToLower(name) {
		case string(compressionBrotli):
			best = pickBetterCandidate(best, candidate{encoding: compressionBrotli, q: q, order: idx})
		case string(compressionGzip):
			best = pickBetterCandidate(best, candidate{encoding: compressionGzip, q: q, order: idx})
		case "*":
			wildcardQ = q
		}
	}

	if best.encoding != compressionIdentity && best.q > 0 {
		return best.encoding
	}
	if wildcardQ > 0 {
		return compressionGzip
	}
	return compressionIdentity
}

func pickBetterCandidate(current, next struct {
	encoding compressionEncoding
	q        float64
	order    int
}) struct {
	encoding compressionEncoding
	q        float64
	order    int
} {
	if next.q <= 0 {
		return current
	}
	if current.encoding == compressionIdentity || next.q > current.q {
		return next
	}
	if next.q < current.q {
		return current
	}
	if next.encoding == compressionBrotli && current.encoding != compressionBrotli {
		return next
	}
	if next.order < current.order {
		return next
	}
	return current
}
