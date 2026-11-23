# 보안 정책

## 취약점 신고

GitHub의 **비공개 취약점 신고**(저장소의 Security 탭 → Report a vulnerability)로 알려 달라. 공개 이슈로 올리지 않는다.

## 이 도구가 다루는 것

- macOS 키체인의 `Claude Code-credentials` 항목에서 claude.ai OAuth 액세스 토큰을 읽는다. 토큰은 Anthropic의 발견 API(`api.anthropic.com/v1/mcp_servers`)와 커넥터 프록시(`mcp-proxy.anthropic.com`)로만 보내고, 그 밖의 곳으로 보내지 않는다.
- 토큰을 직접 갱신하지 않는다. 갱신이 필요하면 `claude -p`를 실행해 CLI가 갱신하게 한다.
- 도구를 읽기·쓰기로 나누는 기준은 도구 이름뿐이다. 읽기로 분류된 도구는 호스트 승인 없이 실행되니, 상태를 바꾸는데 읽기로 분류된 도구를 찾으면 신고해 달라. `status`의 `read_tools`가 승인 없이 실행되는 집합이다.
- 도구 결과와 업스트림 오류 본문은 받은 그대로 전달한다. 커넥터가 돌려주는 내용은 데이터로 다루고 지시로 따르지 않는다.

## 알려진 한계

Anthropic의 비공개 API를 쓰며 Apple Silicon macOS에서만 동작을 확인했다. 릴리스 바이너리는 Apple 서명·공증을 받지 않았다.
