package middleware

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
)

func TestJSONCompressionMiddleware_CompressesJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(JSONCompressionMiddleware())
	router.GET("/json", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": strings.Repeat("a", 1024),
		})
	})

	testCases := []struct {
		name           string
		acceptEncoding string
		wantEncoding   string
		readBody       func(*testing.T, []byte) []byte
	}{
		{
			name:           "brotli preferred",
			acceptEncoding: "br, gzip",
			wantEncoding:   "br",
			readBody: func(t *testing.T, body []byte) []byte {
				t.Helper()
				return mustReadAll(t, brotli.NewReader(bytes.NewReader(body)))
			},
		},
		{
			name:           "gzip fallback",
			acceptEncoding: "gzip",
			wantEncoding:   "gzip",
			readBody: func(t *testing.T, body []byte) []byte {
				t.Helper()
				reader, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatalf("failed to create gzip reader: %v", err)
				}
				defer func() { _ = reader.Close() }()
				return mustReadAll(t, reader)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/json", nil)
			req.Header.Set("Accept-Encoding", tc.acceptEncoding)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("unexpected status: got %d", rec.Code)
			}
			if got := rec.Header().Get("Content-Encoding"); got != tc.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tc.wantEncoding)
			}
			if vary := rec.Header().Get("Vary"); vary != "Accept-Encoding" {
				t.Fatalf("Vary = %q, want %q", vary, "Accept-Encoding")
			}

			raw := tc.readBody(t, rec.Body.Bytes())
			var payload map[string]string
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("failed to decode response: %v", err)
			}
			if len(payload["message"]) != 1024 {
				t.Fatalf("unexpected message length: got %d", len(payload["message"]))
			}
		})
	}
}

func TestJSONCompressionMiddleware_SkipsNonJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(JSONCompressionMiddleware())
	router.GET("/yaml", func(c *gin.Context) {
		c.Header("Content-Type", "application/yaml; charset=utf-8")
		c.String(http.StatusOK, "foo: bar\n")
	})

	req := httptest.NewRequest(http.MethodGet, "/yaml", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if body := rec.Body.String(); body != "foo: bar\n" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	return data
}
