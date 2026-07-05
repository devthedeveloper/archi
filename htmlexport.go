package main

import (
	"os"
	"strings"
)

// renderHTML wraps a Markdown design document in a self-contained HTML page that
// renders the prose and draws the Mermaid diagrams in any browser. Marked and
// Mermaid load from jsDelivr, so viewing needs a network connection.
func renderHTML(title, md string) string {
	// Neutralize any closing script tag inside the document body.
	safe := strings.ReplaceAll(md, "</script>", "<\\/script>")
	return `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + htmlEscape(title) + `</title>
<style>
  body{max-width:820px;margin:2.5rem auto;padding:0 1.25rem;
    font:16px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;color:#1b2b34}
  h1,h2,h3{line-height:1.25;margin:1.6em 0 .5em}
  h2{border-bottom:1px solid #e6eef2;padding-bottom:.2em}
  code{background:#eef4f7;padding:.15em .4em;border-radius:4px;font-size:.9em}
  pre{background:#0f2027;color:#d4e3eb;padding:1rem;border-radius:8px;overflow:auto}
  pre code{background:none;padding:0;color:inherit}
  table{border-collapse:collapse;width:100%;margin:1em 0}
  th,td{border:1px solid #d6e4ea;padding:.5em .7em;text-align:left}
  th{background:#eaf7fb}
  .mermaid{background:#fff;text-align:center;margin:1.2em 0}
  a{color:#007d9c}
</style></head>
<body>
<article id="out"></article>
<script id="doc" type="text/markdown">` + safe + `</script>
<script type="module">
import mermaid from 'https://cdn.jsdelivr.net/npm/mermaid@10/dist/mermaid.esm.min.mjs';
import { marked } from 'https://cdn.jsdelivr.net/npm/marked@12/lib/marked.esm.js';
const src = document.getElementById('doc').textContent;
document.getElementById('out').innerHTML = marked.parse(src);
document.querySelectorAll('code.language-mermaid').forEach(el => {
  const d = document.createElement('pre');
  d.className = 'mermaid';
  d.textContent = el.textContent;
  el.parentElement.replaceWith(d);
});
mermaid.initialize({ startOnLoad: false, theme: 'neutral' });
mermaid.run();
</script>
</body></html>
`
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// writeHTMLFile renders md to a standalone HTML file.
func writeHTMLFile(path, title, md string) error {
	return os.WriteFile(path, []byte(renderHTML(title, md)), 0o644)
}
