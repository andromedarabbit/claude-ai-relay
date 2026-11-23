# DECISIONS — 상세 설계 결정 기록

결정마다 근거를 [MEASURED-FACTS.md](MEASURED-FACTS.md)·[RESEARCH.md](RESEARCH.md)에 남긴다.
코드 작성은 이 파일에 "구현 시작" 항목이 기록된 뒤에만 한다.

## 주제 #1: Gmail 전용 vs 범용 커넥터 지원 (2026-11-15 결정)

### D1. 인터페이스는 범용, v0.1 출하는 Gmail 선언만

> **2026-11-23 정정 (D7)**: 선언 구조·쓰기 제약·"커넥터 추가 = 선언 추가" 방식은 superseded — 아래 D7 참조. 발견 API 균일 구조라는 근거는 D7에서도 그대로 유효하다.

- 도구 표면·선언 구조는 커넥터 전체를 대상으로 설계한다. 커넥터 추가는 선언(데이터) 추가로 끝난다.
- v0.1 선언은 **Gmail·Google Calendar 2건**. 범용 인터페이스 검증은 커넥터 2개 이상이어야 실제로 가능하다 (2026-11-15 사용자 결정).
  - Gmail: 읽기 4종(`search_threads`, `get_thread`, `get_message`, `list_labels`) + 쓰기 `unlabel_message`(`labelIds=["INBOX"]` + 문자열 `messageId` 제약) — gmail-triage 검증면 그대로.
  - Calendar: 읽기 4종(`list_calendars`, `list_events`, `get_event`, `search_events`), 쓰기 없음. 쓰기 도구(`create_event` 등)와 `suggest_time`은 후속 선언 추가로 확장 — 훅 `server` 형식·직접 호출 모두 이 커넥터에서 실측 완료.
- 근거: 커넥터 발견 API(`GET api.anthropic.com/v1/mcp_servers`)가 17개 커넥터를 균일한 구조(`mcpsrv_` id, 도구 목록, `effective_max_permission`)로 반환한다. 범용성은 가설이 아니라 API 스키마가 보장한다.

### D2. 전송 계층: 직접 호출로 피벗, 훅 relay는 폴백으로 문서화만

- 자식 `claude -p` + Stop 훅 대신, relay MCP 서버가 `mcp-proxy.anthropic.com/v1/mcp/{mcpsrv_id}`에 직접 JSON-RPC로 호출한다.
- 근거(실측, 2026-11-15):
  - 지연: 호출당 0.7~0.9초 vs 훅 배치당 3~5초(상시 자식도 턴당 ~2초 하한)
  - 비용: 모델 턴 0 (훅은 배치당 haiku 1턴)
  - 스키마: `tools/list`로 살아 있는 도구 스키마 수신 — 스냅샷 선언·갱신 문제 소멸
  - 배치: 20건 제한·중복 제거 제약 없음 (JSON-RPC id 상관)
- 폴백: 훅 relay는 공개 문서 기능 위의 경로로 유효하다. 이중 구현은 하지 않고 [reference/connector_relay.py](reference/connector_relay.py)를 참조로 남긴다.
- 리스크: 발견 API와 프록시는 비공개 문서다(공식 CLI와 동일 경로, egzo PR #86이 동일 구조 사용·검증). 수명 리스크는 훅 경로의 "auth 게이트 동작 변경" 리스크와 크기는 비슷하고 성격만 다르다.
- 구현 제약(실측에서 확인된 호출 규격):
  - 토큰: 키체인 `Claude Code-credentials`는 **다중 항목**이다. `acct`별 조회로 accessToken이 있는 항목을 골라야 한다(기본 조회는 빈 레거시 항목을 잡는다).
  - 헤더: `Authorization: Bearer`, `X-Mcp-Client-Session-Id: <uuid>`, `anthropic-version: 2023-06-01`. 발견 API에는 추가로 `anthropic-beta: mcp-servers-2025-12-04`.
  - User-Agent: `python-urllib` 기본 UA는 Cloudflare 1010으로 차단된다. 정상 UA 필수.
  - 서버 id는 `mcpsrv_` 형식만 유효 (자식 디버그 로그의 내부 해시 id는 400).
  - **토큰 갱신은 CLI 트리거만** (2026-11-15 실측: 직접 refresh는 회전으로 구 at·rt를 즉시 revoke해 CLI 자격증명 체인을 파괴한다). 401 시 키체인 재독 → env 제거 `claude -p` 스폰 → 재독 → 실패 시 안내.

### D3. 도구 표면 4개 (커넥터 수와 무관하게 고정)

> **2026-11-23 정정 (D7)**: read/write 분류가 선언 기반에서 이름 휴리스틱으로 바뀌고 WriteConstraint 인자 제약 검증은 삭제됐다. 4개 표면과 호스트 권한 구분(call_read 자동허용 / call_write 매번 확인)은 유지.

| 도구 | 역할 | 호스트 권한 권장 |
|---|---|---|
| `search_tools(query, detail)` | 선언된 도구 검색. detail: 이름만 / 이름+설명 / 전체 스키마 | 자동 허용 |
| `call_read(tool, args)` | 읽기 선언만 실행. 선언 스키마로 인자 검증 | 자동 허용 |
| `call_write(tool, args)` | 쓰기 선언만 실행. 인자 제약까지 검증 | 매번 확인 |
| `status()` | 커넥터 상태·선언 대조 (D4) | 자동 허용 |

| 도구 | 역할 | 호스트 권한 권장 |
|---|---|---|
| `search_tools(query, detail)` | 선언된 도구 검색. detail: 이름만 / 이름+설명 / 전체 스키마 | 자동 허용 |
| `call_read(tool, args)` | 읽기 선언만 실행. 선언 스키마로 인자 검증 | 자동 허용 |
| `call_write(tool, args)` | 쓰기 선언만 실행. 인자 제약까지 검증 | 매번 확인 |
| `status()` | 커넥터 상태·선언 대조 (D4) | 자동 허용 |

- 도구명에 `relay_` 접두사를 붙이지 않는다 (2026-11-15 사용자 결정) — MCP가 `mcp__<서버별명>__<도구>`로 자동 네임스페이싱하므로 중복이다.

- 근거: 커넥터 전체 합산 도구가 100개를 넘으므로 동적 검색은 v0.1부터 필요(리서치: 도구 30~50개 초과시 선택 정확도 저하, 검색 메타 도구 + 지연 로딩이 업계 표준). 범용 passthrough 단일 도구는 권한의 읽기/쓰기 구분이 사라져 기각(Cloudflare Code Mode와 같은 사유). 호출 도구를 읽기/쓰기 둘로 쪼개 호스트 권한 체계에 구분을 남긴다.

### D4. `status` 소스 = 커넥터 발견 API

> **2026-11-23 정정 (D7)**: "미선언 커넥터 목록"은 unconnected(도구 취득 실패·미연결 커넥터)로 대체됐다.

- `mcp_servers` API가 목록·상태·도구·권한을 1회 호출로 준다. init 이벤트의 `mcp_servers`는 빈 배열로 실측(부적합), `claude mcp list`는 CLI 기동 비용이 있다.

- `mcp_servers` API가 목록·상태·도구·권한을 1회 호출로 준다. init 이벤트의 `mcp_servers`는 빈 배열로 실측(부적합), `claude mcp list`는 CLI 기동 비용이 있다.
- 표시 내용: 선언된 커넥터 상태, relay 자체 건강(토큰 유효성), 발견됐지만 선언에 없는 커넥터(미노출 목록).

### D5. 도구 스키마는 live 동기화

> **2026-11-23 정정 (D7)**: 선언-live mismatches 경보는 폐지 — 스키마는 live가 유일 원천이고 검증은 호출 시 validateArgs가 한다.

- 선언에 스키마를 박지 않는다. 기동·주기적으로 `tools/list`와 대조해 선언의 읽기/쓰기·제약과 업스트림 스키마가 어긋나면 알린다.

- 선언에 스키마를 박지 않는다. 기동·주기적으로 `tools/list`와 대조해 선언의 읽기/쓰기·제약과 업스트림 스키마가 어긋나면 알린다.

### D6. code-mode 계층 — 도입 안 함

- v0.1 이후 기본 계획에도 넣지 않는다 (2026-11-15 사용자 결정). 필요성이 재검토되면 in-process 임베디드 관점(별도 샌드박스 인프라 불필요)에서만 본다.

### D7. 선언 허용목록 제거 — 발견 전체 노출 + 이름 휴리스틱 분류 (2026-11-23, session-settled: user-directed)

- 발견 API가 돌려준 모든 커넥터의 모든 live 도구를 기본 노출한다. connectors/*.json 선언 시스템(D1의 선언 구조, D5의 대조)과 쓰기 제약(WriteConstraint)은 삭제한다.
- 사유: 노출 범위 통제는 이미 호스트 Claude Code 권한 설정(call_read 자동허용 / call_write 매번 확인)에서 가능하므로 relay 측 허용목록은 이중 관리다. 별도 필터링 메커니즘은 만들지 않는다 — 특별한 지시가 있을 때까지 불필요 (사용자 결정).
- 읽기/쓰기 분리(D3)는 유지한다. 선언이 없으므로 이름 컨벤션 휴리스틱으로 분류하고, 판단이 서지 않으면 쓰기로 편향한다(호스트 승인을 거치는 안전한 쪽).
  - 커넥터 slug 규약: DisplayName 소문자화 + 비알파벳 문자 → `_`. 축약 테이블 없음. 규칙이 기존 선언 name과 우연히 일치해도 규칙이 우선.
  - 읽기 동사 토큰 목록: get·list·search·find·read·describe·download·query·fetch·lookup·discover·whoami. 토큰 정확일치만(부분문자열 금지 — update_target 함정). suggest·export·summarize·follow·status·info는 읽기 아님 → 쓰기. 목록 확장은 별도 지시 시 코드 수정.
- 커넥터별 tools/list 실패(미연결 401 등)는 전체를 죽이지 않고 status의 unconnected로 표시한다. fail fast는 토큰·발견 등 전역 실패만.
- 기각된 대안: 선언을 유지하되 기본 전체 허용 + 선언이 override하는 방식 — 선언 파일·오버레이 스캔·불일치 경보(D5)가 모두 살아남아 복잡도가 그대로 남고, 통제는 호스트에서 가능하므로 남길 이유가 없다.
- 호환성: slug 규약 적용으로 gcal→google_calendar, gdrive→google_drive 등 접두사가 바뀐다. gmail·slack·meetings·claude_data·claude_docs는 유지. 호스트 권한 설정은 MCP 도구명 단위라 불영향.
- 정정: D1(선언 구조·쓰기 제약), D3(선언 기반 read/write·인자 제약 검증), D4(미선언 목록 표시), D5(선언-live 대조) — 선언 관련 부분만 superseded. D2(전송)·D6(code-mode 기각)은 그대로 유효.

## 주제 재배치

| 주제 | 상태 |
|---|---|
| #2 자식 프로세스 모델 | **소멸** — D2로 자식 프로세스가 없다. 상시 자식 실측(매 턴 Stop 훅 발화, 턴 2 = 2.1초)은 MEASURED-FACTS에 기록만 남긴다. |
| #3 권한 모델 | 원칙 확정(D3 표 + D1 제약). 세부(제약 선언 문법, `effective_max_permission` 반영 여부)는 구현 단계에서 확정. |
| #4 패키징·배포 | **Go 확정** (2026-11-15, session-settled: user-directed — 개인 프로젝트·GitHub 공개·배포 용이성 최우선. uv 싱글파일(uv 런타임 전제)·Rust(반복 마찰) 대비 선택). Go 표준 라이브러리만, goreleaser로 GitHub Releases 교차 컴파일, 등록은 사용자 스코프 `claude mcp add` 한 줄. |

## 구현 로드맵 (승인 후 착수)

1. **전송 계층** — 키체인 다중 항목 조회, **토큰 refresh 흐름 실측(최우선 — 만료가 전체를 게이트한다)**, JSON-RPC 래퍼(UA·헤더 포함). 종료 기준: `list_labels` 직접 호출 200.
2. **선언 로더 + Gmail·Calendar 선언 + live 스키마 대조**(D5). 종료 기준: 선언-실제 불일치 감지, **커넥터 2개 혼합 호출** 동작.
3. **도구 표면 4개**(D3). 종료 기준: relay 경유 읽기·쓰기 제약 동작.
4. **relay_status**(D4). 종료 기준: 미선언 커넥터 표시.
5. **패키징·등록**(주제 #4 결정 반영). 종료 기준: `claude mcp add` 후 타 세션에서 도구 4개 노출.

## 구현 시작

2026-11-15 승인. 범위: v0.1 단위 1~5(전송·선언·도구 표면·status·패키징), 언어 Go, 계획 파일은 세션 플랜(`~/.claude/plans/linked-tinkering-turtle.md`).

## GitHub Actions CI 구현 시작 (2026-11-16)

승인 범위: 브랜치 푸시·PR의 오프라인 빌드 검증과 기존 워크플로를 이용한 v0.2.1 릴리스 초안 확인. 태그 푸시·공개·tap 쓰기는 각각 별도 승인 후 진행한다.

## v0.1 완료 (2026-11-15)

단위 1~5 전부 종료 기준 충족 + e2e 시나리오 통과(전송 probe 200, 2커넥터 혼합, 읽기 체인, **쓰기+독립 검증**(아카이빙 재조회), 음성 4종 사전 거부, status 2/17/15, **API env 세션에서 `mcp__claude-ai-relay__*` 실사용**). 산출: `main.go`(Go stdlib만) + `connectors/*.json`(embed) + `.goreleaser.yaml`, 사용자 스코프 등록. 후보: GitHub 공개(module 경로·go install·brew tap), fixed 키 주입(v0.2).

## v0.2 (2026-11-15)

- **커넥터 10종 추가** (읽기 전용): Slack·Google Drive·Atlassian Rovo·Zoom·Meetings·Claude Data·Claude Docs·Google Docs/Sheets/Slides — 쓰기는 선언 없음(미노출). Zapier·Sentry·Miro·Granola·Figma는 미연결(401)이라 제외
- **fixed 주입**: 쓰기 제약의 fixed 키를 호출자가 생략하면 relay가 주입한다 (위반값은 기존대로 거부)
- **선언 스캔**: `~/.config/claude-ai-relay/connectors/*.json` 오버레이로 재빌드 없이 선언 추가·덮어쓰기 (fail fast 유지)
- **프록시 세션 수립**(전송 수정): 커넥터별 initialize 후 세션 uuid 재사용 — stateful 업스트림(Slack 등) 지원. 실측 근거는 MEASURED-FACTS v0.2
- 모듈 경로 `github.com/andromedarabbit/claude-ai-relay` 확정 (go install)

## 도구 표면 재설계 구현 시작 (2026-11-23)

승인 범위: D7 — 선언 허용목록 제거, 발견된 전체 커넥터·live 도구 노출, 이름 컨벤션 read/write 분류(미확실→쓰기), 커넥터 slug 규약, status·probe·selfcheck 재작성, `connectors/` 삭제, README·CLAUDE.md 정정. 릴리스·버전업은 별도 승인. 계획 파일: `~/.claude/plans/connector-deep-ember.md`
