#!/usr/bin/env python3
"""Generates NOTICE: the licences of everything Fabo ships.

Three parts, in this order:

1. The Go modules linked into the shipped binaries (fabo, fabo-daemon and
   fabo-desktop for macOS, fabo, fabo-daemon and fabo-init for Linux), and
   Go's own standard library and runtime. Each licence text appears once,
   with the modules that use it; a module's NOTICE file follows its licence.
2. packaging/notice/bundled.txt: what the app bundles or downloads beside the
   Go code (docker CLI, Compose, buildx, Grype, Simple Icons, the guest
   kernel's GPL text). It is kept by hand; this script checks that its
   versions match the pins in packaging/cli-pins.env and the scanner.
3. guest/kernel/SOURCE_OFFER.md, the GPL source offer for the guest kernel.

    python3 scripts/notice.py            # writes NOTICE
    python3 scripts/notice.py --check    # fails when NOTICE is out of date
"""
import hashlib
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# The shipped binaries: (module directory, packages, GOOS, GOARCH, cgo).
# Both architectures link the same modules, so one per OS is enough.
TARGETS = [
    ("engine", ["./cmd/fabo", "./cmd/fabo-daemon"], "darwin", "arm64", "1"),
    ("desktop", ["."], "darwin", "arm64", "1"),
    ("engine", ["./cmd/fabo", "./cmd/fabo-daemon", "./cmd/fabo-init"], "linux", "amd64", "0"),
]

LICENSE_NAMES = re.compile(r"^(LICEN[CS]E|COPYING|UNLICENSE)([-._][A-Za-z0-9.-]+)?(\.md|\.txt)?$", re.I)
NOTICE_NAMES = re.compile(r"^NOTICE(\.md|\.txt)?$", re.I)

RULE = "-" * 70


# modules lists the third-party modules the shipped binaries link, as
# {path@version: directory}.
def modules() -> dict:
    found = {}
    for moddir, pkgs, goos, goarch, cgo in TARGETS:
        env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED=cgo, GOFLAGS="-mod=mod")
        out = subprocess.run(
            ["go", "-C", os.path.join(ROOT, moddir), "list", "-deps", "-json=Module", *pkgs],
            env=env, check=True, capture_output=True, text=True,
        ).stdout
        # go list -json writes one object after another, not an array.
        decoder, at = json.JSONDecoder(), 0
        while at < len(out):
            if out[at].isspace():
                at += 1
                continue
            obj, at = decoder.raw_decode(out, at)
            mod = obj.get("Module")
            if not mod or mod.get("Main"):
                continue
            # A replaced module is the replacement's code.
            used = mod.get("Replace") or mod
            key = f"{mod['Path']}@{used.get('Version') or 'local'}"
            found[key] = used.get("Dir") or mod.get("Dir")
    return found


# files reads the licence (and NOTICE) files at the top of a module.
def files(directory: str, pattern) -> list:
    texts = []
    for name in sorted(os.listdir(directory)):
        path = os.path.join(directory, name)
        if pattern.match(name) and os.path.isfile(path):
            texts.append(open(path, encoding="utf-8", errors="replace").read().strip())
    return texts


# kind names a licence text by its wording, for the group's heading.
def kind(text: str) -> str:
    t = " ".join(text.split())
    checks = [
        ("Apache License 2.0", "Apache License" in t and "Version 2.0" in t),
        ("Mozilla Public License 2.0", "Mozilla Public License" in t and "2.0" in t),
        ("ISC License", "Permission to use, copy, modify, and/or distribute this software for any purpose" in t and "ISC" in t),
        ("BSD Zero Clause License", "Permission to use, copy, modify, and/or distribute this software for any purpose" in t),
        ("BSD 3-Clause License", "Neither the name" in t or "neither the name" in t),
        ("BSD 2-Clause License", "Redistribution and use in source and binary forms" in t),
        ("MIT License", "Permission is hereby granted, free of charge" in t),
        ("CC0 1.0", "CC0" in t),
        ("Creative Commons Attribution-ShareAlike 4.0", "Attribution-ShareAlike 4.0" in t),
        ("Unlicense", "unlicense" in t.lower()),
    ]
    for name, ok in checks:
        if ok:
            return name
    return "Other licence"


# go_section writes the Go part: each distinct licence text once, the
# modules that carry it, then any NOTICE files they ship.
def go_section() -> str:
    groups = {}
    missing = []
    notices = []
    for key, directory in sorted(modules().items()):
        texts = files(directory, LICENSE_NAMES)
        if not texts:
            missing.append(key)
            continue
        for text in texts:
            digest = hashlib.sha256(text.encode()).hexdigest()
            groups.setdefault(digest, {"text": text, "used": []})["used"].append(key)
        for text in files(directory, NOTICE_NAMES):
            notices.append((key, text))
    if missing:
        sys.exit("no licence file in: " + ", ".join(missing))

    # The engine module's toolchain is the one that builds the release.
    env = ["go", "-C", os.path.join(ROOT, "engine"), "env"]
    goroot = subprocess.run(env + ["GOROOT"], check=True, capture_output=True, text=True).stdout.strip()
    version = subprocess.run(env + ["GOVERSION"], check=True, capture_output=True, text=True).stdout.strip()
    std = open(os.path.join(goroot, "LICENSE"), encoding="utf-8").read().strip()

    out = ["Go modules and the Go runtime", "=" * 29, ""]
    out += [f"Go standard library and runtime ({version}) — BSD 3-Clause License", RULE, std, "", ""]
    # Most used first, so the common licences lead.
    for g in sorted(groups.values(), key=lambda g: (-len(g["used"]), kind(g["text"]), g["used"][0])):
        out += [kind(g["text"]), RULE, "Used by:"]
        out += [f"- {key}" for key in g["used"]]
        out += ["", g["text"], "", ""]
    for key, text in notices:
        out += [f"{key} — upstream NOTICE", RULE, text, "", ""]
    return "\n".join(out)


# check_pins makes sure bundled.txt names the versions actually shipped.
def check_pins(bundled: str) -> None:
    pins = open(os.path.join(ROOT, "packaging/cli-pins.env"), encoding="utf-8").read()
    wanted = {
        "Docker CLI": re.search(r'^DOCKER_CLI="v?([0-9.]+)', pins, re.M).group(1),
        "Docker Compose": re.search(r'^COMPOSE="v?([0-9.]+)', pins, re.M).group(1),
        "Docker Buildx": re.search(r'^BUILDX="v?([0-9.]+)', pins, re.M).group(1),
    }
    scanner = open(os.path.join(ROOT, "engine/internal/imagescan/scanner.go"), encoding="utf-8").read()
    wanted["Grype"] = re.search(r'const version = "([0-9.]+)"', scanner).group(1)
    for name, version in wanted.items():
        if f"\n{name} {version} — " not in "\n" + bundled:
            sys.exit(f"packaging/notice/bundled.txt does not name {name} {version}; update it from the upstream licence")


# build assembles the whole file.
def build() -> str:
    bundled = open(os.path.join(ROOT, "packaging/notice/bundled.txt"), encoding="utf-8").read().strip()
    check_pins(bundled)
    offer = open(os.path.join(ROOT, "guest/kernel/SOURCE_OFFER.md"), encoding="utf-8").read().strip()
    head = "\n".join([
        "Fabo third-party notices",
        "",
        "Fabo is licensed under Apache-2.0. This file contains the licence and",
        "attribution notices for code linked into, bundled with or downloaded by",
        "Fabo. The guest Linux kernel is a separate GPL-2.0-only work; its source",
        "offer is at the end. Generated by scripts/notice.py; do not edit by hand.",
    ])
    parts = [
        head,
        go_section(),
        "Bundled and downloaded components\n" + "=" * 33 + "\n\n" + bundled,
        "Guest kernel source offer\n" + RULE + "\n\n" + offer,
    ]
    return "\n\n\n".join(parts) + "\n"


def main() -> None:
    text = build()
    path = os.path.join(ROOT, "NOTICE")
    if "--check" in sys.argv:
        if open(path, encoding="utf-8").read() != text:
            sys.exit("NOTICE is out of date: run python3 scripts/notice.py")
        return
    open(path, "w", encoding="utf-8").write(text)


if __name__ == "__main__":
    main()
