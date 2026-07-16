#!/usr/bin/env python3
"""
Generate the self-hosted CJK companion face for the instrument-panel UI.

WHY THIS EXISTS
───────────────
The design (REGISTRY-DESIGN §5.0) commits to ONE typeface: IBM Plex Mono.
IBM Plex Mono has no CJK glyphs. Without a deliberate choice, every Chinese
string would fall back to whatever the OS happens to ship (PingFang on macOS,
Microsoft YaHei on Windows, a random DejaVu/Noto mix on Linux) — three
different typographic identities for the same product, none of them chosen.

So we pick the CJK face ourselves and self-host it:

  Latin/digits/symbols → IBM Plex Mono   (unchanged, the identity)
  CJK                  → Noto Sans SC    (this file's output)

WHY NOTO SANS SC AND NOT IBM PLEX SANS SC
──────────────────────────────────────────
IBM Plex Sans SC would be the ideal pairing (same superfamily, drawn to sit
with Plex). It is NOT distributed on npm/Fontsource — only Plex JP, KR and
Thai are (verified against the registry). Vendoring it by hand from IBM's
release would leave an unpinned, unreproducible binary blob in the repo.

Noto Sans SC is the right second choice:
  · available and self-hostable, pinned via @fontsource/noto-sans-sc
  · a low-contrast neo-grotesque with open apertures and an upright, rational
    skeleton — the same drawing logic as Plex Mono's Latin, so it reads as a
    sibling rather than a ransom note
  · CJK is inherently fixed-pitch (every han glyph fills the em square), so it
    sits in a mono column without breaking the instrument-panel grid

WHY SUBSET
──────────
Fontsource ships Noto Sans SC as ~1.09 MB of woff2 PER WEIGHT. The WebUI is
compiled into the Go binary (web/embed.go, //go:embed all:dist), so those bytes
are permanent binary weight, not a lazy CDN download. Four weights would add
~4.4 MB (+12%) to a 35 MB binary to render a UI whose Chinese vocabulary is a
few hundred characters.

Subsetting to GB2312 level-1 (3755 hanzi — ~99.7% of running modern Chinese by
frequency) costs ~516 KB/weight instead. We ship two weights, so ~1 MB total.
English-only users download none of it: @font-face is fetched per-glyph on
demand, so a browser that never renders a han glyph never requests the file.

WHY TWO WEIGHTS (400 + 600) AND NOT FOUR
─────────────────────────────────────────
`uppercase tracking-wider` is an English-only hierarchy device (see
src/index.css `.label-caps`). In Chinese we drop it and carry that hierarchy on
WEIGHT and COLOUR instead — so weight is load-bearing here and 400/600 must
both be real cut weights, never a synthetic smear. The CSS declares them with
`font-weight` RANGES (100–500 → the 400 file, 501–900 → the 600 file) so the
browser always maps to a real file and never fakes a bold, which on dense CJK
at 11–13px turns strokes into mud.

The subset charset is the UNION of:
  · GB2312 level-1          — robust to copy edits and to user-supplied CJK
                              (org display names etc.) without regenerating
  · every char in the zh-CN locale — catches any level-2/rare char the copy
                              actually uses (e.g. 罕见字) so it can't silently
                              fall back mid-sentence
  · CJK punctuation + fullwidth forms

No Latin is included on purpose: Plex Mono owns Latin, and a subset with no
Latin glyphs physically cannot steal it even if the stack order regressed.

REGENERATE
──────────
    cd web && python3 scripts/subset-cjk.py

Requires fontTools + brotli (`pip install fonttools brotli`) and the pinned
@fontsource/noto-sans-sc devDependency. Output is committed to
src/fonts/ — the build must not depend on Python being present.
"""

import json
import pathlib
import subprocess
import sys

WEB = pathlib.Path(__file__).resolve().parent.parent
SRC_DIR = WEB / "node_modules" / "@fontsource" / "noto-sans-sc" / "files"
OUT_DIR = WEB / "src" / "fonts"
LOCALE_DIR = WEB / "src" / "i18n" / "locales" / "zh-CN"

# The two cut weights the design relies on. See module docstring.
WEIGHTS = (400, 600)


def gb2312_level1() -> set[str]:
    """The 3755 most common hanzi, in GB2312 level-1 order (0xB0A1–0xD7F9)."""
    out: set[str] = set()
    for hi in range(0xB0, 0xD8):
        for lo in range(0xA1, 0xFF):
            try:
                out.add(bytes([hi, lo]).decode("gb2312"))
            except UnicodeDecodeError:
                pass
    return out


def locale_chars() -> set[str]:
    """Every character appearing in any zh-CN locale file's values."""
    files = sorted(LOCALE_DIR.glob("*.json"))
    if not files:
        print(f"  ! no locale files in {LOCALE_DIR.relative_to(WEB)} — GB2312 only",
              file=sys.stderr)
        return set()

    chars: set[str] = set()

    def walk(node) -> None:
        if isinstance(node, str):
            chars.update(node)
        elif isinstance(node, dict):
            for v in node.values():
                walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)

    for f in files:
        walk(json.loads(f.read_text(encoding="utf-8")))
    print(f"  scanned {len(files)} zh-CN locale file(s)")
    # Keep only CJK + CJK punctuation; Latin belongs to Plex Mono.
    return {c for c in chars if ord(c) > 0x2000}


# CJK punctuation, fullwidth forms, and the ideographic space.
PUNCT = set(
    "，。、；：？！…—～·《》〈〉「」『』【】〔〕（）％／＋－＝＜＞"
    "“”‘’　￥°①②③④⑤⑥⑦⑧⑨⑩"
)


def main() -> int:
    if not SRC_DIR.exists():
        print(f"error: {SRC_DIR} missing — run `npm install` first", file=sys.stderr)
        return 1

    charset = gb2312_level1() | locale_chars() | PUNCT
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    text_file = OUT_DIR / ".charset.tmp"
    text_file.write_text("".join(sorted(charset)), encoding="utf-8")

    print(f"charset: {len(charset)} chars "
          f"(gb2312-l1 + zh-CN locale + punctuation)")

    try:
        for w in WEIGHTS:
            src = SRC_DIR / f"noto-sans-sc-chinese-simplified-{w}-normal.woff2"
            if not src.exists():
                print(f"error: source missing: {src}", file=sys.stderr)
                return 1
            out = OUT_DIR / f"noto-sans-sc-subset-{w}.woff2"
            subprocess.run(
                [
                    "pyftsubset", str(src),
                    f"--text-file={text_file}",
                    "--flavor=woff2",
                    # Drop OpenType layout: we render plain text, no ligatures
                    # or contextual alternates are in play for han glyphs.
                    "--layout-features=",
                    "--desubroutinize",
                    "--name-IDs=",
                    "--notdef-outline",
                    f"--output-file={out}",
                ],
                check=True,
            )
            print(f"  {out.relative_to(WEB)}  {out.stat().st_size / 1024:.0f} KB")
    except FileNotFoundError:
        print("error: pyftsubset not found — pip install fonttools brotli",
              file=sys.stderr)
        return 1
    except subprocess.CalledProcessError as e:
        print(f"error: pyftsubset failed: {e}", file=sys.stderr)
        return 1
    finally:
        text_file.unlink(missing_ok=True)

    return 0


if __name__ == "__main__":
    sys.exit(main())
