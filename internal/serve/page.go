package serve

import (
	"html/template"
	"net/http"
	"strings"
)

// Every page renders against a page context: where the site lives (Base) and
// whether it is shown inside the router dashboard's Tokens tab (Embed).
//
// Base path: the dashboard reverse-proxies tokenator at /tokens/ and tells
// it so with X-Forwarded-Prefix (per request, the normal case), or the
// operator runs `serve --base-path /tokens` behind a prefix-preserving proxy.
// Every URL a page emits goes through {{.Page.Base}} so nothing escapes the
// prefix; the route table itself stays root-relative because the proxy
// strips the prefix (and Handler also mounts it under --base-path).
//
// Embedded mode: ?embed=1 (sticky through a cookie so links keep it), or the
// header X-Tokenator-Embed: 1. It drops tokenator's own chrome (the page
// header links), restyles the palette to the dashboard's, and turns links
// that would leave tokenator (router requests for this session, the catalog
// for a model) into postMessage jumps the dashboard shell routes through its
// hash grammar (#requests?session=…, #catalog?model=…). Standalone pages are
// untouched.
type page struct {
	Base  string // "" or "/tokens" — never with a trailing slash
	Embed bool
}

const (
	embedCookie = "tokenator_embed"
	embedHeader = "X-Tokenator-Embed"
	prefixHdr   = "X-Forwarded-Prefix"
)

// pageOf resolves the page context and, when ?embed= is present, sets or
// clears the sticky cookie.
func (s *Server) pageOf(w http.ResponseWriter, r *http.Request) page {
	p := page{Base: cleanBase(s.BasePath)}
	if h := cleanBase(r.Header.Get(prefixHdr)); h != "" {
		p.Base = h
	}
	switch r.URL.Query().Get("embed") {
	case "1", "true":
		p.Embed = true
		http.SetCookie(w, &http.Cookie{Name: embedCookie, Value: "1", Path: "/", SameSite: http.SameSiteLaxMode})
	case "0", "false":
		http.SetCookie(w, &http.Cookie{Name: embedCookie, Value: "", Path: "/", MaxAge: -1, SameSite: http.SameSiteLaxMode})
	default:
		if r.Header.Get(embedHeader) == "1" {
			p.Embed = true
		} else if c, err := r.Cookie(embedCookie); err == nil && c.Value == "1" {
			p.Embed = true
		}
	}
	return p
}

// cleanBase normalises a prefix: "" or "/x/y" with no trailing slash.
func cleanBase(b string) string {
	b = strings.TrimSpace(b)
	b = strings.TrimRight(b, "/")
	if b == "" {
		return ""
	}
	if !strings.HasPrefix(b, "/") {
		b = "/" + b
	}
	return b
}

// EmbedCSS repaints tokenator's palette variables with the router
// dashboard's (dashboard.css: --bg, --surface, --border, --text, --text-dim,
// --accent) so a framed page reads as part of the shell. Chips and chart
// slots keep tokenator's dark-scheme colours, which already sit on a dark
// ground. Only applied in embedded mode.
const embedCSS = `/* tokenator embed: the router dashboard's palette */
.viz-root, :root[data-theme="dark"] .viz-root, :root:where(:not([data-theme="light"])) .viz-root {
  color-scheme: dark;
  --page:#0f1117; --surface-1:#1a1d27; --border:#2a2d3a; --grid:#2a2d3a; --baseline:#3a3e4d;
  --ink-1:#e1e4ed; --ink-2:#b7bbc9; --muted:#8b8fa3;
  --s1:#6c8cff; --s2:#3fb950; --s3:#d55181; --s4:#c98500; --s5:#199e70; --s6:#d95926; --s7:#9085e9;
}
.viz-root { padding:12px 16px; }
.viz-root .card { max-width:none; }
`

// embedJS forwards jump links to the parent shell. The shell listens for
// {type:"tokenator-jump", tab, session?, model?} and navigates its hash;
// when nothing is listening the fallback sets the parent's hash directly.
const embedJS = `document.addEventListener("click", function (ev) {
  var a = ev.target.closest && ev.target.closest("a[data-jump]");
  if (!a) return;
  ev.preventDefault();
  var msg = {type: "tokenator-jump", tab: a.dataset.jump};
  if (a.dataset.session) msg.session = a.dataset.session;
  if (a.dataset.model) msg.model = a.dataset.model;
  try { window.parent.postMessage(msg, "*"); } catch (e) {}
  if (window.parent === window) return;
  try {
    var q = msg.session ? "session=" + encodeURIComponent(msg.session) : msg.model ? "model=" + encodeURIComponent(msg.model) : "";
    window.parent.location.hash = "#" + msg.tab + (q ? "?" + q : "");
  } catch (e) {}
});`

// embedHead is the <head> fragment embedded pages add: the palette and the
// jump script.
func embedHead(p page) template.HTML {
	if !p.Embed {
		return ""
	}
	return template.HTML("<style>" + embedCSS + "</style><script>" + embedJS + "</script>")
}
