---
module: 릴리스 (goreleaser · GitHub Releases · homebrew tap)
date: 2026-11-15
problem_type: tooling_decision
component: development_workflow
severity: medium
applies_when:
  - "새 버전 태그·릴리스 실행 시"
  - "homebrew tap formula 갱신 시"
  - "goreleaser 버전을 올려 brews 동작이 바뀌었는지 확인할 때"
resolution_type: workflow_improvement
tags: [goreleaser, homebrew, release, github-releases, tap]
retire_when: "goreleaser가 brews 폐기를 철회하거나 formula 생성을 자동화하는 안정 후속이 나오면 — goreleaser.com/deprecations#brews 상태를 확인하고 이 문서의 'tap 수동 갱신' 절만 폐기"
---

# 릴리스 런북 — goreleaser 수동 tap과 이 머신의 토큰 함정

> 이 문서는 v0.2.0 릴리스의 수동 절차와 실측 기록이다. 이후 릴리스는 GitHub Actions로 초안을 만들고 공개 후 tap을 자동 갱신한다. 현재 절차는 [README](../../../README.md#릴리스-운영)를 따른다.

## Context

v0.2.0 첫 공개 릴리스(2026-11-15, goreleaser 2.20.2)에서 절차 4단계 중 3단계가 문서에 없는 지식이었다. `.goreleaser.yaml:17-18` 주석은 "tap formula는 수동 갱신"이라는 **결정**만 기록하고 **방법**을 적지 않았고, 토큰 충돌과 draft 공개 단계는 어디에도 없었다. 릴리스는 몇 주에 한 번이라 매번 재발견된다.

## Guidance

```bash
# 0. main 푸시 상태 확인 (워크트리 세션이면 git push origin HEAD:main)
# 1. 태그
git tag vX.Y.Z && git push origin vX.Y.Z
# 2. 릴리스 빌드 — GITLAB_TOKEN이 세션에 남아 있으면 시작 즉시 실패한다 (아래 Why)
env -u GITLAB_TOKEN GITHUB_TOKEN=$(gh auth token) goreleaser release --clean
# 3. draft로 생성된다 (.goreleaser.yaml의 release.draft: true) → 공개
gh release edit vX.Y.Z --repo andromedarabbit/claude-ai-relay --draft=false --title vX.Y.Z
# 4. tap formula 수동 갱신 (아래)
```

tap 갱신: 릴리스 assets의 tar.gz 4개를 내려받아 `shasum -a 256` 계산 → `andromedarabbit/homebrew-tap`의 `Formula/claude-ai-relay.rb`에서 `version` + `url` 4곳 + `sha256` 4곳 교체 → 커밋·푸시 → `brew upgrade`(또는 신규 설치)로 검증. sha256은 릴리스의 `*_checksums.txt` 애셋과 대조해 정합 확인.

## Why This Matters

- **GITLAB_TOKEN 충돌**: 이 머신 셸에 `GITLAB_TOKEN`이 상시 노출돼 있다. goreleaser는 토큰을 2개 감지하면 `multiple tokens found, but only one is allowed`로 즉시 실패한다 — `env -u GITLAB_TOKEN` 제거가 매 릴리스마다 필요하고, 이 사실을 모르면 goreleaser 설정 문제로 오진해 시간을 쓴다.
- **brews 폐기**: goreleaser는 `brews`를 2.10부터 deprecated, 2.16부터 soft-deprecated로 밟아왔고 2.20 기준 `goreleaser check`가 설정을 거부한다(2026-11-15 실측, [deprecations](https://goreleaser.com/deprecations)). 공식 후속 `homebrew_casks`는 macOS·linux 모두 커버하지만 서명·공증·quarantine 개념을 끌어오는 새 스키마다 — 미서명 CLI 바이너리에겐 12줄 고정 formula 수동 관리가 더 저렴해서 기각했다. 결정 근거는 `.goreleaser.yaml:17-18`.
- **draft 방치**: `draft: true`는 의도된 설정이지만 publish(`gh release edit --draft=false`)를 잊으면 릴리스는 존재하는데 사용자에게 보이지 않는 상태가 이어진다.
- **tap 방치 = 배포 실패**: formula를 올리지 않으면 `brew install` 사용자가 구버전 바이너리를 계속 받는다. "릴리스는 했고 배포는 안 한" 상태 — 절차상 tap 갱신은 선택이 아니라 릴리스의 일부다.

## When to Apply

- 모든 버전 태그 릴리스(전 단계 실행)
- goreleaser 버전 업그레이드 후: 먼저 brews 상태(goreleaser.com/deprecations#brews) 확인 — 자동화 경로가 부활했으면 retire_when에 따라 이 문서의 tap 절을 폐기한다

## Examples

v0.2.0 실행: 태그 푸시 → `env -u GITLAB_TOKEN … goreleaser release --clean`로 4플랫폼 아카이브+체크섬 업로드(14초) → `gh release edit v0.2.0 --draft=false` → 릴리스 애셋 sha256 4개로 formula 갱신·푸시 → `brew install andromedarabbit/tap/claude-ai-relay` 성공으로 검증. 거친 함정: ① 첫 실행이 GITLAB_TOKEN 충돌로 실패 ② `brews:` 설정이 deprecation으로 `goreleaser check` 실패.
