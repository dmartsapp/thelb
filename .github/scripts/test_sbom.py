"""Tests for sbom.py:  python3 .github/scripts/test_sbom.py"""
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
here = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("sbom", os.path.join(here, "sbom.py"))
sbom = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sbom)

INFO = {
    "GoVersion": "go1.27.3",
    "Path": "github.com/dmartsapp/thelb",
    "Main": {"Path": "github.com/dmartsapp/thelb", "Version": "v4.3.0"},
    "Deps": [
        {"Path": "golang.org/x/net", "Version": "v0.62.0", "Sum": "h1:netnetnet="},
        {"Path": "github.com/spf13/cobra", "Version": "v1.10.2", "Sum": "h1:cobracobra="},
        {"Path": "github.com/Old/Thing", "Version": "v1.0.0", "Sum": "h1:old=",
         "Replace": {"Path": "github.com/New/Thing", "Version": "v1.0.1", "Sum": "h1:new="}},
    ],
    "Settings": [
        {"Key": "-buildmode", "Value": "exe"}, {"Key": "-compiler", "Value": "gc"},
        {"Key": "-ldflags", "Value": "-s -w -X main.Version=v4.3.0/abc/now"},
        {"Key": "-trimpath", "Value": "true"}, {"Key": "CGO_ENABLED", "Value": "0"},
        {"Key": "GOARCH", "Value": "amd64"}, {"Key": "GOOS", "Value": "linux"}, {"Key": "GOAMD64", "Value": "v1"},
        {"Key": "vcs", "Value": "git"}, {"Key": "vcs.revision", "Value": "0123456789abcdef"},
        {"Key": "vcs.time", "Value": "2026-11-15T10:00:00Z"}, {"Key": "vcs.modified", "Value": "false"},
    ],
}


class Docs(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="thelb-sbom-test-")
        self.addCleanup(shutil.rmtree, self.dir)
        self.bin = os.path.join(self.dir, "thelb.linux.amd64")
        with open(self.bin, "wb") as f:
            f.write(b"not really a binary")
        self.s = sbom.Subject(self.bin, "v4.3.0", INFO)
        self.now = "2026-11-15T10:05:00Z"

    def test_cyclonedx(self):
        d = sbom.cyclonedx(self.s, "build", self.now)
        self.assertEqual((d["bomFormat"], d["specVersion"], d["version"]), ("CycloneDX", "1.6", 1))
        self.assertRegex(d["serialNumber"], r"^urn:uuid:[0-9a-f-]{36}$")
        m = d["metadata"]
        self.assertEqual(m["timestamp"], self.now)
        self.assertEqual(m["lifecycles"], [{"phase": "build"}])
        c = m["component"]
        self.assertEqual((c["name"], c["version"], c["purl"]), ("thelb", "v4.3.0", "pkg:golang/github.com/dmartsapp/thelb@v4.3.0"))
        self.assertEqual(c["hashes"], [{"alg": "SHA-256", "content": self.s.sha256}])
        self.assertEqual(c["licenses"], [{"license": {"id": "MIT"}}])
        props = {p["name"]: p["value"] for p in c["properties"]}
        self.assertEqual(props["thelb:build:GOOS"], "linux")
        self.assertEqual(props["thelb:build:vcs.revision"], "0123456789abcdef")
        self.assertNotIn("thelb:build:-ldflags", props)
        self.assertIn("https://github.com/dmartsapp/thelb/releases/download/v4.3.0/thelb.linux.amd64",
                      [r["url"] for r in c["externalReferences"]])
        names = [x["name"] for x in d["components"]]
        self.assertEqual(names, ["stdlib", "github.com/New/Thing", "github.com/spf13/cobra", "golang.org/x/net"])
        std = d["components"][0]
        self.assertEqual((std["version"], std["purl"], std["cpe"]),
                         ("go1.27.3", "pkg:golang/stdlib@1.27.3", "cpe:2.3:a:golang:go:1.27.3:-:*:*:*:*:*:*"))
        new = d["components"][1]
        self.assertEqual(new["purl"], "pkg:golang/github.com/New/Thing@v1.0.1")
        self.assertEqual({p["name"]: p["value"] for p in new["properties"]},
                         {"thelb:go.sum": "h1:new=", "thelb:replaces": "github.com/Old/Thing"})
        self.assertEqual(new["externalReferences"][0]["url"], "https://proxy.golang.org/github.com/!new/!thing/@v/v1.0.1.zip")
        refs = [c["bom-ref"]] + [x["bom-ref"] for x in d["components"]]
        self.assertEqual(len(refs), len(set(refs)))
        top = d["dependencies"][0]
        self.assertEqual((top["ref"], len(top["dependsOn"])), (c["bom-ref"], 4))
        self.assertEqual({x["ref"] for x in d["dependencies"]}, set(refs))

    def test_spdx(self):
        d = sbom.spdx(self.s, "build", self.now)
        self.assertEqual((d["spdxVersion"], d["dataLicense"], d["SPDXID"]), ("SPDX-2.3", "CC0-1.0", "SPDXRef-DOCUMENT"))
        self.assertEqual(d["name"], "thelb.linux.amd64 v4.3.0")
        self.assertRegex(d["documentNamespace"],
                         r"^https://github\.com/dmartsapp/thelb/releases/download/v4\.3\.0/thelb\.linux\.amd64\.spdx\.json\?uuid=[0-9a-f-]{36}$")
        self.assertEqual(d["creationInfo"]["created"], self.now)
        self.assertTrue(all(re.match(r"^(Tool|Organization|Person): .+", c) for c in d["creationInfo"]["creators"]))
        ids = [p["SPDXID"] for p in d["packages"]]
        self.assertEqual(len(ids), len(set(ids)))
        for i in ids:
            self.assertRegex(i, r"^SPDXRef-[A-Za-z0-9.-]+$")
        for p in d["packages"]:
            for k in ("name", "SPDXID", "downloadLocation"):
                self.assertTrue(p.get(k), (k, p))
        root = d["packages"][0]
        self.assertEqual(root["checksums"], [{"algorithm": "SHA256", "checksumValue": self.s.sha256}])
        self.assertEqual((root["licenseDeclared"], root["primaryPackagePurpose"]), ("MIT", "APPLICATION"))
        self.assertIn("GOOS=linux", root["comment"])
        std = d["packages"][1]
        self.assertIn({"referenceCategory": "SECURITY", "referenceType": "cpe23Type",
                       "referenceLocator": "cpe:2.3:a:golang:go:1.27.3:-:*:*:*:*:*:*"}, std["externalRefs"])
        rels = d["relationships"]
        self.assertEqual(rels[0], {"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": root["SPDXID"]})
        self.assertEqual({r["relatedSpdxElement"] for r in rels[1:]}, set(ids[1:]))
        self.assertTrue(all(r["relationshipType"] == "DEPENDS_ON" for r in rels[1:]))

    def test_made_after_the_release_says_so(self):
        c = sbom.cyclonedx(self.s, "post-build", self.now)
        self.assertEqual(c["metadata"]["lifecycles"], [{"phase": "post-build"}])
        self.assertIn("thelb:sbom:made", [p["name"] for p in c["metadata"]["component"]["properties"]])
        self.assertIn("made after the release", sbom.spdx(self.s, "post-build", self.now)["packages"][0]["comment"])

    def test_module_information_as_go_records_it(self):
        mod = ("path\texample.com/m\nmod\texample.com/m\tv1.2.3\th1:main=\n"
               "dep\tgolang.org/x/net\tv0.62.0\th1:net=\n"
               "dep\tgithub.com/Old/Thing\tv1.0.0\th1:old=\n=>\tgithub.com/New/Thing\tv1.0.1\th1:new=\n"
               "build\t-ldflags=\"-s -w -X main.Version=v1.2.3\"\nbuild\tCGO_ENABLED=0\nbuild\tGOOS=aix\n")
        info = sbom.parse_modinfo("go1.27.3", mod)
        self.assertEqual(info["Main"], {"Path": "example.com/m", "Version": "v1.2.3", "Sum": "h1:main="})
        self.assertEqual(info["Deps"][1]["Replace"], {"Path": "github.com/New/Thing", "Version": "v1.0.1", "Sum": "h1:new="})
        self.assertEqual(info["Settings"][0], {"Key": "-ldflags", "Value": "-s -w -X main.Version=v1.2.3"})
        self.assertEqual(sbom.settings(info), [("GOOS", "aix"), ("CGO_ENABLED", "0")])

    def test_the_block_read_from_a_file(self):
        mod = "path\texample.com/m\nmod\texample.com/m\tv1.2.3\t\nbuild\tGOOS=aix\n"
        wrapped = ("x" * 16 + mod + "y" * 16).encode()
        vers = b"go1.27.3"
        block = sbom.MAGIC + bytes([8, 3]) + bytes(16) + bytes([len(vers)]) + vers + bytes([len(wrapped)]) + wrapped
        with tempfile.NamedTemporaryFile(delete=False) as f:
            f.write(b"\x01\xf7" + bytes(30) + block + bytes(64))
        self.addCleanup(os.remove, f.name)
        info = sbom.scan_build_info(f.name)
        self.assertEqual((info["GoVersion"], info["Main"]["Version"], info["Settings"]), ("go1.27.3", "v1.2.3", [{"Key": "GOOS", "Value": "aix"}]))
        with open(f.name, "wb") as g:
            g.write(b"nothing here")
        self.assertIsNone(sbom.scan_build_info(f.name))

    def test_purls_escape_what_they_must(self):
        self.assertEqual(sbom.purl("example.com/a b@c", "v1.0.0"), "pkg:golang/example.com/a%20b%40c@v1.0.0")
        self.assertEqual(sbom.spdx_id("Package", "golang.org/x/net", "v0.62.0"), "SPDXRef-Package-golang.org-x-net-v0.62.0")


class Binaries(unittest.TestCase):
    """sbom.py against a Go binary built here: what Go recorded, not what the test says."""

    def test_a_real_binary(self):
        if not shutil.which(os.environ.get("GO", "go")):
            self.skipTest("no Go toolchain")
        work = tempfile.mkdtemp(prefix="thelb-sbom-bin-")
        self.addCleanup(shutil.rmtree, work)
        with open(os.path.join(work, "go.mod"), "w") as f:
            f.write("module example.com/hello\n\ngo 1.21\n")
        with open(os.path.join(work, "main.go"), "w") as f:
            f.write('package main\n\nfunc main() { println("hello") }\n')
        out = os.path.join(work, "thelb.test.bin")
        env = dict(os.environ, CGO_ENABLED="0", GOFLAGS="-buildvcs=false")
        subprocess.run([os.environ.get("GO", "go"), "build", "-trimpath", "-o", out, "."], cwd=work, env=env, check=True)
        sbom.main(["--version", "v9.9.9", "--lifecycle", "post-build", out])
        with open(out + ".cdx.json") as f:
            cdx = json.load(f)
        with open(out + ".spdx.json") as f:
            spd = json.load(f)
        goversion = subprocess.run([os.environ.get("GO", "go"), "env", "GOVERSION"], capture_output=True, text=True).stdout.split()[0]
        self.assertEqual(cdx["components"][0]["version"], goversion)
        self.assertEqual(spd["packages"][1]["versionInfo"], goversion)
        self.assertEqual(cdx["metadata"]["component"]["purl"], "pkg:golang/example.com/hello@v9.9.9")
        # Read straight from the file, the build information is what the toolchain reads.
        go = json.loads(subprocess.run([os.environ.get("GO", "go"), "version", "-m", "-json", out],
                                       capture_output=True, text=True, check=True).stdout)
        scanned = sbom.scan_build_info(out)
        for k in ("GoVersion", "Path", "Main", "Settings"):
            self.assertEqual(scanned.get(k) or None, go.get(k) or None, k)

    def test_not_a_go_binary(self):
        if not shutil.which(os.environ.get("GO", "go")):
            self.skipTest("no Go toolchain")
        with tempfile.NamedTemporaryFile(prefix="thelb.", suffix=".txt") as f:
            f.write(b"plain text")
            f.flush()
            with self.assertRaises(SystemExit):
                sbom.main(["--version", "v1.0.0", f.name])

    def test_a_release_is_named(self):
        with self.assertRaises(SystemExit):
            orig = sbom.build_info
            sbom.build_info = lambda b, go=None: dict(INFO, Main={"Path": "x", "Version": "(devel)"})
            try:
                sbom.main(["/dev/null"])
            finally:
                sbom.build_info = orig


if __name__ == "__main__":
    unittest.main()
