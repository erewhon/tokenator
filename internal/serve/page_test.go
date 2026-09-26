package serve

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// getWith issues a request with extra headers and returns the recorder.
func getWith(t *testing.T, srv *Server, path string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

var pages = []string{"/", "/session/abc", "/session/abc/transcript", "/model/claude-sonnet-5"}

// Every site-relative URL a page emits must stay under the announced prefix.
var escapedLink = regexp.MustCompile(`(href|src|action)="/(?:[^t"]|t[^o"]|to[^k"])`)

func TestBasePathFromForwardedPrefix(t *testing.T) {
	srv := fixtureServer(t)
	for _, p := range pages {
		rec := getWith(t, srv, p, map[string]string{prefixHdr: "/tokens"})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if m := escapedLink.FindString(body); m != "" {
			t.Errorf("%s: a link escaped the prefix: %q", p, m)
		}
		if !strings.Contains(body, `"/tokens/`) {
			t.Errorf("%s: no prefixed link at all:\n%s", p, body)
		}
	}
	body := getWith(t, srv, "/", map[string]string{prefixHdr: "/tokens"}).Body.String()
	if !strings.Contains(body, `action="/tokens/"`) || !strings.Contains(body, `href="/tokens/session/abc/transcript"`) {
		t.Errorf("index links not prefixed:\n%s", body)
	}
}

func TestBasePathFlagMountsUnderPrefixToo(t *testing.T) {
	srv := fixtureServer(t)
	srv.BasePath = "/tokens/"
	rec := getWith(t, srv, "/tokens/session/abc", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `href="/tokens/session/abc/transcript"`) {
		t.Fatalf("prefixed mount: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := getWith(t, srv, "/tokens", nil); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/tokens/" {
		t.Errorf("bare prefix: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Root mount keeps working (the stripping proxy case) and its links carry
	// the flag prefix.
	if rec := getWith(t, srv, "/session/abc", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `href="/tokens/"`) {
		t.Errorf("root mount with base flag: %d", rec.Code)
	}
}

func TestStandaloneUnchanged(t *testing.T) {
	srv := fixtureServer(t)
	for _, p := range pages {
		body := get(t, srv, p)
		if strings.Contains(body, "tokenator embed") || strings.Contains(body, "data-jump") {
			t.Errorf("%s: embed artifacts in a standalone page", p)
		}
	}
	body := get(t, srv, "/")
	if !strings.Contains(body, `action="/"`) || !strings.Contains(body, `href="/session/abc/transcript"`) || !strings.Contains(body, "<h1>tokenator — sessions</h1>") || !strings.Contains(body, "<footer>") {
		t.Errorf("standalone index changed:\n%s", body)
	}
	if body := get(t, srv, "/session/abc/transcript"); !strings.Contains(body, "&larr; sessions") {
		t.Error("standalone transcript lost its nav")
	}
}

func TestEmbedMode(t *testing.T) {
	srv := fixtureServer(t)
	rec := getWith(t, srv, "/session/abc/transcript?embed=1", nil)
	body := rec.Body.String()
	if strings.Contains(body, "&larr; sessions") || strings.Contains(body, "<footer>") {
		t.Error("embedded transcript still has tokenator's chrome")
	}
	if !strings.Contains(body, "tokenator embed") || !strings.Contains(body, `data-jump="requests" data-session="abc"`) || !strings.Contains(body, "tokenator-jump") {
		t.Errorf("embedded transcript lacks the palette, jump link or script:\n%s", body)
	}
	cookie := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(cookie, embedCookie+"=1") {
		t.Errorf("?embed=1 should set the sticky cookie, got %q", cookie)
	}
	// Sticky: the cookie alone embeds; the header alone embeds.
	for name, hdr := range map[string]map[string]string{
		"cookie": {"Cookie": embedCookie + "=1"},
		"header": {embedHeader: "1"},
	} {
		b := getWith(t, srv, "/", hdr).Body.String()
		if !strings.Contains(b, "tokenator embed") || strings.Contains(b, "<h1>tokenator — sessions</h1>") {
			t.Errorf("%s: index should be embedded", name)
		}
	}
	// ?embed=0 clears it.
	rec = getWith(t, srv, "/?embed=0", map[string]string{"Cookie": embedCookie + "=1"})
	if strings.Contains(rec.Body.String(), "tokenator embed") || !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Errorf("?embed=0 should render standalone and clear the cookie: %q", rec.Header().Get("Set-Cookie"))
	}
	// Profile and model pages: chrome off, jumps on.
	body = getWith(t, srv, "/session/abc", map[string]string{embedHeader: "1"}).Body.String()
	if strings.Contains(body, "&larr; sessions") || !strings.Contains(body, `data-jump="requests"`) || !strings.Contains(body, "tokenator embed") {
		t.Errorf("embedded profile:\n%s", body[:min(600, len(body))])
	}
	body = getWith(t, srv, "/model/claude-sonnet-5", map[string]string{embedHeader: "1", prefixHdr: "/tokens"}).Body.String()
	if !strings.Contains(body, `data-jump="catalog" data-model="claude-sonnet-5"`) || strings.Contains(body, "&larr; sessions") {
		t.Errorf("embedded model page:\n%s", body[:min(600, len(body))])
	}
}
