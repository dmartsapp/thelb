#!/usr/bin/env python3
"""Build the theLB documentation site.

    python3 docs/build.py            # render docs/src/*.md (+ ../CHANGELOG.md) -> docs/*.html
    python3 docs/build.py --check    # build in memory; fail if committed pages are stale or a link is broken
                                     # (links between pages, AND published-site URLs in README.md,
                                     # CHANGELOG.md and the page sources - see check_site_urls)

Standard library only, on purpose: the site is plain static files served by
GitHub Pages, and the project has no Node/Ruby/pip toolchain to maintain.
The Markdown dialect is a small, deliberate subset (see docs/src/tech-docs.md):

    ---                       front matter: title, lead, description, section, order, nav
    ## / ### / ####           headings (h2/h3 get anchors and feed the "On this page" list)
    - / 1.                    lists (nesting by indentation)
    | a | b |                 tables
    ```bash / text / json     fenced code, with light syntax colouring
    :::note Title  ...  :::   callouts: note, tip, warning
    :::html ... :::           raw HTML passthrough (diagrams, cards)
    :::example <file>         a file the binary wrote - docs/examples/<file>, from
    caption                   thelb generate k8s (internal/cli's test keeps it so) - as a code
    :::                       block, with a link to the file itself
    :::screen <name>          a screenshot: docs/screens/<name>.txt - captured from the real
    caption                   binary by docs/screens/capture.sh - drawn as a terminal window,
    :::                       docs/assets/screens/<name>.svg (screen_svg)
    **bold** *italic* `code` [link](page.md#anchor)   inline
"""
import html
import re
import sys
from pathlib import Path

DOCS = Path(__file__).resolve().parent
SRC = DOCS / "src"
ROOT = DOCS.parent
REPO_URL = "https://github.com/dmartsapp/thelb"
# Where GitHub Pages publishes this repository. Pages serves the repository root as-is
# (.nojekyll), so a URL under SITE_URL names a file at the same path in the repo.
SITE_URL = "https://dmartsapp.github.io/thelb"

# Sidebar order. A page's front matter `section` picks its group and `order`
# its position inside it.
SECTIONS = ["Get started", "Guides", "Reference", "Technical"]


# --------------------------------------------------------------------------
# Inline Markdown
# --------------------------------------------------------------------------

def slugify(text):
    text = re.sub(r"<[^>]+>", "", text)
    text = re.sub(r"[`*_]", "", text)
    text = re.sub(r"[^a-zA-Z0-9]+", "-", text).strip("-").lower()
    return text or "section"


def fix_href(url):
    """Sources link to each other as .md (so they also work on GitHub); the site uses .html."""
    if re.match(r"^(https?:|mailto:|#|/)", url):
        return url
    return re.sub(r"\.md(#|$)", r".html\1", url)


def inline(text):
    spans = []

    def stash(m):
        spans.append("<code>" + html.escape(m.group(1), quote=False) + "</code>")
        return "\x00%d\x00" % (len(spans) - 1)

    text = re.sub(r"`([^`]+)`", stash, text)
    text = html.escape(text, quote=False)
    text = re.sub(
        r"\[([^\]]+)\]\(([^)\s]+)\)",
        lambda m: '<a href="%s">%s</a>' % (fix_href(m.group(2)), m.group(1)),
        text,
    )
    text = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", text)
    text = re.sub(r"(?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*])", r"<em>\1</em>", text)
    return re.sub(r"\x00(\d+)\x00", lambda m: spans[int(m.group(1))], text)


# --------------------------------------------------------------------------
# Syntax colouring for code blocks (regex tokenisers; no external deps)
# --------------------------------------------------------------------------

def _tokenise(code, pattern):
    out, pos = [], 0
    for m in pattern.finditer(code):
        out.append(html.escape(code[pos:m.start()], quote=False))
        kind = m.lastgroup
        out.append('<span class="tok-%s">%s</span>' % (kind, html.escape(m.group(), quote=False)))
        pos = m.end()
    out.append(html.escape(code[pos:], quote=False))
    return "".join(out)


_LOG = re.compile(
    r"(?P<ts>^\w{3} \w{3} [ \d]\d \d\d:\d\d:\d\d \w+ \d{4}:)"
    r"|(?P<mod>\[[\w-]+\])"
    r"|(?P<ok>(?<=\] )OK\b)"
    r"|(?P<err>(?<=\] )ERROR\b)"
    r"|(?P<banner>^=+ .* =+$)"
    r"|(?P<key>\b[A-Za-z_][\w.\u00b5]*(?==))",
    re.M,
)
_BASH = re.compile(
    r"(?P<com>(?<![\w$])#.*$)"
    r"|(?P<str>'[^']*'|\"(?:[^\"\\]|\\.)*\")"
    r"|(?P<cmd>^(?:\$ )?[A-Za-z][\w./-]*)"
    r"|(?P<flag>(?<![\w-])--?[A-Za-z][\w-]*)",
    re.M,
)
_JSON = re.compile(
    r"(?P<key>\"(?:[^\"\\]|\\.)*\"(?=\s*:))"
    r"|(?P<str>\"(?:[^\"\\]|\\.)*\")"
    r"|(?P<lit>\b(?:true|false|null)\b)"
    r"|(?P<num>-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b)"
)
_YAML = re.compile(
    r"(?P<com>#.*$)"
    r"|(?P<key>^\s*-?\s*[\w.-]+(?=:))"
    r"|(?P<str>\"[^\"]*\"|'[^']*')"
    r"|(?P<lit>\b(?:true|false|null)\b)",
    re.M,
)
_GO = re.compile(
    r"(?P<com>//.*$)"
    r"|(?P<str>\"(?:[^\"\\]|\\.)*\"|`[^`]*`)"
    r"|(?P<kw>\b(?:package|import|func|type|var|const|return|if|else|for|range|struct|interface|go|defer|select|case|switch|chan|map|nil|true|false)\b)",
    re.M,
)
_DOCKER = re.compile(
    r"(?P<com>^\s*#.*$)"
    r"|(?P<kw>^(?:FROM|RUN|COPY|ARG|ENTRYPOINT|CMD|WORKDIR|ENV|USER)\b|\bAS\b)"
    r"|(?P<str>\"[^\"]*\")",
    re.M,
)
_MAKE = re.compile(
    r"(?P<com>#.*$)"
    r"|(?P<kw>^[\w.-]+(?=:))"
    r"|(?P<flag>\$\([^)]*\))",
    re.M,
)
_LANGS = {
    "text": ("Output", _LOG), "bash": ("Shell", _BASH), "sh": ("Shell", _BASH),
    "json": ("JSON", _JSON), "yaml": ("YAML", _YAML), "go": ("Go", _GO),
    "dockerfile": ("Dockerfile", _DOCKER), "makefile": ("Makefile", _MAKE),
    "plain": ("", None), "": ("", None),
}


def render_code(code, lang):
    label, pattern = _LANGS.get(lang, (lang.upper(), None))
    body = _tokenise(code, pattern) if pattern else html.escape(code, quote=False)
    bar = '<div class="codebar"><span class="codelabel">%s</span></div>' % label if label else ""
    return '<div class="codeblock lang-%s">%s<pre><code>%s</code></pre></div>' % (lang or "plain", bar, body)


# --------------------------------------------------------------------------
# Block Markdown
# --------------------------------------------------------------------------

LIST_RE = re.compile(r"^(\s*)([-*]|\d+[.)])\s+(.*)$")
TABLE_SEP = re.compile(r"^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$")
CALLOUT_TITLES = {"note": "Note", "tip": "Tip", "warning": "Warning"}


def split_row(line):
    """Split a table row on pipes that are not inside `code` or escaped."""
    line = line.strip()
    if line.startswith("|"):
        line = line[1:]
    if line.endswith("|") and not line.endswith("\\|"):
        line = line[:-1]
    cells, cur, in_code, i = [], "", False, 0
    while i < len(line):
        ch = line[i]
        if ch == "\\" and i + 1 < len(line) and line[i + 1] == "|":
            cur += "|"
            i += 2
            continue
        if ch == "`":
            in_code = not in_code
        if ch == "|" and not in_code:
            cells.append(cur.strip())
            cur = ""
        else:
            cur += ch
        i += 1
    cells.append(cur.strip())
    return cells


def render_list(lines, i):
    m = LIST_RE.match(lines[i])
    indent, ordered = len(m.group(1)), m.group(2)[0].isdigit()
    tag = "ol" if ordered else "ul"
    out = ["<%s>" % tag]
    while i < len(lines):
        m = LIST_RE.match(lines[i])
        if not m or len(m.group(1)) != indent or m.group(2)[0].isdigit() != ordered:
            break
        text = m.group(3)
        i += 1
        while (i < len(lines) and lines[i].strip() and not LIST_RE.match(lines[i])
               and len(lines[i]) - len(lines[i].lstrip()) > indent):
            text += " " + lines[i].strip()
            i += 1
        item = inline(text)
        if i < len(lines):
            n = LIST_RE.match(lines[i])
            if n and len(n.group(1)) > indent:
                sub, i = render_list(lines, i)
                item += sub
        out.append("<li>%s</li>" % item)
        j = i
        while j < len(lines) and not lines[j].strip():
            j += 1
        if j > i and j < len(lines):
            n = LIST_RE.match(lines[j])
            if n and len(n.group(1)) == indent and n.group(2)[0].isdigit() == ordered:
                i = j
    out.append("</%s>" % tag)
    return "".join(out), i


def render_blocks(lines, headings, used_ids):
    out, i = [], 0
    while i < len(lines):
        line = lines[i]
        if not line.strip():
            i += 1
            continue

        fence = re.match(r"^```(\w*)\s*$", line)
        if fence:
            i += 1
            code = []
            while i < len(lines) and not lines[i].startswith("```"):
                code.append(lines[i])
                i += 1
            i += 1
            out.append(render_code("\n".join(code), fence.group(1)))
            continue

        cont = re.match(r"^:::(\w+)\s*(.*)$", line)
        if cont:
            kind, title = cont.group(1), cont.group(2).strip()
            i += 1
            inner = []
            while i < len(lines) and lines[i].strip() != ":::":
                inner.append(lines[i])
                i += 1
            i += 1
            if kind == "html":
                out.append("\n".join(inner))
            elif kind == "screen":
                out.append(screen_html(title, " ".join(x.strip() for x in inner if x.strip())))
            elif kind == "example":
                out.append(example_html(title, " ".join(x.strip() for x in inner if x.strip())))
            elif kind in CALLOUT_TITLES:
                body = render_blocks(inner, [], used_ids)
                out.append('<div class="callout callout-%s"><p class="callout-title">%s</p>%s</div>'
                           % (kind, html.escape(title or CALLOUT_TITLES[kind], quote=False), body))
            else:
                raise ValueError("unknown container :::%s" % kind)
            continue

        head = re.match(r"^(#{2,4})\s+(.*?)\s*$", line)
        if head:
            level, text = len(head.group(1)), head.group(2)
            slug = base = slugify(text)
            n = 2
            while slug in used_ids:
                slug = "%s-%d" % (base, n)
                n += 1
            used_ids.add(slug)
            headings.append((level, text, slug))
            out.append('<h%d id="%s">%s<a class="anchor" href="#%s" aria-label="Link to this section">#</a></h%d>'
                       % (level, slug, inline(text), slug, level))
            i += 1
            continue

        if re.match(r"^-{3,}\s*$", line):
            out.append("<hr>")
            i += 1
            continue

        if line.lstrip().startswith("|") and i + 1 < len(lines) and TABLE_SEP.match(lines[i + 1]):
            header = split_row(line)
            i += 2
            rows = []
            while i < len(lines) and lines[i].strip().startswith("|"):
                rows.append(split_row(lines[i]))
                i += 1
            t = ['<div class="tablewrap"><table><thead><tr>']
            t += ["<th>%s</th>" % inline(c) for c in header]
            t.append("</tr></thead><tbody>")
            for r in rows:
                r = (r + [""] * len(header))[: len(header)]
                t.append("<tr>" + "".join("<td>%s</td>" % inline(c) for c in r) + "</tr>")
            t.append("</tbody></table></div>")
            out.append("".join(t))
            continue

        if line.startswith(">"):
            quote = []
            while i < len(lines) and lines[i].startswith(">"):
                quote.append(re.sub(r"^>\s?", "", lines[i]))
                i += 1
            out.append("<blockquote>%s</blockquote>" % render_blocks(quote, [], used_ids))
            continue

        if LIST_RE.match(line):
            block, i = render_list(lines, i)
            out.append(block)
            continue

        para = [line.strip()]
        i += 1
        while (i < len(lines) and lines[i].strip() and not lines[i].startswith(("```", ":::", ">", "#", "|"))
               and not LIST_RE.match(lines[i])):
            para.append(lines[i].strip())
            i += 1
        out.append("<p>%s</p>" % inline(" ".join(para)))
    return "\n".join(out)


def parse_page(text):
    meta = {}
    m = re.match(r"^---\n(.*?)\n---\n", text, re.S)
    if m:
        for row in m.group(1).splitlines():
            if ":" in row:
                k, v = row.split(":", 1)
                meta[k.strip()] = v.strip()
        text = text[m.end():]
    headings = []
    body = render_blocks(text.splitlines(), headings, set())
    return meta, body, headings


# --------------------------------------------------------------------------
# Screenshots: docs/screens/<name>.txt drawn as terminal windows
# --------------------------------------------------------------------------

SCREENS_SRC = DOCS / "screens"
SCREENS_OUT = DOCS / "assets" / "screens"
SCREENS = {}  # name -> SVG, for every :::screen the pages use; main() writes and checks them

WRAP = 110            # a longer line wraps, as a terminal would, at a space where it can
TABLE_WRAP = 120      # a longer table row wraps its last cell, under itself, if that leaves it room
FONT, CW, LH = 13, 13 * 0.62, 18   # CW errs wide: the window fits any monospace font
PAD, BAR = 16, 30
# The window is dark in either site theme, as terminals are.
SCREEN_STYLE = (
    "text{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,'DejaVu Sans Mono','Liberation Mono',monospace;"
    "font-size:13px;white-space:pre;fill:#c9d1d9}"
    ".bar{fill:#8b949e;font-size:12px}.p{fill:#7ee787}.c{fill:#f0f6fc;font-weight:600}.d{fill:#8b949e}"
    ".m{fill:#79c0ff}.ok{fill:#3fb950}.wa{fill:#d29922}.er{fill:#f85149}.db{fill:#bc8cff}.k{fill:#8b949e}"
    ".h{fill:#79c0ff;font-weight:600}")

_SEPARATOR = re.compile(r"^-+(\+-+)*$")
_LOGLINE = re.compile(r"^(\d{4}-\d\d-\d\dT[\d:.]+Z )(\[[\w-]+\] )(OK|WARN|ERROR|DEBUG)\b(.*)$")
_KEY = re.compile(r"(?<![\w.-])([a-z_][\w.-]*=)")
_STATE = re.compile(r"(?<![\w-])(ok|up|warn|fail|down)(?![\w-])")
_STATE_CELL = re.compile(r"^\s*(ok|up|warn|fail|down|-)( / (ok|fail|-))?\s*$")  # a cell that is a state, or two
_LEVEL = {"OK": "ok", "WARN": "wa", "ERROR": "er", "DEBUG": "db"}
_STATE_CLS = {"ok": "ok", "up": "ok", "warn": "wa", "fail": "er", "down": "er"}


def _split(text, pattern, cls_of, rest=""):
    out, pos = [], 0
    for m in pattern.finditer(text):
        if m.start() > pos:
            out.append((rest, text[pos:m.start()]))
        out.append((cls_of(m.group(1)), m.group(0)))
        pos = m.end()
    if pos < len(text):
        out.append((rest, text[pos:]))
    return out


def screen_tokens(line, header=False):
    """One line of a screenshot as (class, text) runs: a command, a log line, a table's
    header, separator or row - its states coloured - or plain output."""
    if line.startswith("$ "):
        return [("p", "$ "), ("c", line[2:])]
    m = _LOGLINE.match(line)
    if m:
        return ([("d", m.group(1)), ("m", m.group(2)), (_LEVEL[m.group(3)], m.group(3))]
                + _split(m.group(4), _KEY, lambda _: "k"))
    if header:
        return [("h", line)]
    if _SEPARATOR.match(line):
        return [("d", line)]
    if " | " in line:
        out = []
        for i, cell in enumerate(line.split("|")):
            if i:
                out.append(("d", "|"))
            out += _split(cell, _STATE, lambda w: _STATE_CLS[w]) if _STATE_CELL.match(cell) else [("", cell)]
        return out
    m = re.match(r"^(\s+)(warn|fail)( - .*)$", line)  # doctor's "What to do"
    if m:
        return [("", m.group(1)), (_STATE_CLS[m.group(2)], m.group(2)), ("", m.group(3))]
    return [("", line)]


def _wraps(line, start=0, width=WRAP, next_width=WRAP - 4):
    """Where a long line breaks: (start, end) slices, at a space if one is near the end."""
    spans = []
    while len(line) - start > width:
        cut = line.rfind(" ", start + width // 2, start + width)
        cut = cut if cut > start else start + width
        spans.append((start, cut))
        start = cut + 1 if line[cut:cut + 1] == " " else cut
        width = next_width
    spans.append((start, len(line)))
    return spans


def _layout(line):
    """How a line is laid out: [(prefix, start, end)] - each visual line is the prefix, then
    line[start:end]. A table keeps its columns: a long row wraps its last cell under itself,
    the cells before it blank but for their bars; one whose last cell would have too little
    room stays whole, as does a separator. Any other long line wraps, its continuations
    indented."""
    if _SEPARATOR.match(line):
        return [("", 0, len(line))]
    if " | " in line:
        if len(line) <= TABLE_WRAP:
            return [("", 0, len(line))]
        cell = line.rfind(" | ") + 3
        if TABLE_WRAP - cell < 24:
            return [("", 0, len(line))]
        blank = re.sub(r"[^|]", " ", line[:cell])
        spans = _wraps(line, cell, TABLE_WRAP - cell, TABLE_WRAP - cell)
        return [("", 0, spans[0][1])] + [(blank, a, b) for a, b in spans[1:]]
    spans = _wraps(line)
    return [("", spans[0][0], spans[0][1])] + [("    ", a, b) for a, b in spans[1:]]


def screen_svg(name, text):
    """docs/screens/<name>.txt as an SVG terminal window: the title, then every line."""
    rows = text.expandtabs(8).rstrip("\n").split("\n")
    title, body = rows[0], rows[1:]
    while body and not body[-1].strip():
        body.pop()
    visual = []  # each a list of (class, text)
    for i, line in enumerate(body):
        line = line.rstrip()
        header = i + 1 < len(body) and _SEPARATOR.match(body[i + 1].rstrip())
        runs = screen_tokens(line, bool(header))
        for prefix, a, b in _layout(line):
            part, pos = [], 0
            for cls, t in runs:
                lo, hi = max(a, pos), min(b, pos + len(t))
                if lo < hi:
                    part.append((cls, t[lo - pos:hi - pos]))
                pos += len(t)
            visual.append(([("d" if "|" in prefix else "", prefix)] if prefix else []) + part)
    # The window is as wide as its widest line; a table's separator, which never wraps,
    # is cut to it.
    seps = [v for v in visual if len(v) == 1 and _SEPARATOR.match(v[0][1])]
    cols = max([60] + [sum(len(t) for _, t in v) for v in visual if not any(v is x for x in seps)])
    for v in seps:
        v[0] = (v[0][0], v[0][1][:cols])
    width = int(PAD * 2 + cols * CW)
    height = BAR + 12 + len(visual) * LH + 10
    esc = lambda t: html.escape(t, quote=False)
    out = ['<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" role="img" '
           'aria-label="%s" xml:space="preserve">' % (width, height, width, height, html.escape(title, quote=True)),
           "<style>%s</style>" % SCREEN_STYLE,
           '<rect width="%d" height="%d" rx="8" fill="#0d1117"/>' % (width, height),
           '<path d="M0 8a8 8 0 0 1 8-8h%dA8 8 0 0 1 %d 8v%dH0z" fill="#161b22"/>' % (width - 16, width, BAR - 8),
           '<circle cx="18" cy="15" r="5.5" fill="#ff5f57"/><circle cx="36" cy="15" r="5.5" fill="#febc2e"/>'
           '<circle cx="54" cy="15" r="5.5" fill="#28c840"/>',
           '<text class="bar" x="%d" y="19.5" text-anchor="middle">%s</text>' % (width // 2, esc(title))]
    y = BAR + 12 + 13
    for v in visual:
        spans = "".join(('<tspan class="%s">%s</tspan>' % (c, esc(t))) if c else esc(t) for c, t in v)
        if spans:
            out.append('<text x="%d" y="%d">%s</text>' % (PAD, y, spans))
        y += LH
    out.append("</svg>")
    return "\n".join(out) + "\n", width, height


def screen_html(name, caption):
    """A :::screen block: the screenshot, its caption, and its text to copy."""
    src = SCREENS_SRC / ("%s.txt" % name)
    if not src.exists():
        raise ValueError("no screenshot %s - capture them: make screens (docs/screens/capture.sh)" % src.relative_to(ROOT))
    text = src.read_text()
    svg, width, height = screen_svg(name, text)
    SCREENS[name] = svg
    body = text.split("\n", 1)[1].rstrip("\n") if "\n" in text else ""
    cap = inline(caption) if caption else ""
    return ('<figure class="screen"><img src="assets/screens/%s.svg" width="%d" height="%d" alt="%s" loading="lazy">'
            '%s<details class="screentext"><summary>As text</summary><pre><code>%s</code></pre></details></figure>'
            % (name, width, height, html.escape(caption or text.split("\n", 1)[0], quote=True),
               ("<figcaption>%s</figcaption>" % cap) if cap else "", html.escape(body, quote=False)))


# --------------------------------------------------------------------------
# Examples: files the binary wrote - docs/examples/<file> - shown and offered
# --------------------------------------------------------------------------

EXAMPLES_DIR = DOCS / "examples"
EXAMPLES = set()  # every file a :::example uses; main() checks none is left unused
EXAMPLE_LANG = {".yaml": "yaml", ".yml": "yaml", ".sh": "bash", ".json": "json"}


def example_html(name, caption):
    """A :::example block: the file as a code block, its caption, and a link to it -
    the file itself is published beside the pages, for curl or a browser."""
    src = EXAMPLES_DIR / name
    if not src.exists():
        raise ValueError("no example %s - write them: THELB_WRITE_EXAMPLES=1 go test ./internal/cli -run TestTheDocsShowWhatGenerateK8sPrints"
                         % src.relative_to(ROOT))
    EXAMPLES.add(name)
    cap = ('<p class="example-caption">%s</p>' % inline(caption)) if caption else ""
    link = '<p class="example-file"><a href="examples/%s" download>%s</a></p>' % (html.escape(name, quote=True), html.escape(name))
    return '<div class="example">%s%s%s</div>' % (cap, render_code(src.read_text().rstrip("\n"), EXAMPLE_LANG.get(src.suffix, "text")), link)


# --------------------------------------------------------------------------
# Page assembly
# --------------------------------------------------------------------------

def read_version():
    """The newest release the changelog names - "## v0.1.0 - ..." - so the pages show
    the release they describe, not the development build's 0.1.0-dev."""
    m = re.search(r"^## v(\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.]*)?) ", (ROOT / "CHANGELOG.md").read_text(), re.M)
    return m.group(1) if m else "dev"


def read_go_version():
    """The Go release go.mod's `go` line names - the one every workflow builds with
    (go-version-file: go.mod) and the Dockerfile's builder image must match."""
    m = re.search(r"^go\s+(\S+)\s*$", (ROOT / "go.mod").read_text(), re.M)
    return m.group(1) if m else "unknown"


def changelog_page():
    """CHANGELOG.md (repo root, the single source of truth) rendered as a docs page."""
    text = (ROOT / "CHANGELOG.md").read_text()
    text = re.sub(r"^# .*\n", "", text, count=1)
    intro = ("---\ntitle: Changelog\nlead: What changed in every release, newest first.\n"
             "description: Release history for theLB.\nsection: Reference\norder: 2\nnav: Changelog\n---\n")
    return intro + text


_TOKEN_RE = re.compile(r"(\\?)\{\{(version|minor|major|go)\}\}")


def fill_tokens(text, version, go=None):
    """The current release in a docs page, from the changelog's newest version, so an example never goes
    stale and a release edits no page: {{version}} -> 4.2.2, {{minor}} -> 4.2, {{major}} -> 4.
    {{go}} is the Go release theLB is built with, from go.mod (1.27.4), so moving Go edits no
    page either. A backslash keeps one literal: \\{{version}} -> {{version}}. Other {{...}} are
    left alone."""
    parts = version.split(".")
    values = {"version": version, "minor": ".".join(parts[:2]), "major": parts[0],
              "go": go if go is not None else read_go_version()}
    return _TOKEN_RE.sub(lambda m: m.group(0)[1:] if m.group(1) else values[m.group(2)], text)


def collect_pages(version=None):
    version = version or read_version()
    pages = []
    for path in sorted(SRC.glob("*.md")):
        pages.append((path.stem, fill_tokens(path.read_text(), version), "docs/src/%s.md" % path.stem))
    if (ROOT / "CHANGELOG.md").exists():
        pages.append(("changelog", changelog_page(), "CHANGELOG.md"))
    out = []
    for slug, text, edit in pages:
        meta, body, headings = parse_page(text)
        meta.setdefault("title", slug)
        out.append({"slug": slug, "meta": meta, "body": body, "headings": headings, "edit": edit})
    return out


LOGO = ('<svg viewBox="0 0 32 32" width="26" height="26" aria-hidden="true"><rect width="32" height="32" rx="7" '
        'fill="var(--accent)"/><circle cx="9" cy="16" r="3" fill="#fff"/><path d="M11.5 16L22 9M11.5 16H22M11.5 16L22 23" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round"/><circle cx="23.5" cy="9" r="2.4" fill="#fff"/><circle cx="23.5" cy="16" r="2.4" fill="#fff"/><circle cx="23.5" cy="23" r="2.4" fill="#fff"/></svg>')


def nav_html(pages, current):
    groups = {s: [] for s in SECTIONS}
    for p in pages:
        groups.setdefault(p["meta"].get("section", "Guides"), []).append(p)
    out = []
    for section in SECTIONS:
        items = sorted(groups.get(section, []), key=lambda p: int(p["meta"].get("order", 99)))
        if not items:
            continue
        out.append('<div class="navgroup"><p class="navtitle">%s</p><ul>' % section)
        for p in items:
            cls = ' class="active" aria-current="page"' if p["slug"] == current else ""
            out.append('<li><a href="%s.html"%s>%s</a></li>' % (p["slug"], cls, html.escape(p["meta"].get("nav", p["meta"]["title"]))))
        out.append("</ul></div>")
    return "".join(out)


def ordered(pages):
    def key(p):
        s = p["meta"].get("section", "Guides")
        return (SECTIONS.index(s) if s in SECTIONS else 99, int(p["meta"].get("order", 99)))
    return sorted(pages, key=key)


def render_page(p, pages, version):
    meta, slug = p["meta"], p["slug"]
    seq = ordered(pages)
    idx = [q["slug"] for q in seq].index(slug)
    prev_p = seq[idx - 1] if idx > 0 else None
    next_p = seq[idx + 1] if idx + 1 < len(seq) else None

    toc = [(l, t, s) for (l, t, s) in p["headings"] if l in (2, 3)]
    toc_html = ""
    if len(toc) >= 3:
        toc_html = '<aside class="toc" aria-label="On this page"><p class="toctitle">On this page</p><ul>%s</ul></aside>' % "".join(
            '<li class="toc%d"><a href="#%s">%s</a></li>' % (l, s, inline(t)) for (l, t, s) in toc)

    pager = ""
    if prev_p or next_p:
        pager = '<nav class="pager" aria-label="Pagination">'
        pager += ('<a class="prev" href="%s.html"><span>Previous</span>%s</a>' % (prev_p["slug"], html.escape(prev_p["meta"]["title"]))) if prev_p else "<span></span>"
        pager += ('<a class="next" href="%s.html"><span>Next</span>%s</a>' % (next_p["slug"], html.escape(next_p["meta"]["title"]))) if next_p else "<span></span>"
        pager += "</nav>"

    lead = '<p class="lead">%s</p>' % inline(meta["lead"]) if meta.get("lead") else ""
    title = meta["title"]
    page_title = "theLB" if slug == "index" else "%s · theLB" % title
    desc = html.escape(meta.get("description", meta.get("lead", "theLB documentation")), quote=True)
    section = meta.get("section", "")

    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{html.escape(page_title)}</title>
<meta name="description" content="{desc}">
<meta name="color-scheme" content="light dark">
<link rel="icon" href="assets/favicon.svg" type="image/svg+xml">
<link rel="stylesheet" href="assets/style.css">
<script>try{{var t=localStorage.getItem("thelb-theme");if(t)document.documentElement.setAttribute("data-theme",t)}}catch(e){{}}</script>
</head>
<body>
<input type="checkbox" id="nav-toggle" class="nav-toggle" aria-label="Toggle navigation">
<header class="topbar">
  <label for="nav-toggle" class="burger" aria-hidden="true"><span></span><span></span><span></span></label>
  <a class="brand" href="index.html">{LOGO}<span class="brandname">theLB</span></a>
  <span class="version">v{html.escape(version)}</span>
  <nav class="toplinks" aria-label="Site">
    <a href="install.html">Install</a>
    <a href="quickstart.html">Docs</a>
    <a href="tech-design.html">Technical</a>
    <a href="{REPO_URL}/releases/latest">Releases</a>
    <a href="{REPO_URL}">GitHub</a>
    <button type="button" class="themebtn" id="themebtn" aria-label="Toggle color theme" title="Toggle color theme">&#9680;</button>
  </nav>
</header>
<div class="layout">
<nav class="sidebar" aria-label="Documentation">{nav_html(pages, slug)}</nav>
<main class="content" id="content">
<article>
{'<p class="eyebrow">%s</p>' % html.escape(section) if section and slug != 'index' else ''}
<h1>{inline(title)}</h1>
{lead}
{p["body"]}
</article>
{pager}
<footer class="pagefoot"><a href="{REPO_URL}/edit/main/{p["edit"]}">Edit this page on GitHub</a><span>theLB v{html.escape(version)} &middot; MIT License</span></footer>
</main>
{toc_html}
</div>
<script src="assets/site.js" defer></script>
</body>
</html>
"""


# --------------------------------------------------------------------------
# Link checking
# --------------------------------------------------------------------------

def check_links(rendered):
    """Every internal href must hit a page (or asset) that exists, and every #anchor an id on it."""
    ids = {name: set(re.findall(r'\bid="([^"]+)"', page)) for name, page in rendered.items()}
    problems = []
    for name, page in rendered.items():
        for href in re.findall(r'href="([^"]+)"', page):
            if re.match(r"^(https?:|mailto:)", href):
                continue
            target, _, frag = href.partition("#")
            if not target:
                if frag and frag not in ids[name]:
                    problems.append("%s: #%s has no matching id" % (name, frag))
                continue
            if target.startswith(("assets/", "examples/")):
                if not (DOCS / target).exists():
                    problems.append("%s: missing file %s" % (name, target))
                continue
            if target not in rendered:
                problems.append("%s: link to missing page %s" % (name, target))
            elif frag and frag not in ids[target]:
                problems.append("%s: %s#%s has no matching id" % (name, target, frag))
    return problems


def site_url_sources():
    """Texts that link to the published site by absolute URL: name -> text."""
    sources = {}
    for name in ("README.md", "CHANGELOG.md", "SECURITY.md", "docs/design.md", "docs/diagrams.md", "docs/build-state.md"):
        f = ROOT / name
        if f.exists():
            sources[name] = f.read_text()
    for f in sorted(SRC.glob("*.md")):
        sources["docs/src/%s" % f.name] = f.read_text()
    return sources


_SITE_URL_RE = re.compile(re.escape(SITE_URL) + r"[^\s)\]\"'<>`]*")


def check_site_urls(sources, rendered):
    """Every absolute URL of the published site in `sources` must name a file that exists.

    The site is the repository root served as-is, so `SITE_URL/docs/install.html` must be
    `docs/install.html` in the repo, and its `#anchor` must be an id on that page. This runs
    offline and deterministically - it exists because the README once linked to
    `SITE_URL/install.html` (missing `/docs/`) and nothing noticed: the page checks in
    check_links only look at links between generated pages.
    """
    ids = {name: set(re.findall(r'\bid="([^"]+)"', page)) for name, page in rendered.items()}
    problems = []
    for name, text in sources.items():
        for url in sorted(set(u.rstrip(".,;:") for u in _SITE_URL_RE.findall(text))):
            path, _, frag = url[len(SITE_URL):].partition("#")
            path = path.partition("?")[0]
            rel = path.lstrip("/")
            if rel == "" or rel.endswith("/"):
                rel += "index.html"
            target = ROOT / rel
            if target.is_dir():  # /thelb/docs -> Pages redirects to /docs/ and serves its index
                rel = rel.rstrip("/") + "/index.html"
                target = ROOT / rel
            page = rel[len("docs/"):] if rel.startswith("docs/") else None
            exists = target.exists() or (page in rendered)
            if not exists:
                hint = ""
                if (DOCS / rel).exists() or ("%s" % rel) in rendered:
                    hint = " (did you mean %s/docs/%s?)" % (SITE_URL, rel)
                problems.append("%s: %s -> no such file %s in the published site%s" % (name, url, rel, hint))
            elif frag and page in ids and frag not in ids[page]:
                problems.append("%s: %s -> %s has no #%s" % (name, url, rel, frag))
    return problems


def main():
    check = "--check" in sys.argv[1:]
    version = read_version()
    pages = collect_pages(version)
    rendered = {"%s.html" % p["slug"]: render_page(p, pages, version) for p in pages}

    problems = check_links(rendered) + check_site_urls(site_url_sources(), rendered)
    if EXAMPLES_DIR.exists():
        problems += ["docs/examples/%s: no page shows it" % f.name
                     for f in sorted(EXAMPLES_DIR.iterdir()) if f.is_file() and f.name not in EXAMPLES]
    svgs = {"assets/screens/%s.svg" % n: svg for n, svg in SCREENS.items()}
    unused = sorted("assets/screens/%s" % f.name for f in SCREENS_OUT.glob("*.svg")
                    if "assets/screens/%s" % f.name not in svgs) if SCREENS_OUT.exists() else []
    if check:
        for name, content in list(rendered.items()) + list(svgs.items()):
            f = DOCS / name
            if not f.exists() or f.read_text() != content:
                problems.append("%s is stale - run: python3 docs/build.py" % name)
        problems += ["%s: no page shows it - delete it" % u for u in unused]
    else:
        SCREENS_OUT.mkdir(parents=True, exist_ok=True)
        for name, content in list(rendered.items()) + list(svgs.items()):
            (DOCS / name).write_text(content)
        for u in unused:
            (DOCS / u).unlink()
        print("built %d pages and %d screenshots (v%s) into %s" % (len(rendered), len(svgs), version, DOCS))

    for p in problems:
        print("PROBLEM:", p)
    sys.exit(1 if problems else 0)


if __name__ == "__main__":
    main()
