# claude-ai-relay

claude.ai 커넥터(Gmail·Google Calendar·Slack 등)를 **API 키 인증 세션에서도** 쓸 수 있게 해주는 MCP 서버.

Claude Code 세션의 인증이 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`(예: API 게이트웨이·프록시 프로필)일 때 claude.ai 커넥터는 로딩되지 않는다("claude.ai connectors are disabled"). 이 서버는 Claude Code CLI가 쓰는 것과 같은 키체인 OAuth 자격증명으로 Anthropic의 커넥터 프록시 API를 직접 호출해, 어떤 세션에서든 커넥터 도구를 stdio MCP로 노출한다. 별도 자식 프로세스도 훅도 없다.

```text
Claude Code (API 키 세션) ──stdio──▶ claude-ai-relay ──▶ mcp-proxy.anthropic.com ──▶ Gmail/Calendar/Slack/…
```

## 설치

```bash
# Homebrew
brew install andromedarabbit/tap/claude-ai-relay

# 또는 go install (Go 1.22+)
go install github.com/andromedarabbit/claude-ai-relay@latest

# 또는 GitHub Releases 바이너리 (darwin/linux × amd64/arm64)
# https://github.com/andromedarabbit/claude-ai-relay/releases
```

## 등록·사용

```bash
claude mcp add --scope user claude-ai-relay -- claude-ai-relay
```

이후 모든 세션에서 도구 4개가 `mcp__claude-ai-relay__*`로 노출된다:

| 도구 | 설명 |
|---|---|
| `search_tools` | 선언된 커넥터 도구 검색 (`detail: names·descriptions·schemas`) |
| `call_read` | 읽기로 선언된 도구 실행 (예: `gmail.search_threads`) |
| `call_write` | 쓰기로 선언된 도구 실행 — 제약은 relay가 사전 검증 |
| `status` | 커넥터 상태·선언-실제 불일치·미선언 커넥터 목록 |

첫 호출 시 macOS 키체인 접근 승인 프롬프트가 한 번 뜬다 (`security` CLI가 `Claude Code-credentials`를 읽는다).

## 내장 커넥터 (v0.2)

Gmail(읽기 4·쓰기 1), Google Calendar — 쓰기는 `unlabel_message`(INBOX 아카이빙) 하나뿐이고 허용값이 선언으로 고정된다. 그 외 읽기 전용 10종: Slack, Google Drive, Atlassian Rovo, Zoom, Meetings, Claude Data, Claude Docs, Google Docs/Sheets/Slides.

## 선언 커스터마이징

커넥터 노출은 `connectors/*.json` 선언이 결정한다 (바이너리에 내장). 재빌드 없이 추가·덮어쓰기하려면:

```bash
mkdir -p ~/.config/claude-ai-relay/connectors
cat > ~/.config/claude-ai-relay/connectors/notion.json <<'JSON'
{
  "name": "notion",
  "match": "Notion",
  "read": ["search", "get_page"],
  "write": {}
}
JSON
```

`match`는 claude.ai 커넥터 표시명과 정확히 일치해야 한다 (`status` 도구의 `undeclared` 목록에서 확인). 읽기·쓰기 분류와 쓰기 제약 문법은 [docs/DECISIONS.md](docs/DECISIONS.md) 참고.

## 안전 모델

- **선언된 도구만 노출** — 발견된 17개 커넥터 중 선언 없는 것은 `status`에만 표시된다
- 쓰기는 선언에 `fixed`(허용값 고정)·`types`(타입 검사) 제약을 명시해야 하고, relay가 호출 전에 검증한다
- 커넥터별 OAuth가 필요한 도구는 401로 구분된다 (`status`의 미선언 목록 = 미연결 커넥터)
- relay는 토큰을 직접 refresh하지 않는다. 만료 시 CLI 갱신을 트리거할 뿐 (자세한 이유는 [docs/MEASURED-FACTS.md](docs/MEASURED-FACTS.md) — refresh 회전이 기존 자격증명을 즉시 폐기하기 때문)

## 제약

Anthropic의 **비공개 API**(`api.anthropic.com/v1/mcp_servers`, `mcp-proxy.anthropic.com`)를 사용한다 — 사양 공지 없이 바뀔 수 있다. 전송 코드는 한 구획에 고립돼 있고 오류는 종류별로 노출된다. claude.ai 유료 구독 + 커넥터 연결 상태가 전제다.

## 개발

```bash
go build -o claude-ai-relay .   # 의존성 0 (표준 라이브러리만)
./claude-ai-relay selfcheck     # 오프라인 검증
./claude-ai-relay probe         # 온라인 1회 호출 검증
```

설계 근거와 실측 기록: [docs/DECISIONS.md](docs/DECISIONS.md) · [docs/MEASURED-FACTS.md](docs/MEASURED-FACTS.md)

## 라이선스

[MIT](LICENSE)
