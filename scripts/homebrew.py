#!/usr/bin/env python3
"""공개된 안정 릴리스의 검증된 자산으로 Homebrew formula를 갱신한다."""

import base64
import hashlib
import json
import os
import re
import subprocess
import tempfile
from pathlib import Path

SOURCE = "andromedarabbit/claude-ai-relay"
TAP = "andromedarabbit/homebrew-tap"
FORMULA = "Formula/claude-ai-relay.rb"
ARCHIVE = "claude-ai-relay_darwin_arm64.tar.gz"


def version_key(tag):
    match = re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", tag)
    if not match:
        raise ValueError(f"안정 버전 태그가 아닙니다: {tag}")
    return tuple(map(int, match.groups()))


def latest_stable(releases):
    tags = []
    for release in releases:
        if release["draft"] or release["prerelease"]:
            continue
        tag = release["tag_name"]
        try:
            version_key(tag)
        except ValueError:
            continue
        tags.append(tag)
    if not tags:
        raise ValueError("공개된 안정 릴리스가 없습니다")
    return max(tags, key=version_key)


def verified_checksum(data, checksums, name):
    matches = []
    for line in checksums.splitlines():
        match = re.fullmatch(r"([0-9a-fA-F]{64})\s+\*?(.+)", line)
        if match and match.group(2) == name:
            matches.append(match.group(1).lower())
    digest = hashlib.sha256(data).hexdigest()
    if len(matches) != 1 or matches[0] != digest:
        raise ValueError("아카이브 체크섬이 없거나 중복되었거나 일치하지 않습니다")
    return digest


def formula(tag, digest):
    version_key(tag)
    return f'''class ClaudeAiRelay < Formula
  desc "claude.ai connectors as MCP tools in API-key sessions"
  homepage "https://github.com/{SOURCE}"
  version "{tag[1:]}"
  license "MIT"

  depends_on :macos
  depends_on arch: :arm64

  url "https://github.com/{SOURCE}/releases/download/{tag}/{ARCHIVE}"
  sha256 "{digest}"

  def install
    bin.install "claude-ai-relay"
  end

  test do
    system bin/"claude-ai-relay", "selfcheck"
  end
end
'''


def needs_update(current, tag, digest):
    if current is None:
        return True
    versions = re.findall(r'^\s*version\s+"([^"]+)"', current, re.MULTILINE)
    if len(versions) != 1:
        raise ValueError("기존 formula 버전을 확인할 수 없습니다")
    old_tag = "v" + versions[0]
    old_version = version_key(old_tag)
    new_version = version_key(tag)
    if old_version > new_version:
        raise ValueError(f"기존 formula {old_tag}가 최신 공개 안정 릴리스 {tag}보다 높습니다. 공개 상태를 확인하세요")
    if old_version < new_version:
        return True
    expected_url = f"https://github.com/{SOURCE}/releases/download/{tag}/{ARCHIVE}"
    pattern = r'url\s+"' + re.escape(expected_url) + r'"\s*\n\s*sha256\s+"([0-9a-fA-F]{64})"'
    hashes = re.findall(pattern, current)
    if len(hashes) != 1 or hashes[0].lower() != digest:
        raise ValueError("동일 버전의 arm64 URL 또는 해시가 다르거나 확인할 수 없습니다")
    return False


def gh(args, token):
    env = dict(os.environ, GH_TOKEN=token, GH_PROMPT_DISABLED="1")
    return subprocess.run(["gh", *args], env=env, check=True, stdout=subprocess.PIPE).stdout


def latest(token):
    pages = json.loads(gh(["api", "--paginate", "--slurp", f"repos/{SOURCE}/releases?per_page=100"], token))
    return latest_stable([release for page in pages for release in page])


def main():
    source_token = os.environ["GITHUB_TOKEN"]
    tap_token = os.environ["HOMEBREW_TAP_TOKEN"]
    if not source_token or not tap_token:
        raise ValueError("릴리스 조회 또는 tap 쓰기 토큰이 비어 있습니다")
    tag = latest(source_token)
    with tempfile.TemporaryDirectory() as directory:
        gh(["release", "download", tag, "--repo", SOURCE, "--dir", directory,
            "--pattern", ARCHIVE, "--pattern", "checksums.txt"], source_token)
        digest = verified_checksum((Path(directory) / ARCHIVE).read_bytes(),
                                   (Path(directory) / "checksums.txt").read_text(), ARCHIVE)
    listing = json.loads(gh(["api", f"repos/{TAP}/contents/Formula?ref=main"], tap_token))
    entries = [entry for entry in listing if entry["path"] == FORMULA]
    if len(entries) > 1:
        raise ValueError("formula가 중복되었습니다")
    sha = entries[0]["sha"] if entries else None
    current = None
    if sha:
        item = json.loads(gh(["api", f"repos/{TAP}/contents/{FORMULA}?ref=main"], tap_token))
        if item["sha"] != sha:
            raise ValueError("조회 중 formula가 변경되었습니다. 다시 실행하세요")
        current = base64.b64decode(item["content"]).decode()
    if not needs_update(current, tag, digest):
        print(f"formula가 이미 {tag}입니다")
        return
    if latest(source_token) != tag:
        raise ValueError("최신 릴리스가 변경되었습니다. 다시 실행하세요")
    payload = {"message": f"Update claude-ai-relay to {tag}", "branch": "main",
               "content": base64.b64encode(formula(tag, digest).encode()).decode()}
    if sha:
        payload["sha"] = sha
    env = dict(os.environ, GH_TOKEN=tap_token, GH_PROMPT_DISABLED="1")
    subprocess.run(["gh", "api", "--method", "PUT", f"repos/{TAP}/contents/{FORMULA}", "--input", "-", "--silent"],
                   input=json.dumps(payload).encode(), env=env, check=True)
    print(f"formula를 {tag}(으)로 갱신했습니다")


if __name__ == "__main__":
    main()
