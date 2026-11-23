# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 프로젝트

claude.ai 커넥터(Gmail 등)를 API 인증(`ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`) 세션에서도 쓸 수 있게 하는 stdio MCP 서버. 세션 인증 게이트는 로컬 MCP 프로세스를 막지 않는다는 점을 이용해, 키체인의 claude.ai OAuth 토큰으로 커넥터 프록시에 직접 JSON-RPC를 보낸다. 배경·문제 정의는 `docs/CONTEXT.md`, 결정과 근거는 `docs/DECISIONS.md`(D1~D8 번호로 코드 주석에서 참조됨).

## 명령어

```bash
go build .                  # 빌드 (바이너리명 claude-ai-relay)
go run . selfcheck          # 오프라인 assert 검증 — 네트워크·키체인 불필요, 코드 변경 후 항상 실행
go run . probe              # e2e: 키체인 토큰 + 네트워크 필요. 발견→전 커넥터 live 취득→분류→읽기 호출 1건
go run .                    # 무인자 = stdio MCP 서버 실행 (claude mcp add 용도)
goreleaser release --snapshot --clean   # 로컬 스냅샷 빌드 (릴리스 공개 없음)
python3 -m unittest discover -s scripts -p 'test_*.py'  # Homebrew 갱신 검증
```

go test 파일은 없다 — `selfcheck`가 assert 기반 검증이고, 네트워크·키체인이 필요한 검증은 `probe`가 담당한다.

## 아키텍처 (main.go 단일 파일)

호출 흐름: stdio JSON-RPC 루프 → `bootstrap()`(첫 tools/call 때 1회) → 키체인 토큰 → 발견 API → 전 커넥터 live 도구 취득 → `call_read`/`call_write`는 프록시로 tools/call.

- **키체인 토큰** (`loadToken`): `security dump-keychain`으로 `Claude Code-credentials` 서비스 항목을 열거한다. 같은 서비스에 항목이 여러 개(빈 레거시 항목 포함)이므로 accessToken이 있는 항목을 골라야 한다 — 기본 `find-generic-password` 조회는 빈 레거시 항목을 잡는다.
- **토큰 갱신은 CLI 트리거만** (`refreshViaCLI`): 직접 refresh는 회전으로 구 at·rt를 즉시 revoke해 CLI 자격증명 체인을 파괴한다(2026-11-15 실측). 401 사다리는 `ensureToken`: 키체인 재독 → env 제거한 `claude -p` 스폰 → 재독. 이 규칙은 절대 우회하지 않는다.
- **호출 규격 상수** (main.go 상단): UA(`claude-cli/2.1.0 ...`), 헤더, URL. Cloudflare 1010 차단 시 UA 상수만 갱신하면 된다. 근거는 `docs/MEASURED-FACTS.md` — 상수를 바꿀 때 실측 문서도 함께 갱신한다.
- **전체 노출 + 이름 규약** (D7·D8): 커넥터 선언·필터링은 없다 — 발견된 커넥터의 모든 live 도구를 노출하고, 통제는 호스트 권한 설정이 담당한다. 커넥터 접두사는 `slugify`(DisplayName 소문자화 + 비알파벳 → `_`), 읽기/쓰기 분류는 `classifyTool`이 결정한다 — 쓰기 동사 토큰이 하나라도 있으면 쓰기(읽기 동사가 섞여 있어도), 아니면 읽기 동사 토큰 정확일치, 미확실 → 쓰기. 읽기는 호스트 승인 없이 실행되므로 새 읽기 동사를 넣을 때는 live 도구명을 설명까지 대조한다(`probe`가 읽기 분류 도구명을 찍는다).
- **편입 규칙은 `addServer` 하나** (D8): bootstrap·status·probe가 모두 이 함수로 커넥터를 state에 넣는다 — 커넥터별 실패는 `unconnected`로 흡수, slug 충돌만 fail fast(사용자 결정), 커넥터당 `sweepTimeout`. bootstrap 결과는 프로세스 수명 동안 캐시되므로 `status`가 실패 커넥터를 재시도하는 유일한 회복 경로다. 스윕의 401은 `withSweepRetry`(키체인에 바뀐 토큰이 있을 때만 1회 재시도)로 처리하고, CLI 갱신 사다리 `withAuthRetry`는 `call_read`·`call_write` 같은 명시적 호출에만 쓴다 — 미연결 커넥터의 401은 토큰 만료와 상태코드가 같아 사다리를 타면 커넥터당 `claude -p`가 뜬다.
- **도구 표면은 4개 고정** (D3): `search_tools`·`call_read`·`call_write`·`status`. 커넥터별 도구를 개별 노출하지 않는다 — 합산 100개 초과라 검색 메타 도구가 표준이다. `relay_` 접두사를 붙이지 않는다(MCP가 자동 네임스페이싱하므로 중복).
- **오류 모델**: `RelayError`(kind: usage|auth|upstream|transport|constraint|not_found). 도구 오류는 JSON-RPC error가 아니라 `isError` 결과로 실는다(MCP 규격).
- **stdlib만**: `go.mod`에 의존성이 없다. 새 의존성 추가는 설계 결정이 필요하다.

## Git 원칙

1인 개발 프로젝트라 별도 지시가 없으면 **main에 바로 커밋·푸시**한다 — 브랜치·PR 우회 없음. Orca 워크트리 세션(main은 기본 체크아웃에 있음)에서는 커밋 후 `git push origin HEAD:main`으로 main을 fast-forward하고, 워크트리 브랜치 원격도 같은 커밋으로 맞춰둔다.

## 문서 규약

- `docs/solutions/` — 과거 문제 해결 기록(버그·워크플로·도구 결정). 카테고리 디렉터리 + YAML frontmatter(`module`, `tags`, `problem_type`)로 검색. 문서화된 영역 작업 시 참고.
- `docs/DECISIONS.md`가 게이트다: 코드 작성은 DECISIONS.md에 "구현 시작" 항목이 기록된 뒤에만.
- `docs/MEASURED-FACTS.md`는 호출 규격의 근거 문서다 — 프로토콜 관련 상수·헤더를 바꿀 때는 실측이 먼저다.
- `docs/reference/connector_relay.py`는 gmail-triage에서 복사한 원형 스냅샷으로, 수정 대상이 아니다. 훅 relay 방식은 폴백으로 문서화만 한다(D2).
- 코드·문서 주석은 한국어. 코멘트는 코드가 보여주지 못하는 why만 한 줄.
