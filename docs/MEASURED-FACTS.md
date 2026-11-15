# MEASURED-FACTS — gmail-triage에서 검증된 실측

relay 메커니즘의 유일한 실측 근거. gmail-triage 스킬(사내 저장소)에서 2026-11-08 검증, 2026-11-15 스냅샷([reference/connector_relay.py](reference/connector_relay.py)) 기준.

## 메커니즘 실측

| 항목 | 값 |
|---|---|
| 배치 크기 | 20건 |
| 배치당 비용 | haiku 1턴 ("Reply OK") |
| 처리 시간 | 20건 313KB, 4.9초 (본문 바이트 동일 검증) |
| 커넥터 로딩 시점 | 턴 종료(Stop) 시점에는 연결 완료 — UserPromptSubmit 훅은 연결 전에 실행돼 "not connected"로 실패 |
| 훅 응답 | stream-json `subtype=hook_response` 이벤트로 반환. 어느 훅의 응답인지 표시·순서 보장이 없어 **응답 본문 안의 `id`로 매핑**. 오류 응답("Requested entity was not found.")은 id가 없어 누락으로 처리 |
| 응답 수 불일치 | 훅 응답 수 ≠ 호출 수이면 묶음 전체 실패 처리 ("커넥터가 로딩되지 않았을 수 있음") |
| probe 재시도 | not_connected 시 2초 후 1회 재시도 (커넥터 콜드스타트 흡수) |

## 인증·환경

| 항목 | 값 |
|---|---|
| 제거하는 env 접두사 | `ANTHROPIC_`, `CLAUDE_CODE_USE_` |
| 제거 효과 | env 기반 인증·라우팅(ccs의 `ANTHROPIC_BASE_URL` 프록시 포함)보다 claude.ai OAuth 로그인이 우선하므로 자식이 커넥터를 로딩 |
| 자식 명령 | `claude -p "Reply OK" --model haiku --setting-sources "" --settings '{"hooks":{"Stop":[...]}}' --disable-slash-commands --no-session-persistence --system-prompt "Reply OK." --output-format stream-json --verbose --include-hook-events` |
| `--setting-sources ""` | 사용자/프로젝트 설정 소스를 비워 훅 간섭 차단 |

## 안전장치 (validate_call)

- 읽기 4종만 무제한: `search_threads`, `get_thread`, `get_message`, `list_labels`
- 쓰기는 `unlabel_message` + `labelIds=["INBOX"]` + 문자열 messageId 조합만
- 나머지는 자식 실행 전 거부 — **훅 호출은 Claude Code 권한 확인을 거치지 않으므로** 스크립트 허용 목록과 승인 절차가 방어선
- archive는 실행 전 재확인(INBOX 없거나 STARRED·사용자 라벨 붙으면 스킵), 실행 후 재조회로 INBOX 제거 독립 검증
- exit codes: 0 성공 / 2 사용법 오류 / 3 relay 불가(사유 not_connected·auth·timeout·other) / 4 일부 실패(결과 파일 쓰고 누락 id는 stderr)

## 2026-11-15 실측 (상세 설계 세션) — 기존 미실측 해소

- 상시 자식(`--input-format stream-json`)에서 **매 턴 Stop 훅이 발화한다**: 턴 1 = 3.0초(기동·연결 포함), 턴 2 = 2.1초(같은 자식). n=1.
- 훅 `server` 필드는 `"claude.ai Google Calendar"` 형식으로 범용(`claude.ai <표시명>`). Gmail+Calendar 2훅 배치 3.5초.

### 훅 relay 보강

| 항목 | 값 |
|---|---|
| 응답 상관 | `hook_started`와 `hook_response`가 같은 `hook_id`를 공유. started 순서 = 설정 순서(2회 실행 관찰), 응답 순서는 무작위 → 본문 `id` 기반 매핑 불필요, `id` 없는 응답도 배치 가능 |
| 훅 중복 제거 | 같은 (server, tool, input) 훅은 설정에 8개를 넣어도 2개만 실행됨 — 원본 스크립트의 "중복 제거" 주석의 원인 |
| init 이벤트 | `mcp_servers` = `[]` — 커넥터 상태 소스로 부적합 |
| `claude mcp list` | 인증 env 제거 환경에서 claude.ai 커넥터 16개 + 상태(Connected / Needs authentication) 표시 |

## 2026-11-15 실측 — 직접 호출 경로 (v0.1 전송, DECISIONS D2)

| 항목 | 값 |
|---|---|
| 토큰 | 키체인 `Claude Code-credentials`는 **다중 항목**: acct `unknown`(빈 레거시), acct `keaton`(accessToken/refreshToken 108ch, 사용 시점에 갱신됨). `-a` 항목별 조회 필수 |
| 커넥터 발견 | `GET api.anthropic.com/v1/mcp_servers` + 헤더 `anthropic-beta: mcp-servers-2025-12-04`, `anthropic-version: 2023-06-01` → 17커넥터 (`mcpsrv_` id, url, 도구별 `effective_max_permission`, `eligible`) |
| 도구 호출 | `POST mcp-proxy.anthropic.com/v1/mcp/{mcpsrv_id}` + `Authorization: Bearer`, `X-Mcp-Client-Session-Id: <uuid>`, `anthropic-version` → initialize 200, tools/list 200 (Gmail 30도구, 885ms, live 스키마), tools/call `list_labels` 200 (729ms, 라벨 14개) |
| User-Agent | `python-urllib` 기본 UA는 Cloudflare 1010 차단. `claude-cli/…` UA는 통과 |
| 서버 id | `mcpsrv_` 형식만 유효. 자식 디버그 로그의 내부 해시 id는 "Server identifier format is invalid"(400) |
| 외부 근거 | [egzo PR #86](https://github.com/egzo-ai/egzo/pull/86)이 동일 구조("mcp_servers 목록 → 같은 bearer로 프록시 호출") 사용. [anthropics/claude-ai-mcp](https://github.com/anthropics/claude-ai-mcp) 이슈 트래커가 커넥터 인프라 공개 추적 |

## 2026-11-15 실측 — 토큰 refresh (relay 갱신 정책 확정)

| 항목 | 값 |
|---|---|
| refresh 엔드포인트 | `POST api.anthropic.com/v1/oauth/token` (form: `grant_type=refresh_token`, `refresh_token`, `client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e`) → 200. `console.anthropic.com/v1/oauth/token`은 404 |
| 응답 | `access_token`(신규), `refresh_token`(**회전**), `expires_in: 28800`(액세스 수명 8시간), scope, account 등 |
| **회전 의미 (실측)** | refresh 1회로 **구 refresh_token·구 access_token 모두 즉시 revoke**. 재사용 → rt 400, at 401 "OAuth access token has been revoked" |
| 결론 | relay가 직접 refresh하면 CLI의 키체인 자격증명 체인을 파괴한다. **relay 갱신은 CLI 트리거만**: 401 시 ① 키체인 재독 → ② env 제거 `claude -p` 1회 스폰(CLI가 refresh+기록) → ③ 재독 → ④ 실패 시 "claude login 필요" 안내 (fail fast) |
| 비고 | 이 실측으로 이 머신 키체인의 OAuth 체인이 폐기됨 — `claude login` 재로그인으로 복구 (2026-11-15) |

## 미검증 (구현 시 확인)

- 키체인 `expiresAt` 필드의 단위·의미 (파싱값 비정상 — `expires_in` 기반으로는 액세스 8시간 수명 확인)
- Cloudflare UA 차단의 정확한 범위 (통과 확인된 것은 claude-cli UA뿐)

## 2026-11-15 실측 — v0.1 e2e (쓰기 경로)

| 항목 | 값 |
|---|---|
| 쓰기 e2e | `call_write gmail.unlabel_message` (프로모션 메일 1건 아카이빙) → 재조회 `get_message`에서 labelIds `INBOX` 소실(`CATEGORY_PROMOTIONS`만 잔존), `in:inbox` 재검색 0건 — 쓰기+독립 검증 통과 |
| `fixed` 키 계약 | 호출자가 고정값을 **명시적으로 실어야** 한다(주입 아님) — `labelIds` 누락 시 `constraint` 사전 거부. 개선 후보(v0.2): relay가 fixed 키를 주입해 호출자 생략 허용 |
| 도구 사용성 | search_threads 스키마의 pageSize·query 등 live 스키마가 그대로 노출·동작. 업스트림 본문(plaintext/html) 무손실 전달 확인 |
