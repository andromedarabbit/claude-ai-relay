# DESIGN-QUESTIONS — 상세 설계에서 풀 주제들

상세 설계 세션의 논의 순서 제안. 첫 주제(#1)는 이미 사용자가 지정했다.

## 주제 #1: Gmail 전용 vs 범용 커넥터 지원

첫 설계 주제. 양쪽의 비용 구조:

- **Gmail 전용**: gmail-triage의 검증된 도구면(list_labels, search_threads, get_thread, get_message, unlabel_message)을 그대로 노출. 가장 작은 첫 출하.
- **범용**: 커넥터별 선언(서버명, 도구 목록, 도구별 스키마, 허용 목록)을 설정으로 분리해 커넥터 추가를 데이터 추가로 만든다. 범용화 비용은 "서버명·도구 스키마·allow-list의 커넥터별 선언화"가 전부이지만, Gmail 하나로도 이 선언 구조는 필요하다 — 즉 Gmail 전용으로 시작해도 **선언 구조는 범용 형태로** 설계하는 쪽이 낫다는 가설. (검증할 가설이지 결정이 아니다)

## 주제 #2: 자식 프로세스 모델

- **호출마다 `claude -p` 스폰** (gmail-triage 방식): 구현 가장 단순, 콜당 CLI 기동+커넥터 연결 수 초.
- **`--input-format stream-json` 상시 자식**: 턴 = 배치. 커넥터 연결을 상각. 자식 hang·재시작 관리가 붙고, 매 턴 Stop 훅 발화 여부는 미실측(문서 근거만).

## 주제 #3: 권한 모델

- MCP 서버로 감싸면 도구 호출이 Claude Code의 MCP 권한 체계를 다시 탄다(allow-list·프롬프트).
- 스크립트 내부 `validate_call` 허용 목록도 그대로 유지해 이중 방어.
- Gmail unlabel_message는 labelIds=["INBOX"] + 문자열 messageId 조합만 허용 — 이 경계를 범용화 시 어떻게 선언화할지.

## 주제 #4: 패키징·배포

- uv 싱글파일 + 표준 라이브러리 JSON-RPC (mcp SDK 불필요) vs mcp Python SDK.
- 등록은 사용자 스코프 `claude mcp add` 한 줄. 팀 배포 여부(omcc 패키지화)는 별도 후행 결정.

## 검증된 실측은 [MEASURED-FACTS.md](MEASURED-FACTS.md), relay 메커니즘 배경은 [CONTEXT.md](CONTEXT.md) 참고.
