#!/usr/bin/env python3
"""Checks installer/windows/fabo.wxs where WiX cannot run (macOS, Linux).

WiX builds MSIs on Windows only, so mistakes in fabo.wxs would otherwise
show up there first. This checks what it can anywhere:
  - the file is well-formed XML;
  - Id attributes are unique per element kind (File, Component, ...);
  - every $(Bundle)\\... source exists in the bundle, and every bundle file
    is installed (a new file in scripts/windows-bundle.sh must be added here);
  - every other source file (icon, licence) exists;
  - GUIDs are well-formed and unique, and the UpgradeCode is the one
    fabo-win's updater checks (engine/cmd/fabo-win/update.go).

    scripts/check-wxs.py [bundle dir]   (default target/windows/fabo-windows-x86_64)
"""
import os
import re
import sys
import xml.etree.ElementTree as ET

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
WXS = os.path.join(ROOT, "installer", "windows", "fabo.wxs")
NS = "{http://wixtoolset.org/schemas/v4/wxs}"


def main() -> int:
    bundle = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "target", "windows", "fabo-windows-x86_64")
    problems = []
    tree = ET.parse(WXS)  # raises on malformed XML; the file is the repo's own
    ids = {}
    guids = []
    sources = set()
    for el in tree.iter():
        tag = el.tag.split("}")[-1]
        if "Id" in el.attrib and tag not in ("Publish", "Custom", "ComponentGroupRef", "UIRef", "WixVariable"):
            # A SetProperty's action is its own Action, when given.
            key = (tag, el.attrib.get("Action", el.attrib["Id"]))
            if key in ids:
                problems.append(f"duplicate {tag} Id {el.attrib['Id']}")
            ids[key] = True
        for attr in ("Guid", "UpgradeCode"):
            if attr in el.attrib:
                g = el.attrib[attr]
                if not re.fullmatch(r"[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}", g):
                    problems.append(f"{tag} {attr} {g} is not an upper-case GUID")
                guids.append(g)
        for attr in ("Source", "SourceFile"):
            src = el.attrib.get(attr)
            if not src:
                continue
            if src.startswith("$(Bundle)\\"):
                rel = src[len("$(Bundle)\\"):].replace("\\", "/")
                sources.add(rel)
                if not os.path.isfile(os.path.join(bundle, rel)):
                    problems.append(f"{rel} is not in {bundle}")
            elif "$(" not in src:
                path = os.path.normpath(os.path.join(os.path.dirname(WXS), src.replace("\\", "/")))
                if not os.path.isfile(path):
                    problems.append(f"{src} does not exist")
    if len(set(guids)) != len(guids):
        problems.append("a GUID is used twice")
    for dirpath, _, files in os.walk(bundle):
        for f in files:
            rel = os.path.relpath(os.path.join(dirpath, f), bundle).replace(os.sep, "/")
            if rel not in sources:
                problems.append(f"{rel} is in the bundle but not installed by fabo.wxs")
    package = tree.getroot().find(f"{NS}Package")
    code = package.attrib.get("UpgradeCode", "") if package is not None else ""
    with open(os.path.join(ROOT, "engine", "cmd", "fabo-win", "update.go"), encoding="utf-8") as f:
        m = re.search(r'upgradeCode = "\{([0-9A-F-]+)\}"', f.read())
    if not m or m.group(1) != code:
        problems.append(f"UpgradeCode {code} differs from engine/cmd/fabo-win/update.go")
    for p in problems:
        print("check-wxs:", p, file=sys.stderr)
    if not problems:
        print(f"check-wxs: fabo.wxs matches {bundle}")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
