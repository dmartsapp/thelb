#!/usr/bin/env python3
"""Writes the software bill of materials (SBOM) of theLB release binaries, in
CycloneDX 1.6 JSON and SPDX 2.3 JSON:

    python3 .github/scripts/sbom.py --version v0.1.0 dist/thelb-0.1.0-linux-amd64

writes dist/thelb-0.1.0-linux-amd64.cdx.json and dist/thelb-0.1.0-linux-amd64.spdx.json.

It reads the build information the Go toolchain embeds in every Go binary -
with `go version -m -json`, which reads the file and never runs it - so an
SBOM lists exactly what that binary was built from:

- the binary itself: its SHA-256, the platform it is for, the commit, the
  build settings;
- the Go standard library, as the Go release that built it (purl
  pkg:golang/stdlib, and the CPE golang:go that vulnerability scanners match
  Go's own advisories against);
- every Go module compiled into it, with its version and its go.sum hash.

It runs in the release workflow for every binary it builds (`--lifecycle
build`), and it made the SBOMs of the releases published before it, from their
published binaries (`--lifecycle post-build`). Python 3.9 or later, standard
library only.
"""
import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import uuid
from datetime import datetime, timezone

sys.dont_write_bytecode = True

REPO = "dmartsapp/thelb"
NAME = "thelb"
LICENSE = "MIT"
SUPPLIER = "dmartsapp"
TOOL = "sbom.py"
TOOL_GROUP = "github.com/dmartsapp/thelb/.github/scripts"
TAG_RE = re.compile(r"^v\d+\.\d+\.\d+$")
SPDXID_BAD = re.compile(r"[^A-Za-z0-9.-]")
PURL_SAFE = re.compile(r"[^A-Za-z0-9.\-_~/]")

# The build settings an SBOM records, in this order; the rest of what the
# toolchain records (flags of the compiler, the full ldflags) stays out.
SETTINGS = ["GOOS", "GOARCH", "GOARM", "GOAMD64", "GOARM64", "GOPPC64", "CGO_ENABLED", "-buildmode", "-compiler",
            "-trimpath", "vcs", "vcs.revision", "vcs.time", "vcs.modified"]


def build_info(binary, go=None):
    """The build information Go embedded in binary, as `go version -m -json` gives it - or, where
    the toolchain cannot find it (it finds none in AIX's XCOFF binaries), read from the binary
    directly."""
    go = go or os.environ.get("GO", "go")
    r = subprocess.run([go, "version", "-m", "-json", binary], capture_output=True, text=True)
    if r.returncode == 0 and r.stdout.strip():
        return json.loads(r.stdout)
    info = scan_build_info(binary)
    if info is None:
        raise SystemExit("sbom.py: %s: no Go build information (%s)" % (binary, r.stderr.strip() or "none in the file"))
    return info


MAGIC = b"\xff Go buildinf:"


def uvarint(b, i):
    x = shift = 0
    while True:
        c = b[i]
        i += 1
        x |= (c & 0x7f) << shift
        if c < 0x80:
            return x, i
        shift += 7


def scan_build_info(binary):
    """The build information in binary, read as Go's debug/buildinfo reads it - the block that
    starts with MAGIC: since Go 1.18 the Go release and the module information follow its 32-byte
    header as two length-prefixed strings, the second wrapped in 16 bytes at each end - and
    parsed as runtime/debug.ParseBuildInfo parses it. None if there is none."""
    with open(binary, "rb") as f:
        data = f.read()
    at = data.find(MAGIC)
    while at >= 0:
        if at % 16 == 0 and len(data) > at + 32 and data[at + 15] & 2:
            try:
                n, i = uvarint(data, at + 32)
                vers = data[i:i + n].decode()
                n, i = uvarint(data, i + n)
                mod = data[i:i + n].decode("utf-8", "replace")
            except (IndexError, UnicodeDecodeError):
                vers, mod = "", ""
            if vers.startswith("go"):
                mod = mod[16:-16] if len(mod) >= 33 and mod[-17] == "\n" else ""
                return parse_modinfo(vers, mod)
        at = data.find(MAGIC, at + 1)
    return None


def unquote(v):
    return json.loads(v) if v.startswith('"') else v


def mod_fields(f):
    """Path, Version and - if there is one - Sum, as `go version -m -json` gives a module."""
    m = {"Path": f[1], "Version": f[2]}
    if len(f) > 3 and f[3]:
        m["Sum"] = f[3]
    return m


def parse_modinfo(vers, mod):
    """The fields of `go version -m -json` from the module information's lines: path, mod, dep,
    => (the replacement of the dep before it) and build key=value."""
    info = {"GoVersion": vers, "Path": "", "Main": {}, "Deps": [], "Settings": []}
    for line in mod.splitlines():
        f = line.split("\t")
        if f[0] == "path" and len(f) > 1:
            info["Path"] = f[1]
        elif f[0] == "mod" and len(f) > 2:
            info["Main"] = mod_fields(f)
        elif f[0] == "dep" and len(f) > 2:
            info["Deps"].append(mod_fields(f))
        elif f[0] == "=>" and len(f) > 2 and info["Deps"]:
            info["Deps"][-1]["Replace"] = mod_fields(f)
        elif f[0] == "build" and len(f) > 1:
            kv = "\t".join(f[1:])
            if kv.startswith('"'):
                key, end = json.JSONDecoder().raw_decode(kv)
                value = kv[end + 1:]
            else:
                key, _, value = kv.partition("=")
            info["Settings"].append({"Key": key, "Value": unquote(value)})
    return info


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def purl(path, version):
    """A Go module's package URL: pkg:golang/<module path>@<version>."""
    return "pkg:golang/%s@%s" % (PURL_SAFE.sub(lambda m: "%%%02X" % ord(m.group()), path), version)


def proxy_url(path, version):
    """Where the Go module proxy serves the module: upper-case letters escaped as !lower-case."""
    esc = re.sub(r"[A-Z]", lambda m: "!" + m.group().lower(), path)
    return "https://proxy.golang.org/%s/@v/%s.zip" % (esc, version)


def spdx_id(*parts):
    return "SPDXRef-" + SPDXID_BAD.sub("-", "-".join(parts))


def go_release(info):
    """go1.27.3, without anything after it (GOEXPERIMENT and the like)."""
    return info.get("GoVersion", "").split()[0]


def modules(info):
    """The modules compiled in: (path, version, go.sum hash, replaced path or None), sorted."""
    out = []
    for d in info.get("Deps") or []:
        r = d.get("Replace")
        if r:
            out.append((r["Path"], r.get("Version", ""), r.get("Sum", ""), d["Path"]))
        else:
            out.append((d["Path"], d.get("Version", ""), d.get("Sum", ""), None))
    return sorted(out)


def settings(info):
    have = {s["Key"]: s["Value"] for s in info.get("Settings") or []}
    return [(k, have[k]) for k in SETTINGS if k in have]


class Subject:
    """The binary an SBOM is about."""

    def __init__(self, path, version, info):
        self.path = path
        self.file = os.path.basename(path)
        self.version = version
        self.info = info
        self.sha256 = sha256_of(path)
        self.go = go_release(info)
        self.module = (info.get("Main") or {}).get("Path") or info.get("Path", "")
        self.download = "https://github.com/%s/releases/download/%s/%s" % (REPO, version, self.file)
        self.purl = purl(self.module, version)


def cyclonedx(s, lifecycle, now):
    root = "%s#%s" % (s.purl, s.file)
    std = "pkg:golang/stdlib@%s" % s.go[2:]
    props = [{"name": "thelb:build:" + k, "value": v} for k, v in settings(s.info)]
    if lifecycle == "post-build":
        props.append({"name": "thelb:sbom:made", "value": "after the release, from the published binary"})
    components = [{
        "type": "library", "bom-ref": std, "name": "stdlib", "version": s.go, "purl": std,
        "cpe": "cpe:2.3:a:golang:go:%s:-:*:*:*:*:*:*" % s.go[2:],
        "description": "The Go standard library, as the Go release that built the binary.",
        "externalReferences": [{"type": "distribution", "url": "https://go.dev/dl/%s.src.tar.gz" % s.go}],
    }]
    for path, version, sum_, replaced in modules(s.info):
        c = {"type": "library", "bom-ref": purl(path, version), "name": path, "version": version, "purl": purl(path, version),
             "externalReferences": [{"type": "distribution", "url": proxy_url(path, version)}]}
        p = []
        if sum_:
            p.append({"name": "thelb:go.sum", "value": sum_})
        if replaced:
            p.append({"name": "thelb:replaces", "value": replaced})
        if p:
            c["properties"] = p
        components.append(c)
    return {
        "$schema": "http://cyclonedx.org/schema/bom-1.6.schema.json",
        "bomFormat": "CycloneDX",
        "specVersion": "1.6",
        "serialNumber": "urn:uuid:%s" % uuid.uuid4(),
        "version": 1,
        "metadata": {
            "timestamp": now,
            "lifecycles": [{"phase": lifecycle}],
            "tools": {"components": [{"type": "application", "group": TOOL_GROUP, "name": TOOL}]},
            "supplier": {"name": SUPPLIER, "url": ["https://github.com/%s" % SUPPLIER]},
            "component": {
                "type": "application", "bom-ref": root, "name": NAME, "version": s.version, "purl": s.purl,
                "description": "%s, the release binary %s" % (NAME, s.file),
                "hashes": [{"alg": "SHA-256", "content": s.sha256}],
                "licenses": [{"license": {"id": LICENSE}}],
                "externalReferences": [{"type": "distribution", "url": s.download},
                                       {"type": "vcs", "url": "https://github.com/%s" % REPO}],
                "properties": props,
            },
        },
        "components": components,
        "dependencies": [{"ref": root, "dependsOn": [c["bom-ref"] for c in components]}]
                        + [{"ref": c["bom-ref"], "dependsOn": []} for c in components],
    }


def spdx(s, lifecycle, now):
    root = spdx_id("Package", NAME)
    built = ", ".join("%s=%s" % kv for kv in settings(s.info))
    comment = "The release binary %s, built with %s (%s)." % (s.file, s.go, built)
    if lifecycle == "post-build":
        comment += " This SBOM was made after the release, from the published binary."
    std = spdx_id("Package", "stdlib")
    packages = [{
        "name": NAME, "SPDXID": root, "versionInfo": s.version, "supplier": "Organization: " + SUPPLIER,
        "downloadLocation": s.download, "filesAnalyzed": False,
        "checksums": [{"algorithm": "SHA256", "checksumValue": s.sha256}],
        "licenseConcluded": LICENSE, "licenseDeclared": LICENSE, "copyrightText": "NOASSERTION",
        "primaryPackagePurpose": "APPLICATION", "comment": comment,
        "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": s.purl}],
    }, {
        "name": "stdlib", "SPDXID": std, "versionInfo": s.go, "downloadLocation": "https://go.dev/dl/%s.src.tar.gz" % s.go,
        "filesAnalyzed": False, "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
        "copyrightText": "NOASSERTION", "primaryPackagePurpose": "LIBRARY",
        "comment": "The Go standard library, as the Go release that built the binary.",
        "externalRefs": [
            {"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": "pkg:golang/stdlib@%s" % s.go[2:]},
            {"referenceCategory": "SECURITY", "referenceType": "cpe23Type", "referenceLocator": "cpe:2.3:a:golang:go:%s:-:*:*:*:*:*:*" % s.go[2:]},
        ],
    }]
    for path, version, sum_, replaced in modules(s.info):
        p = {"name": path, "SPDXID": spdx_id("Package", path, version), "versionInfo": version,
             "downloadLocation": proxy_url(path, version), "filesAnalyzed": False,
             "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION",
             "primaryPackagePurpose": "LIBRARY",
             "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": purl(path, version)}]}
        notes = []
        if sum_:
            notes.append("go.sum: " + sum_)
        if replaced:
            notes.append("replaces " + replaced)
        if notes:
            p["comment"] = "; ".join(notes)
        packages.append(p)
    rels = [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": root}]
    rels += [{"spdxElementId": root, "relationshipType": "DEPENDS_ON", "relatedSpdxElement": p["SPDXID"]} for p in packages[1:]]
    return {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": "%s %s" % (s.file, s.version),
        "documentNamespace": "https://github.com/%s/releases/download/%s/%s.spdx.json?uuid=%s" % (REPO, s.version, s.file, uuid.uuid4()),
        "creationInfo": {"created": now, "creators": ["Organization: " + SUPPLIER, "Tool: %s-%s" % (TOOL_GROUP.replace("/", "-"), TOOL)]},
        "packages": packages,
        "relationships": rels,
    }


def write(path, doc):
    with open(path, "w") as f:
        json.dump(doc, f, indent=2, sort_keys=False)
        f.write("\n")


def main(argv=None):
    ap = argparse.ArgumentParser(description="Write CycloneDX and SPDX SBOMs of theLB release binaries.")
    ap.add_argument("binaries", nargs="+", metavar="binary")
    ap.add_argument("--version", help="the release, vX.Y.Z (default: the version Go recorded for the main module)")
    ap.add_argument("--lifecycle", choices=["build", "post-build"], default="build",
                    help="build: in the release workflow; post-build: from a published binary, after the release")
    ap.add_argument("--out-dir", help="where to write them (default: next to each binary)")
    a = ap.parse_args(argv)
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if os.environ.get("SOURCE_DATE_EPOCH"):
        now = datetime.fromtimestamp(int(os.environ["SOURCE_DATE_EPOCH"]), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    for binary in a.binaries:
        info = build_info(binary)
        version = a.version or (info.get("Main") or {}).get("Version", "")
        if not TAG_RE.match(version):
            raise SystemExit("sbom.py: %s: say which release it is with --version vX.Y.Z (Go recorded %r)" % (binary, version))
        if not go_release(info).startswith("go"):
            raise SystemExit("sbom.py: %s: Go recorded no Go release (%r)" % (binary, info.get("GoVersion")))
        s = Subject(binary, version, info)
        out = a.out_dir or os.path.dirname(binary) or "."
        cdx = os.path.join(out, s.file + ".cdx.json")
        spd = os.path.join(out, s.file + ".spdx.json")
        write(cdx, cyclonedx(s, a.lifecycle, now))
        write(spd, spdx(s, a.lifecycle, now))
        print("%s: %s, %d modules -> %s, %s" % (s.file, s.go, len(modules(info)), os.path.basename(cdx), os.path.basename(spd)))


if __name__ == "__main__":
    main()
