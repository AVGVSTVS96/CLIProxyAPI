package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClaudeWebUILoginFromAnotherDeviceUsesHostedCodePage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	router := gin.New()
	router.GET("/anthropic-auth-url", handler.RequestAnthropicToken)

	cases := []struct {
		name         string
		remoteAddr   string
		query        string
		wantRedirect string
		wantFlow     string
	}{
		{"phone over the tailnet", "100.64.0.7:52000", "?is_webui=true", claude.ManualRedirectURI, "code"},
		{"API caller", "100.64.0.7:52000", "", claude.RedirectURI, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/anthropic-auth-url"+tc.query, nil)
			req.RemoteAddr = tc.remoteAddr
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var payload struct {
				URL   string `json:"url"`
				State string `json:"state"`
				Flow  string `json:"flow"`
			}
			if errDecode := json.Unmarshal(w.Body.Bytes(), &payload); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			defer CancelOAuthSession(payload.State)
			authURL, errParse := url.Parse(payload.URL)
			if errParse != nil {
				t.Fatalf("parse url: %v", errParse)
			}
			if got := authURL.Query().Get("redirect_uri"); got != tc.wantRedirect {
				t.Fatalf("redirect_uri=%q, want %q", got, tc.wantRedirect)
			}
			if payload.Flow != tc.wantFlow {
				t.Fatalf("flow=%q, want %q", payload.Flow, tc.wantFlow)
			}
		})
	}
}

func TestBrowserOnThisMachine(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:52000":  true,
		"[::1]:52000":      true,
		"100.64.0.7:52000": false,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.RemoteAddr = addr
		if got := browserOnThisMachine(c); got != want {
			t.Errorf("browserOnThisMachine(%s)=%v, want %v", addr, got, want)
		}
	}
}
