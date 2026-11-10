#!/usr/bin/env python3
"""文書の照合。

1. コード (internal/config、internal/relay、internal/capture) が読む環境変数の集合と、
   docs/configuration.md の「このプログラムの設定」 (config-check の印の範囲) に
   書かれた名前の集合が一致することを確かめる。
2. 廃止した用語が、文書とコードに再混入していないことを確かめる。

Python の標準ライブラリだけを使う。誤りがあれば、その内容を出して 1 で終わる。
"""

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# 環境変数の読み取りがあるファイル。新しい読み取りを足すときは、ここにも足す。
CODE_FILES = [
    "internal/config/config.go",
    "internal/relay/relay.go",
    "internal/capture/shot.go",
]

CONFIG_DOC = "docs/configuration.md"
BEGIN = "<!-- config-check:begin -->"
END = "<!-- config-check:end -->"

# 廃止した用語 (比喩的な旧用語)。このスクリプト自身は検査の対象から外す。
FORBIDDEN_TERMS = ["窓口", "見張り", "手元"]
SCAN_SUFFIXES = {".md", ".go", ".yaml", ".yml"}


def code_env_names() -> set:
    names = set()
    simple = re.compile(r'(?:envStr|envInt|envBool|envDur)\(\s*"([A-Z][A-Z0-9_]*)"')
    secret = re.compile(r'envSecret\(\s*"([A-Z][A-Z0-9_]*)"')
    getenv = re.compile(r'os\.Getenv\(\s*"([A-Z][A-Z0-9_]*)"')
    first_secret = re.compile(r"firstSecret\(([^)]*)\)")
    literal = re.compile(r'"([A-Z][A-Z0-9_]*)"')
    for rel in CODE_FILES:
        text = (ROOT / rel).read_text(encoding="utf-8")
        names.update(simple.findall(text))
        names.update(getenv.findall(text))
        for name in secret.findall(text):
            names.add(name)
            names.add(name + "_FILE")
        for args in first_secret.findall(text):
            for name in literal.findall(args):
                names.add(name)
                names.add(name + "_FILE")
    return names


def doc_env_names() -> set:
    text = (ROOT / CONFIG_DOC).read_text(encoding="utf-8")
    if BEGIN not in text or END not in text:
        raise SystemExit(f"{CONFIG_DOC} に照合範囲の印 ({BEGIN} / {END}) が無い")
    region = text.split(BEGIN, 1)[1].split(END, 1)[0]
    names = set()
    for span in re.findall(r"`([^`]+)`", region):
        names.update(re.findall(r"[A-Z][A-Z0-9_]{2,}", span))
    return names


def forbidden_hits() -> list:
    hits = []
    self_path = Path(__file__).resolve()
    for path in sorted(ROOT.rglob("*")):
        if not path.is_file() or path.suffix not in SCAN_SUFFIXES:
            continue
        if ".git" in path.parts or path.resolve() == self_path:
            continue
        text = path.read_text(encoding="utf-8", errors="ignore")
        for term in FORBIDDEN_TERMS:
            if term in text:
                hits.append(f"{path.relative_to(ROOT)}: {term}")
    return hits


def main() -> int:
    failed = False
    code_names = code_env_names()
    doc_names = doc_env_names()
    only_code = sorted(code_names - doc_names)
    only_doc = sorted(doc_names - code_names)
    if only_code:
        failed = True
        print("コードが読むが docs/configuration.md に無い環境変数:")
        for name in only_code:
            print(f"  {name}")
    if only_doc:
        failed = True
        print("docs/configuration.md にあるがコードが読まない環境変数:")
        for name in only_doc:
            print(f"  {name}")
    if not only_code and not only_doc:
        print(f"環境変数の照合: 一致 ({len(code_names)} 件)")
    hits = forbidden_hits()
    if hits:
        failed = True
        print("廃止した用語が残っている:")
        for hit in hits:
            print(f"  {hit}")
    else:
        print("廃止した用語: 無し")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
