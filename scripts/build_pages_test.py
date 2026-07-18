from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from scripts.build_pages import build_pages, normalize_base_url


class NormalizeBaseURLTest(unittest.TestCase):
    def test_accepts_https_origin_and_path_prefix(self) -> None:
        self.assertEqual(
            normalize_base_url("https://api.example.test:8443/raydio/"),
            "https://api.example.test:8443/raydio",
        )
        self.assertEqual(
            normalize_base_url("HTTPS://api.example.test/"),
            "https://api.example.test",
        )

    def test_rejects_unsafe_or_unusable_values(self) -> None:
        invalid_values = (
            "",
            "http://api.example.test",
            " https://api.example.test",
            "https://user:secret@api.example.test",
            "https://api.example.test?mode=test",
            "https://api.example.test#fragment",
            "https:///missing-host",
            "https://api.example.test\\other",
            "https://api.example.test/path with space",
            "https://api.example.test:invalid",
        )
        for value in invalid_values:
            with self.subTest(value=value), self.assertRaises(ValueError):
                normalize_base_url(value)


class BuildPagesTest(unittest.TestCase):
    def test_builds_only_runtime_assets_with_generated_config(self) -> None:
        repository_root = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "_site"
            build_pages(
                repository_root / "web",
                output,
                "https://api.example.test/raydio/",
            )

            self.assertEqual(
                {path.name for path in output.iterdir()},
                {"index.html", "app.js", "styles.css", "config.json"},
            )
            self.assertEqual(
                (output / "config.json").read_text(encoding="utf-8"),
                '{"apiBaseUrl":"https://api.example.test/raydio"}\n',
            )
            index = (output / "index.html").read_text(encoding="utf-8")
            self.assertIn('href="./styles.css"', index)
            self.assertNotIn("config.js", index)
            self.assertIn('src="./app.js"', index)

    def test_refuses_to_overwrite_an_existing_output(self) -> None:
        repository_root = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "_site"
            output.mkdir()
            sentinel = output / "keep"
            sentinel.write_text("keep", encoding="utf-8")

            with self.assertRaises(ValueError):
                build_pages(repository_root / "web", output, "https://api.example.test")
            self.assertEqual(sentinel.read_text(encoding="utf-8"), "keep")


if __name__ == "__main__":
    unittest.main()
