package report

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
)

// kindSlot maps each block kind to its fixed categorical slot (1-based CSS
// var --s1..--s7). Fixed assignment, never cycled: a kind keeps its color no
// matter which kinds a session contains.
var kindSlot = map[string]int{
	"tool_result": 1, "assistant_text": 2, "thinking": 3,
	"user_text": 4, "meta_text": 5, "tool_use": 6, "image": 7,
}

const (
	c1W, c1H, padL, padR, padT, padB = 880, 260, 64, 16, 14, 30
	c2H                              = 220
)

// RenderSessionHTML writes a self-contained session page: stat tiles, the
// context-size timeline with compaction/invalidation markers, the
// new-content stacked composition, and the relief tables.
func RenderSessionHTML(w io.Writer, v *SessionView) error {
	data := struct {
		V              *SessionView
		Label          string
		Chart1, Chart2 template.HTML
		DataJSON       template.JS
		ReusePct       string
		HasEvents      bool
		KindRows       []kindRow
		SlotFor        func(string) int
	}{
		V:         v,
		Label:     sessionLabel(v),
		Chart1:    template.HTML(buildTimelineSVG(v)),
		Chart2:    template.HTML(buildCompositionSVG(v)),
		DataJSON:  template.JS(buildChartJSON(v)),
		ReusePct:  reusePct(v.Reuse),
		HasEvents: len(v.Events) > 0,
		KindRows:  kindRows(v),
		SlotFor:   func(k string) int { return kindSlot[k] },
	}
	t := template.Must(template.New("session").Funcs(template.FuncMap{
		"comma": comma,
		"slot":  func(k string) int { return kindSlot[k] },
		"trunc": func(s string) string { return truncate(s, 64) },
	}).Parse(sessionPageTmpl))
	return t.Execute(w, data)
}

func sessionLabel(v *SessionView) string {
	label := v.Meta.Project
	if v.Meta.Title != "" {
		label += " — " + v.Meta.Title
	} else if v.Meta.Slug != "" {
		label += " — " + v.Meta.Slug
	}
	return label
}

func reusePct(r float64) string {
	if r < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", 100*r)
}

type kindRow struct {
	Kind   string
	Slot   int
	Blocks int64
	Est    int64
	Share  string
}

func kindRows(v *SessionView) []kindRow {
	var total int64
	byKind := map[string]kindRow{}
	for _, r := range v.Kinds {
		total += r.EstTokens
		byKind[r.Group] = kindRow{Kind: r.Group, Slot: kindSlot[r.Group], Blocks: r.Blocks, Est: r.EstTokens}
	}
	var out []kindRow
	for _, k := range KindOrder {
		r, ok := byKind[k]
		if !ok {
			continue
		}
		if total > 0 {
			r.Share = fmt.Sprintf("%.1f%%", 100*float64(r.Est)/float64(total))
		}
		out = append(out, r)
	}
	return out
}

// niceCeil rounds up to a 1/2/5 × 10^k boundary for a clean axis maximum.
func niceCeil(v int64) int64 {
	if v <= 10 {
		return 10
	}
	mag := int64(1)
	for mag*10 < v {
		mag *= 10
	}
	for _, m := range []int64{1, 2, 5, 10} {
		if m*mag >= v {
			return m * mag
		}
	}
	return 10 * mag
}

func fmtAxis(v int64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%gM", float64(v)/1e6)
	case v >= 1_000:
		return fmt.Sprintf("%gk", float64(v)/1e3)
	default:
		return fmt.Sprintf("%d", v)
	}
}

func xPos(i, n int) float64 {
	if n <= 1 {
		return float64(padL)
	}
	return float64(padL) + float64(i)*float64(c1W-padL-padR)/float64(n-1)
}

// buildTimelineSVG renders the context-size-per-request line with an area
// wash, compaction dashes, and status-colored invalidation markers.
func buildTimelineSVG(v *SessionView) string {
	n := len(v.Points)
	if n == 0 {
		return `<p class="muted">no requests</p>`
	}
	var ymaxRaw int64
	for _, p := range v.Points {
		ymaxRaw = max(ymaxRaw, p.PromptSize)
	}
	ymax := niceCeil(ymaxRaw)
	y := func(val int64) float64 {
		return float64(padT) + (1-float64(val)/float64(ymax))*float64(c1H-padT-padB)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg id="c1" viewBox="0 0 %d %d" role="img" aria-label="context size per request">`, c1W, c1H)
	// gridlines + y labels (0, half, max)
	for _, gv := range []int64{0, ymax / 2, ymax} {
		gy := y(gv)
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="grid"/>`, padL, c1W-padR, gy, gy)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="axis" text-anchor="end">%s</text>`, padL-8, gy+4, fmtAxis(gv))
	}
	// x tick labels: ~8 across
	step := max(1, n/8)
	for i := 0; i < n; i += step {
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="axis" text-anchor="middle">%d</text>`, xPos(i, n), c1H-8, i+1)
	}
	// compaction markers behind the line
	for i, p := range v.Points {
		if p.Compaction {
			fmt.Fprintf(&b, `<line x1="%.1f" x2="%.1f" y1="%d" y2="%d" class="compact"/>`, xPos(i, n), xPos(i, n), padT, c1H-padB)
		}
	}
	// area + line
	var pts strings.Builder
	for i, p := range v.Points {
		fmt.Fprintf(&pts, "%.1f,%.1f ", xPos(i, n), y(p.PromptSize))
	}
	fmt.Fprintf(&b, `<polygon points="%.1f,%.1f %s%.1f,%.1f" class="area"/>`,
		xPos(0, n), y(0), pts.String(), xPos(n-1, n), y(0))
	fmt.Fprintf(&b, `<polyline points="%s" class="line"/>`, strings.TrimSpace(pts.String()))
	// invalidation markers on top, with a surface ring
	for i, p := range v.Points {
		if p.Event == nil {
			continue
		}
		cls := "ev-edit"
		if p.Event.Cause == "ttl_expiry" {
			cls = "ev-ttl"
		} else if p.Event.Cause == "compaction" {
			cls = "ev-compact"
		}
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4.5" class="ev %s"/>`, xPos(i, n), y(p.PromptSize), cls)
	}
	// hover layer
	fmt.Fprintf(&b, `<line id="xhair" x1="0" x2="0" y1="%d" y2="%d" class="xhair" visibility="hidden"/>`, padT, c1H-padB)
	fmt.Fprintf(&b, `<rect id="c1hit" x="%d" y="%d" width="%d" height="%d" fill="transparent"/>`, padL, padT, c1W-padL-padR, c1H-padT-padB)
	b.WriteString(`</svg>`)
	return b.String()
}

// buildCompositionSVG renders new extracted content per bucket as stacked
// bars in fixed kind order, with 2px surface gaps between segments.
func buildCompositionSVG(v *SessionView) string {
	nb := len(v.Buckets)
	if nb == 0 {
		return `<p class="muted">no extracted blocks</p>`
	}
	var ymaxRaw int64
	for _, bkt := range v.Buckets {
		var t int64
		for _, e := range bkt.ByKind {
			t += e
		}
		ymaxRaw = max(ymaxRaw, t)
	}
	ymax := niceCeil(ymaxRaw)
	plotH := float64(c2H - padT - padB)
	slotW := float64(c1W-padL-padR) / float64(nb)
	barW := max(2.0, slotW-2)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg id="c2" viewBox="0 0 %d %d" role="img" aria-label="new content per request bucket">`, c1W, c2H)
	for _, gv := range []int64{0, ymax / 2, ymax} {
		gy := float64(padT) + (1-float64(gv)/float64(ymax))*plotH
		fmt.Fprintf(&b, `<line x1="%d" x2="%d" y1="%.1f" y2="%.1f" class="grid"/>`, padL, c1W-padR, gy, gy)
		fmt.Fprintf(&b, `<text x="%d" y="%.1f" class="axis" text-anchor="end">%s</text>`, padL-8, gy+4, fmtAxis(gv))
	}
	for bi, bkt := range v.Buckets {
		x := float64(padL) + float64(bi)*slotW + 1
		yCursor := float64(padT) + plotH // baseline
		for _, kind := range KindOrder {
			est := bkt.ByKind[kind]
			if est == 0 {
				continue
			}
			h := float64(est) / float64(ymax) * plotH
			if h < 1 {
				h = 1
			}
			top := yCursor - h
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" class="s%d bar" data-b="%d"/>`,
				x, top, barW, max(0.5, h-2), kindSlot[kind], bi)
			yCursor = top
		}
		// x labels: every ~8th bucket, show starting request number
		if bi%max(1, nb/8) == 0 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%d" class="axis" text-anchor="middle">%d</text>`, x+barW/2, c2H-8, bkt.StartIdx+1)
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func buildChartJSON(v *SessionView) string {
	type jsPoint struct {
		TS     string `json:"ts"`
		Model  string `json:"model"`
		Prompt int64  `json:"prompt"`
		In     int64  `json:"in"`
		Rd     int64  `json:"rd"`
		Wr     int64  `json:"wr"`
		Out    int64  `json:"out"`
		Note   string `json:"note,omitempty"`
	}
	type jsBucket struct {
		Label string           `json:"label"`
		Kinds map[string]int64 `json:"kinds"`
	}
	pts := make([]jsPoint, len(v.Points))
	for i, p := range v.Points {
		pts[i] = jsPoint{TS: p.TS, Model: p.Model, Prompt: p.PromptSize,
			In: p.Input, Rd: p.CacheRead, Wr: p.CacheWrite, Out: p.Output}
		if p.Event != nil {
			pts[i].Note = p.Event.Cause + ": " + comma(p.Event.Shortfall) + " tokens re-processed"
		} else if p.Compaction {
			pts[i].Note = "compaction boundary"
		}
	}
	bks := make([]jsBucket, len(v.Buckets))
	for i, b := range v.Buckets {
		bks[i] = jsBucket{Label: fmt.Sprintf("requests %d–%d", b.StartIdx+1, b.EndIdx+1), Kinds: b.ByKind}
	}
	out, _ := json.Marshal(map[string]any{
		"points": pts, "buckets": bks, "kinds": KindOrder,
		"geom": map[string]int{"padL": padL, "padR": padR, "w": c1W},
	})
	return string(out)
}

const sessionPageTmpl = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tokenator — {{.Label}}</title>
<style>
.viz-root {
  color-scheme: light;
  --surface-1:#fcfcfb; --page:#f9f9f7; --ink-1:#0b0b0b; --ink-2:#52514e;
  --muted:#898781; --grid:#e1e0d9; --baseline:#c3c2b7; --border:rgba(11,11,11,.10);
  --s1:#2a78d6; --s2:#008300; --s3:#e87ba4; --s4:#eda100; --s5:#1baf7a; --s6:#eb6834; --s7:#4a3aa7;
  --ev-serious:#ec835a; --ev-critical:#d03b3b;
}
@media (prefers-color-scheme: dark) {
  :root:where(:not([data-theme="light"])) .viz-root {
    color-scheme: dark;
    --surface-1:#1a1a19; --page:#0d0d0d; --ink-1:#ffffff; --ink-2:#c3c2b7;
    --muted:#898781; --grid:#2c2c2a; --baseline:#383835; --border:rgba(255,255,255,.10);
    --s1:#3987e5; --s2:#008300; --s3:#d55181; --s4:#c98500; --s5:#199e70; --s6:#d95926; --s7:#9085e9;
  }
}
:root[data-theme="dark"] .viz-root {
  color-scheme: dark;
  --surface-1:#1a1a19; --page:#0d0d0d; --ink-1:#ffffff; --ink-2:#c3c2b7;
  --muted:#898781; --grid:#2c2c2a; --baseline:#383835; --border:rgba(255,255,255,.10);
  --s1:#3987e5; --s2:#008300; --s3:#d55181; --s4:#c98500; --s5:#199e70; --s6:#d95926; --s7:#9085e9;
}
.viz-root { margin:0; background:var(--page); color:var(--ink-1);
  font:14px/1.45 system-ui,-apple-system,"Segoe UI",sans-serif; padding:24px; }
.card { background:var(--surface-1); border:1px solid var(--border); border-radius:8px;
  padding:16px 20px; margin:0 auto 16px; max-width:960px; }
h1 { font-size:17px; margin:0 0 2px; } h2 { font-size:13px; color:var(--ink-2); margin:0 0 10px;
  font-weight:600; text-transform:uppercase; letter-spacing:.04em; }
.meta { color:var(--muted); font-size:12px; margin-bottom:0; }
.tiles { display:flex; flex-wrap:wrap; gap:12px; max-width:960px; margin:0 auto 16px; }
.tile { flex:1 1 130px; background:var(--surface-1); border:1px solid var(--border);
  border-radius:8px; padding:12px 16px; }
.tile .v { font-size:22px; font-weight:650; } .tile .l { font-size:11px; color:var(--muted);
  text-transform:uppercase; letter-spacing:.05em; margin-top:2px; }
.chartwrap { overflow-x:auto; }
svg { display:block; width:100%; height:auto; }
.grid { stroke:var(--grid); stroke-width:1; } .axis { fill:var(--muted); font-size:11px; }
.line { fill:none; stroke:var(--s1); stroke-width:2; stroke-linejoin:round; }
.area { fill:var(--s1); opacity:.08; }
.compact { stroke:var(--baseline); stroke-width:1.5; stroke-dasharray:3 3; }
.ev { stroke:var(--surface-1); stroke-width:2; }
.ev-ttl { fill:var(--ev-serious); } .ev-edit { fill:var(--ev-critical); } .ev-compact { fill:var(--baseline); }
.xhair { stroke:var(--baseline); stroke-width:1; stroke-dasharray:2 2; }
.bar:hover { opacity:.85; }
.s1{fill:var(--s1)} .s2{fill:var(--s2)} .s3{fill:var(--s3)} .s4{fill:var(--s4)}
.s5{fill:var(--s5)} .s6{fill:var(--s6)} .s7{fill:var(--s7)}
.legend { display:flex; flex-wrap:wrap; gap:14px; font-size:12px; color:var(--ink-2); margin-top:8px; }
.legend span { display:inline-flex; align-items:center; gap:5px; }
.sw { width:10px; height:10px; border-radius:2px; display:inline-block; }
.dot { width:9px; height:9px; border-radius:50%; display:inline-block; }
.dash { width:14px; height:0; border-top:2px dashed var(--baseline); display:inline-block; }
table { border-collapse:collapse; width:100%; font-size:13px; }
th { text-align:left; color:var(--muted); font-weight:600; font-size:11px;
  text-transform:uppercase; letter-spacing:.04em; padding:4px 12px 4px 0; }
td { padding:4px 12px 4px 0; border-top:1px solid var(--grid); }
td.n, th.n { text-align:right; font-variant-numeric:tabular-nums; }
.muted { color:var(--muted); }
#tip { position:fixed; pointer-events:none; background:var(--surface-1); color:var(--ink-1);
  border:1px solid var(--border); border-radius:6px; padding:8px 10px; font-size:12px;
  box-shadow:0 2px 8px rgba(0,0,0,.12); visibility:hidden; z-index:10; max-width:280px; }
#tip .t { color:var(--muted); font-size:11px; } #tip b { font-variant-numeric:tabular-nums; }
footer { max-width:960px; margin:0 auto; color:var(--muted); font-size:11px; }
</style></head>
<body class="viz-root">
<div class="card">
  <h1>{{.Label}}</h1>
  <p class="meta">session {{.V.Meta.Key}}{{if .V.Meta.Agent}} · agent: {{.V.Meta.Agent}}{{end}}
   · {{.V.Start}} → {{.V.End}} · {{range $i, $m := .V.Models}}{{if $i}}, {{end}}{{$m}}{{end}}</p>
</div>

<div class="tiles">
  <div class="tile"><div class="v">{{len .V.Points}}</div><div class="l">requests</div></div>
  <div class="tile"><div class="v">{{comma .V.TotalOut}}</div><div class="l">output tokens</div></div>
  <div class="tile"><div class="v">{{.ReusePct}}</div><div class="l">warm-prefix reuse</div></div>
  <div class="tile"><div class="v">{{comma .V.Shortfall}}</div><div class="l">re-processed after invalidations</div></div>
  <div class="tile"><div class="v">{{comma .V.NewEst}}</div><div class="l">est new content (blocks)</div></div>
</div>

<div class="card">
  <h2>Context size per request</h2>
  <div class="chartwrap">{{.Chart1}}</div>
  <div class="legend">
    <span><span class="sw" style="background:var(--s1)"></span>prompt tokens (input + cache read + cache write)</span>
    {{if .HasEvents}}<span><span class="dot" style="background:var(--ev-serious)"></span>⏱ ttl expiry</span>
    <span><span class="dot" style="background:var(--ev-critical)"></span>✕ history edit</span>{{end}}
    <span><span class="dash"></span>compaction boundary</span>
  </div>
</div>

<div class="card">
  <h2>New content entering context (est tokens, by kind)</h2>
  <div class="chartwrap">{{.Chart2}}</div>
  <div class="legend">
    {{range .KindRows}}<span><span class="sw" style="background:var(--s{{.Slot}})"></span>{{.Kind}}</span>{{end}}
  </div>
</div>

<div class="card">
  <h2>Composition (extracted blocks)</h2>
  <table><tr><th>kind</th><th class="n">blocks</th><th class="n">est tokens</th><th class="n">share</th></tr>
  {{range .KindRows}}<tr>
    <td><span class="sw" style="background:var(--s{{.Slot}})"></span> {{.Kind}}</td>
    <td class="n">{{comma .Blocks}}</td><td class="n">{{comma .Est}}</td><td class="n">{{.Share}}</td>
  </tr>{{end}}</table>
</div>

{{if .V.TopTools}}<div class="card">
  <h2>Top tools (result tokens)</h2>
  <table><tr><th>tool</th><th class="n">calls</th><th class="n">errors</th><th class="n">est tokens</th></tr>
  {{range .V.TopTools}}<tr><td>{{trunc .Group}}</td><td class="n">{{comma .Blocks}}</td>
  <td class="n">{{comma .Errors}}</td><td class="n">{{comma .EstTokens}}</td></tr>{{end}}</table>
</div>{{end}}

{{if .V.TopFiles}}<div class="card">
  <h2>Top files (result tokens)</h2>
  <table><tr><th>file</th><th class="n">reads</th><th class="n">est tokens</th></tr>
  {{range .V.TopFiles}}<tr><td>{{trunc .Group}}</td><td class="n">{{comma .Blocks}}</td>
  <td class="n">{{comma .EstTokens}}</td></tr>{{end}}</table>
</div>{{end}}

<footer>generated by tokenator · block token counts are bytes/4 estimates (read as shares);
request usage and cache splits are harness-reported ground truth</footer>
<div id="tip"></div>
<script>
const D = {{.DataJSON}};
const tip = document.getElementById('tip');
function showTip(html, ev) {
  tip.innerHTML = html; tip.style.visibility = 'visible';
  const pad = 14, vw = window.innerWidth, vh = window.innerHeight;
  let x = ev.clientX + pad, y = ev.clientY + pad;
  const r = tip.getBoundingClientRect();
  if (x + r.width > vw - 8) x = ev.clientX - r.width - pad;
  if (y + r.height > vh - 8) y = ev.clientY - r.height - pad;
  tip.style.left = x + 'px'; tip.style.top = y + 'px';
}
function hideTip() { tip.style.visibility = 'hidden'; }
const f = n => n.toLocaleString('en-US');
// chart 1: crosshair + nearest-point tooltip
const c1 = document.getElementById('c1');
if (c1 && D.points.length) {
  const hit = document.getElementById('c1hit'), xh = document.getElementById('xhair');
  const n = D.points.length, g = D.geom;
  const xAt = i => n <= 1 ? g.padL : g.padL + i * (g.w - g.padL - g.padR) / (n - 1);
  c1.addEventListener('mousemove', ev => {
    const box = c1.getBoundingClientRect();
    const sx = (ev.clientX - box.left) * g.w / box.width;
    let best = 0, bd = 1e18;
    for (let i = 0; i < n; i++) { const d = Math.abs(xAt(i) - sx); if (d < bd) { bd = d; best = i; } }
    const p = D.points[best], px = xAt(best);
    xh.setAttribute('x1', px); xh.setAttribute('x2', px); xh.setAttribute('visibility', 'visible');
    showTip('<div class="t">request ' + (best + 1) + ' · ' + p.ts + '</div>' +
      '<div>prompt <b>' + f(p.prompt) + '</b> · out <b>' + f(p.out) + '</b></div>' +
      '<div class="t">in ' + f(p.in) + ' · cache rd ' + f(p.rd) + ' · wr ' + f(p.wr) + '</div>' +
      (p.note ? '<div>' + p.note + '</div>' : ''), ev);
  });
  c1.addEventListener('mouseleave', () => { hideTip(); xh.setAttribute('visibility', 'hidden'); });
}
// chart 2: per-bar tooltip
document.querySelectorAll('#c2 .bar').forEach(r => {
  r.addEventListener('mousemove', ev => {
    const b = D.buckets[+r.dataset.b];
    let rows = '';
    for (const k of D.kinds) if (b.kinds[k]) rows += '<div class="t">' + k + ' <b>' + f(b.kinds[k]) + '</b></div>';
    showTip('<div>' + b.label + '</div>' + rows, ev);
  });
  r.addEventListener('mouseleave', hideTip);
});
</script>
</body></html>
`
