---
title: This documentation
lead: How this site is built, how its screenshots are made, and how to change a page.
description: How the theLB documentation site is structured, built with docs/build.py, illustrated with real screenshots, and published with GitHub Pages from main.
section: Technical
order: 5
nav: This documentation
---

## Structure

The site is plain static HTML served by GitHub Pages. There is no framework and no external service: no JavaScript build, no CDN, no web fonts.

```plain
docs/
├── build.py              the generator (Python 3, standard library only)
├── test_build.py         its tests
├── src/*.md              one Markdown source per page
├── screens/capture.sh    captures the screenshots from the real binary
├── screens/*.txt         the captured output, one file per screenshot
├── assets/               style.css, site.js, favicon.svg, and screens/*.svg
├── *.html                generated pages - committed, never edited by hand
├── design.md             the full design (read on GitHub)
├── diagrams.md           the design in pictures (read on GitHub)
└── build-state.md        where the code is, and what comes next
CHANGELOG.md              at the repository root; also the Changelog page
index.html, .nojekyll     at the repository root; see Hosting
```

The generator turns each `docs/src/<name>.md` into `docs/<name>.html`, wraps it in the shared layout (top bar, sidebar, "On this page", previous and next links), and renders `CHANGELOG.md` as the [Changelog](changelog.md) page.

## Changing a page

Documentation changes go straight to `main`. They are not releases: no branch, no tag, and no workflow runs.

```bash
$EDITOR docs/src/directors.md
python3 docs/build.py           # regenerate the HTML
python3 docs/build.py --check   # pages current? every link and anchor good?
git add docs && git commit
git push origin main
```

`--check` fails if a generated page or screenshot is stale, an internal link or `#anchor` is broken, a screenshot is no longer used, or a link to the published site (`https://dmartsapp.github.io/thelb/...`) in `README.md`, `CHANGELOG.md` or a page names a page that does not exist. The release workflow runs it too.

## Screenshots

Every screenshot is **real output** of the theLB binary, captured in the [lab](lab.md) - never typed by hand.

```bash
make screens                       # on a Linux machine, as root, with the lab down
make remote-screens REMOTE=<host>  # on another machine, over ssh; the files come back
```

`docs/screens/capture.sh` starts a lab with a binary of its own, runs every command the pages show, and saves each output as `docs/screens/<name>.txt`: the window title on the first line, then the commands (`$ ...`) and what they printed. Then `python3 docs/build.py` draws each one as a terminal window, `docs/assets/screens/<name>.svg`, with a "As text" box under it for copying.

To show a screenshot on a page:

```plain
:::screen status
The caption, under the picture.
:::
```

When theLB's output changes, capture them all again: they then all come from the same build.

## Page format

Each source starts with a small header, then Markdown:

```plain
---
title: Directors and failover
lead: One sentence under the title.
description: Text for search engines and link previews.
section: Guides
order: 1
nav: Directors and failover
---

## First section
```

| Header key | Meaning |
|---|---|
| `title` | the page heading and browser title |
| `lead` | the larger sentence under the title |
| `description` | the `<meta name="description">` |
| `section` | the sidebar group: `Get started`, `Guides`, `Reference` or `Technical` |
| `order` | its position in the group |
| `nav` | the sidebar label, if not the title |

The Markdown is a small dialect: headings (`##`, `###`), lists, tables, inline code, bold, italic and links (`.md` links become `.html`); fenced code blocks (`bash`, `text`, `json`, `yaml`, `go`); callouts (`:::note`, `:::tip`, `:::warning`); raw HTML (`:::html`, for diagrams and cards); and screenshots (`:::screen`).

`\{{version}}`, `\{{minor}}` and `\{{go}}` in a page become the newest version in the changelog, its minor line, and the Go release from `go.mod`.

## Writing guidelines

- **Simple English.** Short sentences, common words. Explain a term the first time it appears.
- **Real output only.** Examples and screenshots come from the real binary.
- **What and why before how.** The user pages explain; the Technical section goes deep.
- **Exact facts.** Defaults, limits and timings as the code has them.

## Hosting

The repository's Pages setting builds from the `main` branch, folder `/`. Two small files at the repository root make that work:

- **`.nojekyll`** tells GitHub Pages to serve the files as they are, without Jekyll.
- **`index.html`** sends visitors from `https://dmartsapp.github.io/thelb/` to `docs/`.

A push to `main` runs GitHub's own `pages-build-deployment`, and the change is live within a minute or two. Nothing in this repository's workflows builds the site, which is why the generated HTML is committed.
