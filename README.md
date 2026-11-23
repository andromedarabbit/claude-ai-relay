# claude-ai-relay

claude.ai 커넥터(Gmail·Google Calendar·Slack 등)를 **API 키 인증 세션에서도** 쓸 수 있게 해주는 MCP 서버.

Claude Code 세션의 인증이 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`(예: API 게이트웨이·프록시 프로필)일 때 claude.ai 커넥터는 로딩되지 않는다("claude.ai connectors are disabled"). 이 서버는 Claude Code CLI가 쓰는 것과 같은 키체인 OAuth 자격증명으로 Anthropic의 커넥터 프록시 API를 직접 호출해, 어떤 세션에서든 커넥터 도구를 stdio MCP로 노출한다. 별도 자식 프로세스도 훅도 없다.

```text
Claude Code (API 키 세션) ──stdio──▶ claude-ai-relay ──▶ mcp-proxy.anthropic.com ──▶ Gmail/Calendar/Slack/…
```

## 설치

지원 플랫폼은 **Apple Silicon macOS**다. 인증이 macOS 키체인에 의존하므로 `go install`도 Linux 지원을 의미하지 않는다. Intel Mac과 Linux용 바이너리는 배포하지 않는다.

```bash
# Homebrew
brew install andromedarabbit/tap/claude-ai-relay

# 또는 소스에서 설치 (Go 1.22+)
go install github.com/andromedarabbit/claude-ai-relay@latest
```

[GitHub Releases](https://github.com/andromedarabbit/claude-ai-relay/releases)에서 `claude-ai-relay_darwin_arm64.tar.gz`와 `checksums.txt`를 받아 체크섬을 확인한 뒤 압축을 풀어 설치할 수도 있다.

```bash
shasum -a 256 -c --ignore-missing checksums.txt
tar -xzf claude-ai-relay_darwin_arm64.tar.gz
```

체크섬은 파일 손상 확인용이다. 같은 릴리스의 아카이브와 체크섬이 모두 바뀌면 출처를 인증하지 못한다. 바이너리는 Apple 서명·공증을 받지 않아 macOS에서 보안 경고가 표시될 수 있다. 신뢰 여부를 확인하고 macOS의 안내에 따라 직접 허용한다.

## 릴리스 운영

버전 태그를 푸시하면 GitHub Actions가 macOS arm64 아카이브와 체크섬이 담긴 릴리스 **초안**을 만든다. 산출물을 확인한 뒤 GitHub에서 직접 공개한다. 공개 이벤트 후 별도 워크플로가 최신 안정 릴리스의 체크섬을 검증하고 Homebrew tap을 갱신한다. 공개하면 tap 갱신이 자동으로 시작되므로 공개 전에 두 작업의 승인 여부를 확인한다. 사전 릴리스는 Homebrew에 반영하지 않는다. 태그를 올리기 전에 `main.go`의 `version` 상수를 태그와 맞춘다 — serverInfo·clientInfo에 그대로 노출된다.

원본 저장소의 Actions 비밀값 `HOMEBREW_TAP_TOKEN`에는 tap 저장소에만 접근하고 Contents 쓰기 권한을 가진 GitHub App 설치 토큰 또는 세분화 PAT가 필요하다. 릴리스 생성에는 기본 `GITHUB_TOKEN`을 사용한다. 비밀값은 저장소 설정에서 별도 승인 후 등록하고, 만료 또는 유출 시 교체한다.

갱신에 실패하면 Actions의 **Update Homebrew tap** 실행 로그에서 원인을 확인한 후 해당 실행을 재실행하거나 워크플로를 수동 실행한다. 두 경로 모두 최신 공개 안정 릴리스와 체크섬을 다시 확인한다. 동일 버전의 URL·해시가 달라졌거나 기존 formula가 최신 공개 안정 릴리스보다 높은 버전을 가리킨다면 자동으로 덮어쓰지 않고 실패한다. 릴리스의 공개 상태와 기존 formula를 조사한 후 복구한다.

## 등록·사용

```bash
claude mcp add --scope user claude-ai-relay -- claude-ai-relay
```

이후 모든 세션에서 도구 4개가 `mcp__claude-ai-relay__*`로 노출된다:

| 도구 | 설명 |
|---|---|
| `search_tools` | 커넥터 live 도구 검색 — 결과에 읽기/쓰기 분류(`kind`) 포함, `slug.tool` 전체에 부분일치 (`detail: names·descriptions·schemas`) |
| `call_read` | 읽기로 분류된 도구 실행 (예: `gmail.search_threads`) |
| `call_write` | 쓰기로 분류된 도구 실행 — 호스트 승인 대상 |
| `status` | 커넥터 상태·미연결 커넥터 목록. 실패했던 커넥터의 취득을 재시도하고 읽기로 분류된 도구명(`read_tools`)도 싣는다 |

첫 호출 시 macOS 키체인 접근 승인 프롬프트가 한 번 뜬다 (`security` CLI가 `Claude Code-credentials`를 읽는다).

## 커넥터 노출 규칙

claude.ai에 연결된 **모든 커넥터의 모든 live 도구**가 노출된다 (docs/DECISIONS.md D7). 노출 범위 통제는 호스트 Claude Code 권한 설정이 담당한다 — `call_read`는 자동 허용, `call_write`는 매번 확인이 기본 권장 설정이다.

- **커넥터 접두사**: 커넥터 표시명을 규약으로 변환한다 — 소문자화 + 알파벳·숫자 외 문자를 `_` 하나로 (예: `Gmail`→`gmail`, `Google Calendar`→`google_calendar`, `Zoom for Claude`→`zoom_for_claude`). 축약 테이블은 없다. claude.ai 측 표시명이 바뀌면 접두사도 같이 바뀐다.
- **읽기/쓰기 분류**: 도구명 토큰에 쓰기 동사(create·send·delete·update·set·certify·invoke 등)가 하나라도 있으면 쓰기 — 읽기 동사가 섞여 있어도 그렇다 (`certify_query`는 조직 단위 쓰기다). 없으면 읽기 동사 토큰(get·list·search·find·read·describe·download·query·fetch·lookup·discover·whoami)이 하나라도 있을 때 읽기, 아니면 쓰기. 토큰은 정확일치만 맞춘다 (`list_labels`의 `labels`는 `label`이 아니다). 판단이 서지 않으면 쓰기로 편향한다 — `suggest_time`·`export_*`·`token_status` 같은 도구는 쓰기라 `call_write`로 호출해야 하고 호스트 승인을 거친다. 두 목록의 확장은 코드 수정이 필요하다(별도 지시 시).
- **첫 호출과 회복**: 첫 도구 호출이 전 커넥터 도구를 가져온다(실측 약 16초, 커넥터당 최대 10초). 실패한 커넥터는 `status`를 부를 때마다 다시 시도한다 — claude.ai에서 커넥터를 새로 연결했으면 `status` 한 번으로 반영된다.
- **호환성**: 예전 선언 방식의 `gcal.list_events`·`gdrive.*` 접두사는 `google_calendar.list_events`·`google_drive.*`로 바뀌었다. `gmail`·`slack`·`meetings`·`claude_data`·`claude_docs`는 그대로다. 호스트 권한 설정(`mcp__claude-ai-relay__call_read/write`)은 MCP 도구명 단위라 영향 없다.

## 안전 모델

- **전체 노출, 호스트 통제** — 발견된 커넥터의 도구를 가리지 않는다. 통제는 호스트 권한 설정(call_read 자동허용 / call_write 매번 확인)에서 한다. 읽기로 분류된 도구는 승인 없이 실행되므로 그 집합은 `status`의 `read_tools`로 감사한다
- **미연결 커넥터 흡수** — 커넥터별 OAuth가 필요한 도구는 tools/list가 401로 실패해도 전체가 죽지 않고 `status`의 `unconnected`로 표시된다. 접두사(slug)가 겹치는 커넥터는 잘못된 커넥터로 라우팅될 수 있어 relay 전체가 실패한다(fail fast)
- relay는 토큰을 직접 refresh하지 않는다. 만료 시 CLI 갱신을 트리거할 뿐 (자세한 이유는 [docs/MEASURED-FACTS.md](docs/MEASURED-FACTS.md) — refresh 회전이 기존 자격증명을 즉시 폐기하기 때문). 이 CLI 갱신은 `call_read`·`call_write`의 401에만 쓴다 — 전 커넥터 조회 중 미연결 커넥터가 돌려주는 401은 토큰 만료와 상태코드가 같아서, 여기서는 키체인의 토큰이 실제로 바뀌었을 때만 한 번 재시도한다

## 제약

Anthropic의 **비공개 API**(`api.anthropic.com/v1/mcp_servers`, `mcp-proxy.anthropic.com`)를 사용한다 — 사양 공지 없이 바뀔 수 있다. 전송 코드는 한 구획에 고립돼 있고 오류는 종류별로 노출된다. claude.ai 유료 구독 + 커넥터 연결 상태가 전제다.

## 개발

```bash
go build -o claude-ai-relay .   # 의존성 0 (표준 라이브러리만)
./claude-ai-relay selfcheck     # 오프라인 검증
./claude-ai-relay probe         # 온라인 1회 호출 검증
python3 -m unittest discover -s scripts -p 'test_*.py'  # Homebrew 갱신 검증
```

브랜치 푸시와 PR에서 GitHub Actions CI가 Go 1.22·1.25로 macOS arm64 교차 빌드, 오프라인 selfcheck, Homebrew 갱신 테스트를 실행한다. selfcheck는 Linux 러너에서 실행하며, 키체인과 네트워크가 필요한 `probe`는 CI에서 실행하지 않는다.

설계 근거와 실측 기록: [docs/DECISIONS.md](docs/DECISIONS.md) · [docs/MEASURED-FACTS.md](docs/MEASURED-FACTS.md)

## 라이선스

[MIT](LICENSE)
