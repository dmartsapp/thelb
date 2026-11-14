#!/usr/bin/env python3
"""Tests for docs/build.py: the Markdown renderer's rules and the published-site URL check.

    python3 docs/test_build.py

Standard library only, like build.py itself.
"""
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build  # noqa: E402


def rendered_site():
    pages = build.collect_pages()
    version = build.read_version()
    return {"%s.html" % p["slug"]: build.render_page(p, pages, version) for p in pages}


class Tokens(unittest.TestCase):
    """fill_tokens: the current release in a page, from the changelog's newest version."""

    def test_the_three_tokens(self):
        self.assertEqual(build.fill_tokens("{{version}} {{minor}} {{major}}", "4.3.0"), "4.3.0 4.3 4")

    def test_the_go_token_is_go_mods_go_line(self):
        self.assertEqual(build.fill_tokens("golang:{{go}}-alpine", "4.3.0", go="1.27.3"), "golang:1.27.3-alpine")
        self.assertRegex(build.read_go_version(), r"^1\.\d+(\.\d+)?$")
        self.assertEqual(build.fill_tokens("{{go}}", "4.3.0"), build.read_go_version())

    def test_a_backslash_keeps_one_literal(self):
        self.assertEqual(build.fill_tokens(r"\{{version}} is {{version}}", "4.3.0"), "{{version}} is 4.3.0")

    def test_other_braces_are_left_alone(self):
        for text in ("${{ github.ref_name }}", "{{ version }}", "{{foo}}", "{version}"):
            with self.subTest(text=text):
                self.assertEqual(build.fill_tokens(text, "4.3.0"), text)

    def test_every_page_is_filled_in_except_its_escaped_tokens(self):
        for src in sorted(build.SRC.glob("*.md")):
            raw = src.read_text()
            out = build.fill_tokens(raw, "9.8.7", go="9.9.9")
            for name in ("version", "minor", "major", "go"):
                with self.subTest(page=src.name, token=name):
                    self.assertEqual(out.count("{{%s}}" % name), raw.count("\\{{%s}}" % name))


class Screens(unittest.TestCase):
    """screen_svg / screen_html: a captured screenshot drawn as a terminal window."""

    TEXT = ("d1 - a log\n$ sudo thelb status\n"
            " what      | total | up | down\n"
            "-----------+-------+----+-----\n"
            " directors |     3 |  3 |    0\n"
            "2026-11-10T22:31:04.118Z [director] OK leader node=d1 term=2\n"
            "2026-11-10T22:31:05.118Z [director] WARN backend not serving node=<b2> & more\n")

    def svg(self, text=None):
        return build.screen_svg("t", text or self.TEXT)[0]

    def test_a_window_with_its_title(self):
        svg = self.svg()
        self.assertTrue(svg.startswith("<svg "))
        self.assertIn(">d1 - a log</text>", svg)

    def test_commands_logs_and_tables_are_coloured(self):
        svg = self.svg()
        self.assertIn('<tspan class="p">$ </tspan><tspan class="c">sudo thelb status</tspan>', svg)
        self.assertIn('<tspan class="h"> what      | total | up | down</tspan>', svg)
        self.assertIn('<tspan class="ok">OK</tspan>', svg)
        self.assertIn('<tspan class="wa">WARN</tspan>', svg)
        self.assertIn('<tspan class="k">node=</tspan>', svg)

    def test_only_a_cell_that_is_a_state_is_coloured(self):
        svg = self.svg("t\n a    | state | detail\n------+-------+--------\n link | ok    | its machine is down\n x    | ok / fail | -\n")
        self.assertIn('<tspan class="ok">ok</tspan>', svg)
        self.assertIn('<tspan class="er">fail</tspan>', svg)
        self.assertNotIn('<tspan class="er">down</tspan>', svg)

    def test_text_is_escaped(self):
        svg = self.svg()
        self.assertIn("&lt;b2&gt; &amp; more", svg)
        self.assertNotIn("<b2>", svg)

    def test_a_long_line_wraps_at_a_space(self):
        line = " ".join(["word"] * 60)  # 299 characters
        svg = self.svg("t\n" + line + "\n")
        rows = [r for r in svg.split("\n") if r.startswith("<text x=")]
        self.assertEqual(len(rows), 3)
        self.assertTrue(all(len(build.html.unescape(build.re.sub("<[^>]+>", "", r))) <= build.WRAP for r in rows))
        self.assertIn(">    word", rows[1])  # a continuation is indented

    def test_a_wide_table_wraps_its_last_cell_under_itself(self):
        row = " ok   | " + " ".join(["detail"] * 30)  # 216 characters
        svg = self.svg("t\n area | detail\n------+-------\n" + row + "\n")
        rows = [build.html.unescape(build.re.sub("<[^>]+>", "", r)) for r in svg.split("\n") if r.startswith("<text x=")]
        self.assertEqual(rows[2][:8], " ok   | ")
        self.assertTrue(all(r.startswith("      | ") for r in rows[3:]), rows[3:])
        self.assertTrue(all(len(r) <= build.TABLE_WRAP for r in rows))

    def test_a_table_whose_last_cell_has_no_room_stays_whole(self):
        row = " " + " | ".join(["x" * 20] * 6)  # 133 characters, the last cell from 111
        svg = self.svg("t\n" + row + "\n")
        self.assertEqual(len([r for r in svg.split("\n") if r.startswith("<text x=")]), 1)

    def test_a_separator_is_cut_to_the_window(self):
        row = " ok   | " + " ".join(["detail"] * 30)
        svg = self.svg("t\n area | detail\n" + "-" * 6 + "+" + "-" * 210 + "\n" + row + "\n")
        rows = [build.html.unescape(build.re.sub("<[^>]+>", "", r)) for r in svg.split("\n") if r.startswith("<text x=")]
        self.assertEqual(len(rows[1]), max(len(r) for r in rows[2:]))

    def test_the_block_needs_a_capture(self):
        with self.assertRaisesRegex(ValueError, "make screens"):
            build.screen_html("no-such-screen", "")


class SiteUrlCheck(unittest.TestCase):
    """check_site_urls: absolute URLs of the published site must name files that exist."""

    @classmethod
    def setUpClass(cls):
        cls.rendered = rendered_site()

    def check(self, text):
        return build.check_site_urls({"t.md": text}, self.rendered)

    def test_repository_is_currently_clean(self):
        self.assertEqual(build.check_site_urls(build.site_url_sources(), self.rendered), [])

    def test_valid_urls_pass(self):
        base = build.SITE_URL
        for url in (base, base + "/", base + "/docs/", base + "/docs/index.html",
                    base + "/docs/install.html", base + "/docs/install.html#download-a-release",
                    base + "/docs/assets/style.css", base + "/docs/src/index.md"):
            with self.subTest(url=url):
                self.assertEqual(self.check("see " + url), [])

    def test_the_readme_bug_is_caught(self):
        """The README once linked to /install.html instead of /docs/install.html and nothing noticed."""
        pages = ("install", "quickstart", "directors", "troubleshooting", "tech-design")
        text = "\n".join("[x](%s/%s.html)" % (build.SITE_URL, p) for p in pages)
        problems = self.check(text)
        self.assertEqual(len(problems), len(pages))
        joined = "\n".join(problems)
        for page in pages:
            self.assertIn("did you mean %s/docs/%s.html?" % (build.SITE_URL, page), joined)

    def test_missing_page_without_a_suggestion(self):
        problems = self.check(build.SITE_URL + "/docs/no-such-page.html")
        self.assertEqual(len(problems), 1)
        self.assertNotIn("did you mean", problems[0])

    def test_missing_anchor_is_caught(self):
        problems = self.check(build.SITE_URL + "/docs/install.html#no-such-section")
        self.assertEqual(len(problems), 1)
        self.assertIn("#no-such-section", problems[0])

    def test_trailing_punctuation_is_not_part_of_the_url(self):
        self.assertEqual(self.check("Docs: %s/docs/install.html." % build.SITE_URL), [])
        self.assertEqual(self.check("(%s/docs/install.html)" % build.SITE_URL), [])

    def test_other_hosts_are_ignored(self):
        self.assertEqual(self.check("https://example.com/thelb/install.html https://github.com/dmartsapp/thelb"), [])


class Markdown(unittest.TestCase):
    def render(self, md):
        return build.render_blocks(md.splitlines(), [], set())

    def test_md_links_become_html(self):
        self.assertIn('href="install.html#docker"', self.render("[x](install.md#docker)"))

    def test_pipes_inside_code_do_not_split_table_cells(self):
        html = self.render("| a | b |\n|---|---|\n| `x|y` | z |")
        self.assertIn("<code>x|y</code>", html)

    def test_log_lines_are_highlighted(self):
        html = build.render_code("Sun Sep 20 01:26:48 MDT 2026: [telnet] OK connect ok host=1.2.3.4", "text")
        self.assertIn('tok-ok">OK<', html)
        self.assertIn('tok-mod">[telnet]<', html)


if __name__ == "__main__":
    unittest.main(verbosity=2)
