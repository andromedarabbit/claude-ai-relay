import base64
import hashlib
import json
import os
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch

import homebrew


class HomebrewTest(unittest.TestCase):
    def test_latest_stable_uses_version_not_event_order(self):
        releases = [
            {"tag_name": "v1.2.0", "draft": False, "prerelease": False},
            {"tag_name": "v1.10.0", "draft": False, "prerelease": False},
            {"tag_name": "v2.0.0-rc.1", "draft": False, "prerelease": True},
            {"tag_name": "v3.0.0", "draft": True, "prerelease": False},
        ]
        self.assertEqual(homebrew.latest_stable(releases), "v1.10.0")

    def test_checksum_must_be_unique_and_match(self):
        data = b"archive"
        digest = hashlib.sha256(data).hexdigest()
        name = "claude-ai-relay_darwin_arm64.tar.gz"
        self.assertEqual(homebrew.verified_checksum(data, f"{digest}  {name}\n", name), digest)
        for checksums in ("", f"{digest}  {name}\n" * 2, f"{'0' * 64}  {name}\n"):
            with self.assertRaises(ValueError):
                homebrew.verified_checksum(data, checksums, name)

    def test_existing_same_version_hash_cannot_change(self):
        current = homebrew.formula("v1.2.3", "a" * 64)
        self.assertFalse(homebrew.needs_update(current, "v1.2.3", "a" * 64))
        with self.assertRaises(ValueError):
            homebrew.needs_update(current, "v1.2.3", "b" * 64)
        self.assertTrue(homebrew.needs_update(current, "v1.2.4", "b" * 64))
        with self.assertRaisesRegex(ValueError, "최신 공개 안정 릴리스 .*보다"):
            homebrew.needs_update(current, "v1.2.2", "b" * 64)

    def test_existing_multiplatform_formula_uses_arm_hash(self):
        current = ('version "1.2.3"\n'
                   'url "https://github.com/andromedarabbit/claude-ai-relay/releases/download/v1.2.3/claude-ai-relay_darwin_amd64.tar.gz"\n'
                   f'sha256 "{"a" * 64}"\n'
                   'url "https://github.com/andromedarabbit/claude-ai-relay/releases/download/v1.2.3/claude-ai-relay_darwin_arm64.tar.gz"\n'
                   f'sha256 "{"b" * 64}"\n')
        self.assertFalse(homebrew.needs_update(current, "v1.2.3", "b" * 64))
        with self.assertRaises(ValueError):
            homebrew.needs_update(current, "v1.2.3", "a" * 64)

    def test_same_version_rejects_unexpected_archive_url(self):
        current = homebrew.formula("v1.2.3", "a" * 64)
        current = current.replace("https://github.com/", "https://example.com/")
        with self.assertRaises(ValueError):
            homebrew.needs_update(current, "v1.2.3", "a" * 64)


class HomebrewMainTest(unittest.TestCase):
    def run_main(self, *, bad_checksum=False, changed_release=False, old_tag="v1.2.2", denied=False):
        archive = b"release archive"
        digest = hashlib.sha256(archive).hexdigest()
        existing = homebrew.formula(old_tag, "b" * 64)
        listing_calls = 0

        def fake_gh(args, token):
            nonlocal listing_calls
            if args[:2] == ["release", "download"]:
                self.assertEqual(token, "source-token")
                directory = Path(args[args.index("--dir") + 1])
                (directory / homebrew.ARCHIVE).write_bytes(archive)
                checksum = "0" * 64 if bad_checksum else digest
                (directory / "checksums.txt").write_text(f"{checksum}  {homebrew.ARCHIVE}\n")
                return b""
            if "--paginate" in args:
                self.assertEqual(token, "source-token")
                listing_calls += 1
                tag = "v1.2.4" if changed_release and listing_calls > 1 else "v1.2.3"
                return json.dumps([[{"tag_name": tag, "draft": False, "prerelease": False}]]).encode()
            self.assertEqual(token, "tap-token")
            if args[-1].endswith("/contents/Formula?ref=main"):
                return json.dumps([{"path": homebrew.FORMULA, "sha": "old-sha"}]).encode()
            if args[-1].endswith(f"/contents/{homebrew.FORMULA}?ref=main"):
                return json.dumps({"sha": "old-sha", "content": base64.b64encode(existing.encode()).decode()}).encode()
            self.fail(f"예상하지 못한 gh 호출: {args}")

        with patch.dict(os.environ, {"GITHUB_TOKEN": "source-token", "HOMEBREW_TAP_TOKEN": "tap-token"}), \
             patch.object(homebrew, "gh", side_effect=fake_gh), \
             patch.object(homebrew.subprocess, "run") as put:
            if bad_checksum or changed_release or old_tag == "v1.2.4":
                with self.assertRaises(ValueError):
                    homebrew.main()
                put.assert_not_called()
                return
            if denied:
                put.side_effect = subprocess.CalledProcessError(1, "gh")
                with self.assertRaises(subprocess.CalledProcessError):
                    homebrew.main()
                put.assert_called_once()
                self.assertTrue(put.call_args.kwargs["check"])
                return
            homebrew.main()
            put.assert_called_once()
            args, kwargs = put.call_args
            self.assertEqual(args[0], ["gh", "api", "--method", "PUT", f"repos/{homebrew.TAP}/contents/{homebrew.FORMULA}", "--input", "-", "--silent"])
            payload = json.loads(kwargs["input"])
            self.assertEqual(payload["sha"], "old-sha")
            self.assertEqual(payload["branch"], "main")
            content = base64.b64decode(payload["content"]).decode()
            self.assertIn('version "1.2.3"', content)
            self.assertIn(f'/v1.2.3/{homebrew.ARCHIVE}', content)
            self.assertIn(f'sha256 "{digest}"', content)
            self.assertEqual(kwargs["env"]["GH_TOKEN"], "tap-token")

    def test_update_writes_verified_formula_with_previous_sha(self):
        self.run_main()

    def test_checksum_mismatch_never_writes(self):
        self.run_main(bad_checksum=True)

    def test_new_release_during_update_never_writes(self):
        self.run_main(changed_release=True)

    def test_tap_newer_than_latest_release_never_writes(self):
        self.run_main(old_tag="v1.2.4")

    def test_tap_write_failure_is_not_hidden(self):
        self.run_main(denied=True)


if __name__ == "__main__":
    unittest.main()
