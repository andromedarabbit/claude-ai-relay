# RESEARCH — OSS 조사 (2026-11-15)

"claude.ai 커넥터 relay를 독립 MCP로 래핑"하는 프로젝트는 조사 시점 기준 **존재하지 않는다**. 다만 이 개념을 이루는 부품은 모두 공식·알려진 것들이고, 근방 개념의 프로젝트들은 있다.

## 근방 프로젝트

| 프로젝트 | 컨셉 | 차이 |
|---|---|---|
| [steipete/claude-code-mcp](https://github.com/steipete/claude-code-mcp) | Claude Code 에이전트 전체를 MCP 서버로 감싸기 | 커넥터 relay가 아니라 에이전트-as-도구. 인증 env 제거 없음 |
| [dyshay/proxyclawd](https://github.com/dyshay/proxyclawd) | Claude Code ↔ Anthropic API 트래픽 MITM 프록시 | API 트래픽 가로채기지, 커넥터 우회가 아님 |
| [authsome-mcp-proxy](https://github.com/stephaneberle9/authsome-mcp-proxy) | 로컬 stdio MCP를 claude.ai 웹 커넥터로 노출 | **정반대 방향** — 로컬 MCP → claude.ai 커넥터 |
| Gmail API 직결 MCP 서버 다수 (예: `gongrzhe/server-gmail-autoauth-mcp`) | 자체 Google OAuth로 Gmail API 직접 호출 | claude.ai를 안 거친다 — OAuth 관리가 사용자 몫 |

## 왜 없을까

- 전제 조건이 좁다: 머신에 claude.ai 구독 로그인 + API env 세션 + 커넥터로 OAuth 위임.
- 진입장벽: Stop 훅 + `--settings` 주입 조합을 발견해야 한다. gmail-triage가 실측으로 발견한 조합.

## 이 프로젝트의 유일한 실익과 대안

relay 컨셉의 실익은 **Google OAuth 자격증명을 claude.ai 구독에 이미 위임돼 있다는 점을 재활용**하는 것 — OAuth 앱 등록·토큰 관리를 하지 않아도 된다. "Gmail 도구를 인증 불문 상시 노출"이 유일한 목적이면 Gmail API 직결 MCP가 생태계 표준이고(콜당 haiku 턴·커넥터 연결 대기 없음, claude.ai 의존 없음), 판단 기준은 "OAuth 관리를 내 손에 둘지 claude.ai에 맡길지"다.

## 수명 한계

- `mcp_tool` 훅 타입은 공식 문서화된 1급 기능 (Claude Code changelog: "Hooks can now invoke MCP tools directly via type: `mcp_tool`").
- 커넥터 비활성화 동작은 upstream이 인지한 문제 (#108528, #58127)라 언제든 바뀔 수 있다. **API env 세션에서 커넥터가 허용되는 날이 오면 이 프로젝트는 통째로 불필요해진다.**
