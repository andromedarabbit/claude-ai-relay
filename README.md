# claude-ai-relay

claude.ai 커넥터(Gmail 등)를 API 인증 세션에서도 쓸 수 있게 해주는 MCP 서버.

세션 인증이 `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`(예: ccs 프로필)일 때 claude.ai 커넥터는 로딩되지 않는다("claude.ai connectors are disabled"). 이 프로젝트는 인증 env를 지운 `claude -p` 자식 프로세스에서 `mcp_tool` Stop 훅으로 커넥터 도구를 직접 호출하는 **relay 메커니즘**을 stdio MCP 서버로 감싸, 어떤 세션에서든 커넥터 도구를 상시 노출시킨다.

## 상태

**설계 전.** 구현은 아직 없다. 다음 세션에서 상세 설계를 진행한다 — 첫 주제는 [docs/DESIGN-QUESTIONS.md](docs/DESIGN-QUESTIONS.md)의 주제 #1(Gmail 전용 vs 범용 커넥터 지원).

## 문서 지도

| 문서 | 내용 |
|---|---|
| [docs/CONTEXT.md](docs/CONTEXT.md) | 문제 정의와 relay 메커니즘 배경 |
| [docs/RESEARCH.md](docs/RESEARCH.md) | OSS 조사(동일 컨셉 부재 확인)와 근거 |
| [docs/DESIGN-QUESTIONS.md](docs/DESIGN-QUESTIONS.md) | 설계 주제 목록 (첫 주제: Gmail 전용 vs 범용) |
| [docs/MEASURED-FACTS.md](docs/MEASURED-FACTS.md) | gmail-triage에서 검증된 실측 수치 |
| [docs/KICKOFF.md](docs/KICKOFF.md) | 상세 설계 세션 시작 프롬프트 |
| [docs/reference/connector_relay.py](docs/reference/connector_relay.py) | relay 원형 구현 스냅샷 (gmail-triage에서 복사, 참고용 — 수정 대상 아님) |
