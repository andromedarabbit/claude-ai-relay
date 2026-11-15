# KICKOFF — 상세 설계 세션 시작 프롬프트

아래 코드블록 안의 프롬프트 전체를 복사해 새 세션(Orca 작업 공간 권장)에 붙여넣는다.

````text
claude-ai-relay 상세 설계를 시작한다. 코드는 작성하지 않는다 — 설계만.

## 배경

이 저장소는 claude.ai 커넥터(Gmail 등)를 API 인증 세션에서도 쓸 수 있게 하는
relay MCP 서버 프로젝트다. relay 메커니즘은 gmail-triage 스킬에서 검증됐고,
문서가 이 대화 없이도 읽히게 돼 있다.

## 먼저 읽을 것 (순서대로, 전부)

1. docs/CONTEXT.md — 문제 정의와 relay 메커니즘
2. docs/RESEARCH.md — OSS 조사 결과
3. docs/DESIGN-QUESTIONS.md — 설계 주제 목록
4. docs/MEASURED-FACTS.md — 검증된 실측 수치
5. docs/reference/connector_relay.py — relay 원형 구현 (참고용 스냅샷)

## 오늘의 주제

docs/DESIGN-QUESTIONS.md 주제 #1: **Gmail 전용 vs 범용 커넥터 지원**.
DESIGN-QUESTIONS.md에 적은 가설(Gmail 전용으로 시작해도 선언 구조는 범용 형태로)
을 출발점으로 삼고, ce-brainstorm 또는 ce-plan으로 결론을 내라.

## 산출물

결정 사항과 근거를 docs/DECISIONS.md에 기록한다. 결론이 나면 남은 주제(#2~#4)도
같은 방식으로 이어간다. 코드 작성은 DECISIONS.md에 "구현 시작" 항목이 생긴 뒤에만.

## 진행 방식

한 번에 한 주제. 결정마다 AskUserQuestion으로 사용자 확인을 받는다.
````

실행 한 줄:

```bash
orca worktree create --repo id:<repoId> --name design-kickoff --agent claude --prompt "$(cat docs/KICKOFF.md 중 프롬프트 블록)" --json
```

(정확한 `--repo id:` 값은 `orca repo list --json`으로 확인)
