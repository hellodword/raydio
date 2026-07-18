#!/usr/bin/env python3
"""Build the static Raydio player for GitHub Pages."""

from __future__ import annotations

import argparse
import json
import shutil
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit


RUNTIME_FILES = ("index.html", "app.js", "styles.css")
MAX_BASE_URL_LENGTH = 2048
FORBIDDEN_URL_CHARACTERS = frozenset("\\\"'<>")


def normalize_base_url(raw: str) -> str:
    if not raw:
        raise ValueError("base URL is required")
    if len(raw) > MAX_BASE_URL_LENGTH:
        raise ValueError(f"base URL must not exceed {MAX_BASE_URL_LENGTH} characters")
    if raw != raw.strip() or any(
        character.isspace()
        or ord(character) < 0x20
        or character in FORBIDDEN_URL_CHARACTERS
        for character in raw
    ):
        raise ValueError("base URL contains unsupported characters")

    try:
        parsed = urlsplit(raw)
    except ValueError as error:
        raise ValueError("base URL is invalid") from error
    if parsed.scheme.lower() != "https":
        raise ValueError("base URL must use https")
    if not parsed.netloc or parsed.hostname is None:
        raise ValueError("base URL must include a host")
    if parsed.username is not None or parsed.password is not None:
        raise ValueError("base URL must not include credentials")
    if parsed.query or parsed.fragment:
        raise ValueError("base URL must not include a query or fragment")
    try:
        parsed.port
    except ValueError as error:
        raise ValueError("base URL contains an invalid port") from error

    path = parsed.path.rstrip("/")
    return urlunsplit(("https", parsed.netloc, path, "", ""))


def build_pages(source: Path, output: Path, raw_base_url: str) -> str:
    source = source.resolve(strict=True)
    output = output.resolve(strict=False)
    if output.exists():
        raise ValueError(f"output path already exists: {output}")
    if source == output or source in output.parents:
        raise ValueError("output path must be outside the source directory")

    base_url = normalize_base_url(raw_base_url)
    for name in RUNTIME_FILES:
        if not (source / name).is_file():
            raise ValueError(f"missing runtime asset: {source / name}")

    output.mkdir(parents=True)
    try:
        for name in RUNTIME_FILES:
            shutil.copyfile(source / name, output / name)
        config = json.dumps(
            {"apiBaseUrl": base_url},
            ensure_ascii=True,
            separators=(",", ":"),
        )
        (output / "config.json").write_text(
            f"{config}\n",
            encoding="utf-8",
        )
    except Exception:
        shutil.rmtree(output)
        raise
    return base_url


def main() -> None:
    repository_root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--source",
        type=Path,
        default=repository_root / "web",
        help="directory containing the static player assets",
    )
    parser.add_argument("--output", type=Path, required=True, help="new output directory")
    parser.add_argument("--base-url", required=True, help="public HTTPS Raydio API base URL")
    args = parser.parse_args()
    try:
        build_pages(args.source, args.output, args.base_url)
    except (OSError, ValueError) as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
