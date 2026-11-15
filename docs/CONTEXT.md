# CONTEXT — 문제 정의와 relay 메커니즘

## 문제

Claude Code 세션의 인증이 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`(예: ccs 프로필, 로컬 프록시 게이트웨이)일 때, claude.ai 커넥터(Gmail, Google Calendar 등)가 **아예 로딩되지 않는다**. 세션 시작 시 "claude.ai connectors are disabled because ANTHROPIC_API_KEY or another auth source is set and takes precedence over your claude.ai login" 경고가 뜨고 `mcp__claude_ai_*` 도구가 세션에 존재하지 않는다.

이 동작은 의도된 것으로, upstream이 알고 있는 문제다:

- claude-code 이슈 [#108528](https://github.com/anthropics/claude-code/issues/108528) — 경고가 실제 auth 소스를 식별하지 못함
- claude-code 이슈 [#58127](https://github.com/anthropics/claude-code/issues/58127) — 문서에 이 제약이 누락됨 (changelog v2.1.139: "Remote Control, /schedule, claude.ai MCP connectors ... are now disabled when ANTHROPIC_API_KEY / apiKeyHelper / ANTHROPIC_AUTH_TOKEN is set")

## 해결 아이디어: relay

MCP 서버는 Claude Code가 로컬로 띄우는 프로세스라 **세션 인증 게이트를 통과하지 않는다**. 그래서:

1. 인증 env(`ANTHROPIC_*`, `CLAUDE_CODE_USE_*`)를 지운 자식 `claude -p` 프로세스를 띄우면, 자식은 claude.ai OAuth 로그인 자격증명으로 fallback 인증하고 커넥터가 로딩된다.
2. 호출하고 싶은 커넥터 도구 호출을 `mcp_tool` 타입 Stop 훅으로 자식에게 주입한다 — 자식 모델은 "Reply OK" 한 마디만 답하고, 턴이 끝나는 Stop 시점에 훅이 커넥터 도구를 설정에 박힌 인자 그대로 실행한다. 모델은 인자를 건드리지 않는다.
3. 결과는 stream-json의 `hook_response.output`으로 돌아온다.

이 메커니즘은 gmail-triage 스킬에서 만들어 2026-11-08 실측 검증됐다 (스냅샷: [reference/connector_relay.py](reference/connector_relay.py)).

## 이 프로젝트: relay를 상시 노출

gmail-triage는 relay를 스킬 전용 스크립트로만 쓴다. 이 프로젝트는 같은 메커니즘을 **stdio MCP 서버로 래핑**해 모든 세션·스킬에서 쓸 수 있게 한다:

```text
세션(인증 불문) ──mcp__claude-ai-relay__*──▶ relay MCP 서버 (stdio, 로컬)
                                                │ 내부: 인증 env 제거 + claude -p 자식
                                                │       + mcp_tool Stop 훅으로 커넥터 호출
                                                ▼
                                      claude.ai OAuth → 커넥터(Gmail 등) → 응답
```

부수 이득: gmail-triage의 relay는 훅 호출이 Claude Code 권한 확인을 거치지 않는 약점이 있었다. MCP 서버로 감싸면 도구 호출이 다시 MCP 권한 체계(allow-list·프롬프트)를 타므로, 스킬 밖에서도 안전하게 상시 노출할 수 있다.

## 전제

- 머신에 claude.ai OAuth 로그인이 있어야 한다 (자식이 fallback 인증으로 사용).
- 콜당(또는 배치당) haiku 턴 비용이 미량 발생한다 ("Reply OK" 한 마디).
- upstream이 API env 세션에서도 커넥터를 허용하도록 바뀌면 이 프로젝트는 통째로 불필요해진다.
