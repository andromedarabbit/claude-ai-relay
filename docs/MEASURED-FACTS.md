# MEASURED-FACTS — gmail-triage에서 검증된 실측

relay 메커니즘의 유일한 실측 근거. gmail-triage 스킬(git.baemin.in/dataplatform/oh-my-skills)에서 2026-11-08 검증, 2026-11-15 스냅샷([reference/connector_relay.py](reference/connector_relay.py)) 기준.

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

## 미실측 (다음 세션에서 검증 대상)

- `--input-format stream-json`으로 자식을 상시 유지할 때 **매 턴 Stop 훅이 발화하는지** (문서 근거만 있음)
- Gmail 외 커넥터의 훅 `server` 필드 형식 (`"claude.ai Gmail"`이 Gmail 전용 형식)
