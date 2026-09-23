package serve

import (
	"html/template"

	"github.com/erewhon/tokenator/internal/report"
)

// serveCSS extends report.BaseCSS with browser/transcript-specific styling.
// Palette slots s1..s7 keep the same kind assignment as every other view.
const serveCSS = `
.wrap { max-width:960px; margin:0 auto; }
form.filters { display:flex; flex-wrap:wrap; gap:8px; align-items:center; }
form.filters input[type=text] { flex:1 1 260px; padding:7px 10px; font:inherit;
  color:var(--ink-1); background:var(--page); border:1px solid var(--border); border-radius:6px; }
form.filters select { padding:6px 8px; font:inherit; color:var(--ink-1);
  background:var(--page); border:1px solid var(--border); border-radius:6px; }
form.filters button { padding:7px 14px; font:inherit; font-weight:600; color:var(--surface-1);
  background:var(--s1); border:0; border-radius:6px; cursor:pointer; }
a { color:var(--s1); text-decoration:none; } a:hover { text-decoration:underline; }
.chip { display:inline-block; font-size:10px; font-weight:600; letter-spacing:.04em;
  text-transform:uppercase; padding:1px 7px; border-radius:9px; color:var(--surface-1); }
.chip.s1{background:var(--s1)} .chip.s2{background:var(--s2)} .chip.s3{background:var(--s3)}
.chip.s4{background:var(--s4)} .chip.s5{background:var(--s5)} .chip.s6{background:var(--s6)}
.chip.s7{background:var(--s7)} .chip.s0{background:var(--baseline);color:var(--ink-1)}
mark { background:var(--s4); color:var(--ink-1); border-radius:2px; padding:0 1px; }
.snips { margin:8px 0 0; padding:0; list-style:none; }
.snips li { margin:6px 0; font-size:12.5px; color:var(--ink-2); }
.snips .t { color:var(--muted); font-size:11px; margin-right:6px; }
.entry { border-top:1px solid var(--grid); padding:8px 0; }
.entry:target { background:color-mix(in srgb, var(--s4) 12%, transparent); border-radius:6px; }
.ehead { display:flex; gap:10px; align-items:baseline; font-size:12px; color:var(--muted);
  flex-wrap:wrap; }
.ehead .tool { color:var(--ink-2); font-weight:600; }
.entry pre { margin:6px 0 0; white-space:pre-wrap; overflow-wrap:anywhere; font-size:12.5px;
  line-height:1.5; font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  color:var(--ink-1); }
.entry.err pre { color:var(--ev-critical); }
details summary { cursor:pointer; color:var(--muted); font-size:12px; margin-top:4px; }
.pagenote { color:var(--muted); font-size:12px; margin:6px 0 0; }
td a.title { color:var(--ink-1); font-weight:550; }
.agent { color:var(--muted); font-size:11px; }
td.when { white-space:nowrap; }
`

// favicon is an inline SVG ticket emoji — avoids a 404 round-trip.
const favicon = `<link rel="icon" href="data:image/svg+xml,<svg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 100 100%22><text y=%22.9em%22 font-size=%2290%22>🎟</text></svg>">`

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tokenator — sessions</title>
` + favicon + `
<style>` + report.BaseCSS + serveCSS + `</style></head>
<body class="viz-root">
<div class="card">
  <h1>tokenator — sessions</h1>
  <form class="filters" method="get" action="/">
    <input type="text" name="q" value="{{.Query}}" placeholder="search session content…" autofocus>
    <select name="project">
      <option value="">all projects</option>
      {{$p := .Project}}{{range .Projects}}<option{{if eq . $p}} selected{{end}}>{{.}}</option>{{end}}
    </select>
    <select name="since">
      {{$s := .Since}}
      <option value=""{{if eq $s ""}} selected{{end}}>all time</option>
      <option value="24h"{{if eq $s "24h"}} selected{{end}}>last 24h</option>
      <option value="7d"{{if eq $s "7d"}} selected{{end}}>last 7d</option>
      <option value="30d"{{if eq $s "30d"}} selected{{end}}>last 30d</option>
    </select>
    <button type="submit">{{if .Searched}}search{{else}}filter{{end}}</button>
  </form>
  {{if .Searched}}<p class="pagenote">{{len .Rows}} session{{if ne (len .Rows) 1}}s{{end}} matched
    · scanned {{.Scanned}} newest candidate sessions in {{.ScanMS}}ms{{if .Truncated}}
    · candidate list hit the {{.Limit}}-session scan cap — narrow with project/since to reach older sessions{{end}}</p>{{end}}
</div>

{{if .Searched}}
  {{range .Rows}}
  <div class="card">
    <p style="margin:0"><a class="title" href="/session/{{.Key}}/transcript?q={{$.Query}}">{{if .Title}}{{.Title}}{{else}}{{.Key}}{{end}}</a>
      {{if .Agent}}<span class="agent">agent: {{.Agent}}</span>{{end}}</p>
    <p class="meta">{{.When}} · {{.Project}} · {{.Requests}} req · {{.TotalToks}} toks
      · {{if .Matches}}{{.Matches}} match{{if ne .Matches 1}}es{{end}}{{else}}title match{{end}}
      · <a href="/session/{{.Key}}">profile</a>
      {{if .ScanErr}}· <span class="agent">transcript unavailable: {{.ScanErr}}</span>{{end}}</p>
    {{$row := .}}{{if .Hits}}<ul class="snips">
      {{range .Hits}}<li><span class="t">{{.TS}}</span><span class="chip s{{.Slot}}">{{.Kind}}</span>
        {{if .Tool}}<span class="t">{{.Tool}}</span>{{end}}
        <a href="/session/{{$row.Key}}/transcript?q={{$.Query}}#e{{.Anchor}}">¶</a>
        {{.Snippet}}</li>{{end}}
    </ul>{{end}}
  </div>
  {{end}}
  {{if not .Rows}}<div class="card"><p class="muted">no matches</p></div>{{end}}
{{else}}
<div class="card">
  <table>
    <tr><th>when</th><th>project</th><th>session</th><th class="n">req</th>
      <th class="n">tokens</th><th class="n">out</th><th></th></tr>
    {{range .Rows}}<tr>
      <td class="when">{{.When}}</td>
      <td>{{.Project}}</td>
      <td><a class="title" href="/session/{{.Key}}/transcript">{{if .Title}}{{.Title}}{{else}}{{.Key}}{{end}}</a>
        {{if .Agent}}<span class="agent">· {{.Agent}}</span>{{end}}</td>
      <td class="n">{{.Requests}}</td>
      <td class="n">{{.TotalToks}}</td>
      <td class="n">{{.OutToks}}</td>
      <td><a href="/session/{{.Key}}">profile</a></td>
    </tr>{{end}}
  </table>
  {{if not .Rows}}<p class="muted">no sessions — run tokenator ingest first</p>{{end}}
</div>
{{end}}
<footer>tokenator serve · content search reads the harness transcript files on demand; nothing leaves this machine</footer>
</body></html>
`))

var modelTmpl = template.Must(template.New("model").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tokenator — model {{.Name}}</title>
` + favicon + `
<style>` + report.BaseCSS + serveCSS + `</style></head>
<body class="viz-root">
<div class="card">
  <p class="meta" style="margin-bottom:6px"><a href="/">&larr; sessions</a></p>
  <h1>model {{.Name}}</h1>
  <p class="meta">{{len .Rows}} session{{if ne (len .Rows) 1}}s{{end}}{{if .Trimmed}} (newest {{.Limit}}){{end}}
    · router: {{.Totals.Requests}} req · {{.GwToks}} toks · {{.GwOut}} out{{if .Totals.Unpaired}} · {{.Totals.Unpaired}} not attributed to a session{{end}}</p>
</div>
<div class="card">
  {{if .Rows}}<table>
    <tr><th>last</th><th>project</th><th>session</th><th class="n">req</th>
      <th class="n">tokens</th><th class="n">out</th><th>from</th><th></th></tr>
    {{range .Rows}}<tr>
      <td class="when">{{.When}}</td>
      <td>{{.Project}}</td>
      <td><a class="title" href="/session/{{.Key}}/transcript">{{if .Title}}{{.Title}}{{else}}{{.Key}}{{end}}</a>
        {{if .Agent}}<span class="agent">· {{.Agent}}</span>{{end}}</td>
      <td class="n">{{.Requests}}</td>
      <td class="n">{{.TotalToks}}</td>
      <td class="n">{{.OutToks}}</td>
      <td><span class="agent">{{.Via}}</span></td>
      <td><a href="/session/{{.Key}}">profile</a></td>
    </tr>{{end}}
  </table>{{else}}<p class="muted">no session used {{.Name}} — neither a transcript nor the router log names it</p>{{end}}
</div>
<footer>tokenator serve · matched on the harness's model name, the router alias the caller sent, and the registry id it resolved to</footer>
</body></html>
`))

var transcriptTmpl = template.Must(template.New("transcript").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tokenator — transcript {{.Meta.Key}}</title>
` + favicon + `
<style>` + report.BaseCSS + serveCSS + `</style></head>
<body class="viz-root">
<div class="card">
  <p class="meta" style="margin-bottom:6px"><a href="/">&larr; sessions</a> &middot;
    <a href="/session/{{.Meta.Key}}">profile</a>{{if .MonitorURL}} &middot; <a href="{{.MonitorURL}}" title="agent-monitor board">monitor</a>{{end}}</p>
  <h1>{{.Meta.Project}}{{if .Meta.Title}} — {{.Meta.Title}}{{end}}</h1>
  <p class="meta">session {{.Meta.Key}}{{if .Meta.Agent}} · agent: {{.Meta.Agent}}{{end}}
    · {{.Total}} entries</p>
  <form class="filters" method="get" style="margin-top:10px">
    <input type="text" name="q" value="{{.Query}}" placeholder="highlight in this session…">
    <button type="submit">highlight</button>
  </form>
</div>

<div class="card">
  {{if .Err}}<p class="muted">{{.Err}}</p>{{end}}
  {{range .Entries}}
  <div class="entry{{if .IsError}} err{{end}}" id="e{{.Idx}}">
    <div class="ehead">
      <a href="#e{{.Idx}}" class="t">¶{{.Idx}}</a>
      <span class="chip s{{.Slot}}">{{.Kind}}</span>
      {{if .Tool}}<span class="tool">{{.Tool}}</span>{{end}}
      {{if .IsError}}<span class="chip s0">error</span>{{end}}
      <span>{{.TS}}</span>
    </div>
    {{if .Collapse}}
    <details{{if .Open}} open{{end}}><summary>{{.Chars}} chars</summary><pre>{{.Body}}</pre></details>
    {{else}}<pre>{{.Body}}</pre>{{end}}
  </div>
  {{end}}
</div>
<footer>tokenator serve · rendered from the harness transcript files</footer>
</body></html>
`))
