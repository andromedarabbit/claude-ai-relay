#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.9"
# dependencies = []
# ///
# 스냅샷: oh-my-skills 저장소 gmail-triage 스킬의 scripts/connector_relay.py
# 출처: https://git.baemin.in/dataplatform/oh-my-skills — skills/gmail-experts/gmail-triage/scripts/connector_relay.py
# 스냅샷일: 2026-11-15. 이 파일은 참고용 원본이며 이 프로젝트에서 수정하지 않는다.
"""claude.ai Gmail 커넥터를 모델 없이 직접 호출하는 relay — 커넥터가 안 뜨는 세션용.

왜 필요한가: claude.ai 커넥터는 claude.ai 로그인이 세션의 인증 수단일 때만 로딩된다.
ccs 프로필처럼 ``ANTHROPIC_AUTH_TOKEN``/``ANTHROPIC_API_KEY``가 설정된 세션에서는
"claude.ai connectors are disabled"로 Gmail 도구가 아예 없다(2026-11-08 실측).

방법: 인증 env를 지운 ``claude -p`` 자식을 띄우고, 호출 목록을 ``mcp_tool`` 타입의
Stop 훅 N개로 넣는다. 자식 모델은 "OK" 한 마디만 답하고(이 턴이 커넥터 연결 시간을 번다),
턴이 끝날 때 훅들이 커넥터 도구를 설정에 박힌 인자 그대로 직접 부른다. 결과는
stream-json의 ``hook_response.output``에 Gmail 원본 그대로 실린다 — 모델이 인자나
본문을 건드리지 않는다. 실측: 본문 20건 313KB 바이트 동일, 4.9초, 묶음당 haiku 1턴.
(UserPromptSubmit 훅은 커넥터 연결 전에 실행돼 "not connected"로 실패한다.)

서브커맨드:
  probe                              list_labels 1건으로 relay 사용 가능 여부 확인
  list-labels                        라벨 목록 JSON 조회
  fetch --query Q --output-file F    커넥터 경로와 같은 collected.json 생성
  archive --ids F [--output-file F]  재확인 → INBOX 제거 → 독립 검증

응답 매핑: hook_response에는 어느 훅의 응답인지 표시가 없고 순서도 입력과 다르다.
읽기 결과는 응답 안의 id로 매핑하고, 요청했는데 응답에 없는 id는 실패로 본다
(오류 응답 "Requested entity was not found."에는 id가 없다). unlabel_message 응답(``{}``)은
성공 판정에 쓰지 않는다 — 성공은 archive의 재조회 검증으로만 판정한다.

종료 코드:
  0  성공
  2  사용법 오류(입력 파일 없음, 허용되지 않은 호출 등)
  3  relay 사용 불가(자식 실패, 커넥터 미연결, 인증 실패, 타임아웃) — 사유는 stderr
  4  일부 실패(수집 누락 또는 검증 실패) — 결과 파일은 쓰고 누락·실패 id는 stderr

shortcut: 훅 응답을 내용으로 매핑하므로 같은 id를 한 묶음에 두 번 넣으면 구분하지 못한다 —
호출 목록은 항상 중복을 제거해 넘긴다.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import gmail_ids  # noqa: E402
import state_cache  # noqa: E402
from filter_labels import user_labels  # noqa: E402
from outfile import write_text  # noqa: E402

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_UNAVAILABLE = 3
EXIT_INCOMPLETE = 4

SERVER = "claude.ai Gmail"
READ_TOOLS = frozenset({"search_threads", "get_thread", "get_message", "list_labels"})
ARCHIVE_LABELS = ["INBOX"]
BATCH_SIZE = 20  # 실측 20건(313KB) 4.9초. 더 키우면 훅 시간 제한 실측부터
TIMEOUT_SECONDS = 300
MAX_SEARCH_PAGES = 100
SEARCH_PAGE_SIZE = 50
STRIP_ENV_PREFIXES = ("ANTHROPIC_", "CLAUDE_CODE_USE_")
CHILD_SYSTEM_PROMPT = "Reply OK."


class RelayError(Exception):
    """relay 자체를 쓸 수 없는 실패. kind: not_connected | auth | timeout | other."""

    def __init__(self, kind: str, detail: str):
        super().__init__(f"{kind}: {detail}")
        self.kind = kind
        self.detail = detail


def validate_call(tool: str, args: dict) -> None:
    """허용 목록 밖 호출을 자식 실행 전에 거부한다(훅 호출은 Claude Code 권한 확인을 거치지 않는다)."""
    if tool in READ_TOOLS:
        return
    if tool == "unlabel_message" and args.get("labelIds") == ARCHIVE_LABELS and isinstance(args.get("messageId"), str):
        return
    raise ValueError(f"허용되지 않은 relay 호출: {tool} {json.dumps(args, ensure_ascii=False)}")


def child_env(base: dict | None = None) -> dict:
    """claude.ai 로그인보다 우선하는 인증·라우팅 env를 지운다(ccs가 BASE_URL을 다른 모델 프록시로 돌려놓는다)."""
    env = dict(os.environ if base is None else base)
    return {k: v for k, v in env.items() if not k.startswith(STRIP_ENV_PREFIXES)}


def build_settings(calls: list[tuple[str, dict]]) -> dict:
    hooks = [{"type": "mcp_tool", "server": SERVER, "tool": tool, "input": args} for tool, args in calls]
    return {"hooks": {"Stop": [{"hooks": hooks}]}}


def classify_failure(text: str) -> str:
    lowered = text.lower()
    if "not connected" in lowered:
        return "not_connected"
    if any(k in lowered for k in ("/login", "unauthorized", "authentication", "invalid api key", "oauth")):
        return "auth"
    return "other"


def run_batch(calls: list[tuple[str, dict]], timeout: int = TIMEOUT_SECONDS) -> list[dict]:
    """호출 목록을 한 번의 ``claude -p``로 실행한다. 응답 목록(순서 무관) [{ok, output}]을 돌려준다.

    응답 수가 호출 수와 다르거나 커넥터 미연결 응답이 섞이면 묶음 전체를 RelayError로 본다.
    """
    for tool, args in calls:
        validate_call(tool, args)
    cmd = [
        "claude", "-p", "Reply OK", "--model", "haiku",
        "--setting-sources", "", "--settings", json.dumps(build_settings(calls)),
        "--disable-slash-commands", "--no-session-persistence",
        "--system-prompt", CHILD_SYSTEM_PROMPT,
        "--output-format", "stream-json", "--verbose", "--include-hook-events",
    ]
    with tempfile.TemporaryDirectory(prefix="gmail-relay-") as workdir:
        try:
            proc = subprocess.run(cmd, cwd=workdir, env=child_env(), stdin=subprocess.DEVNULL,
                                  capture_output=True, text=True, timeout=timeout)
        except FileNotFoundError as e:
            raise RelayError("other", f"claude CLI를 찾을 수 없습니다: {e}") from e
        except subprocess.TimeoutExpired as e:
            raise RelayError("timeout", f"{timeout}초 안에 끝나지 않았습니다") from e
    responses, result_text = [], ""
    for line in proc.stdout.splitlines():
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") == "result":
            result_text = str(event.get("result") or "")
        if event.get("subtype") == "hook_response" and event.get("hook_event") == "Stop":
            responses.append({"ok": event.get("outcome") == "success", "output": event.get("output") or ""})
    if proc.returncode != 0:
        detail = (proc.stderr.strip() or result_text or "출력 없음")[-500:]
        raise RelayError(classify_failure(detail), f"claude -p 종료 코드 {proc.returncode}: {detail}")
    not_connected = [r for r in responses if not r["ok"] and classify_failure(r["output"]) == "not_connected"]
    if not_connected:
        raise RelayError("not_connected", not_connected[0]["output"].strip()[:300])
    if len(responses) != len(calls):
        detail = (result_text or proc.stderr.strip())[-300:]
        raise RelayError(classify_failure(detail),
                         f"훅 응답 {len(responses)}건 / 호출 {len(calls)}건 — 커넥터가 로딩되지 않았을 수 있습니다. {detail}")
    return responses


def run_calls(calls: list[tuple[str, dict]], batch_size: int = BATCH_SIZE) -> list[dict]:
    out: list[dict] = []
    for start in range(0, len(calls), batch_size):
        out.extend(run_batch(calls[start:start + batch_size]))
    return out


def parse_objects(responses: list[dict]) -> list[dict]:
    """성공 응답 중 JSON 객체만 돌려준다(오류 응답은 id가 없어 매핑할 수 없다 — 누락으로 처리된다)."""
    objects = []
    for r in responses:
        if not r["ok"]:
            continue
        try:
            obj = json.loads(r["output"])
        except ValueError:
            continue
        if isinstance(obj, dict):
            objects.append(obj)
    return objects


def fetch_by_id(tool: str, key: str, ids: list[str], extra: dict) -> dict[str, dict]:
    """ids마다 tool을 불러 응답 안의 ``id``로 매핑한다. 응답이 없는 id는 결과에 없다."""
    unique = list(dict.fromkeys(ids))
    responses = run_calls([(tool, {key: i, **extra}) for i in unique])
    wanted = set(unique)
    return {obj["id"]: obj for obj in parse_objects(responses) if obj.get("id") in wanted}


def probe() -> int:
    for attempt in range(2):
        try:
            responses = run_batch([("list_labels", {})])
        except RelayError as e:
            if e.kind == "not_connected" and attempt == 0:
                time.sleep(2)
                continue
            print(f"relay 사용 불가({e.kind}): {e.detail}", file=sys.stderr)
            return EXIT_UNAVAILABLE
        if parse_objects(responses):
            print("relay=ok")
            return EXIT_OK
        print(f"relay 사용 불가(other): list_labels 응답 오류 — {responses[0]['output'][:300]}", file=sys.stderr)
        return EXIT_UNAVAILABLE
    return EXIT_UNAVAILABLE


def list_labels() -> int:
    try:
        objects = parse_objects(run_batch([("list_labels", {})]))
    except RelayError as e:
        print(f"라벨 조회 실패({e.kind}): {e.detail}", file=sys.stderr)
        return EXIT_UNAVAILABLE
    if not objects or not isinstance(objects[0].get("labels"), list):
        print("라벨 조회 실패(other): list_labels 응답 오류", file=sys.stderr)
        return EXIT_UNAVAILABLE
    print(json.dumps(objects[0], ensure_ascii=False))
    return EXIT_OK


def search_all_threads(query: str) -> list[str]:
    thread_ids: list[str] = []
    token = None
    for _ in range(MAX_SEARCH_PAGES):
        args = {"query": query, "view": "THREAD_VIEW_IDS_ONLY", "pageSize": SEARCH_PAGE_SIZE}
        if token:
            args["pageToken"] = token
        page = parse_objects(run_batch([("search_threads", args)]))
        if not page:
            raise RelayError("other", f"search_threads 실패(pageToken={token})")
        thread_ids.extend(t["id"] for t in page[0].get("threads") or [] if isinstance(t, dict) and t.get("id"))
        token = page[0].get("nextPageToken")
        if not token:
            return list(dict.fromkeys(thread_ids))
    raise RelayError("other", f"검색 결과가 {MAX_SEARCH_PAGES}페이지를 넘었습니다 — 쿼리를 좁히세요")


def fetch(query: str, output_file: str, state_path: str) -> int:
    try:
        thread_ids = search_all_threads(query)
        threads = fetch_by_id("get_thread", "threadId", thread_ids, {"messageFormat": "METADATA_ONLY"})
        missing_threads = [t for t in thread_ids if t not in threads]
        inbox_ids = [m["id"] for t in thread_ids if t in threads
                     for m in threads[t].get("messages") or []
                     if isinstance(m, dict) and "INBOX" in (m.get("labelIds") or [])]
        try:
            fresh, warning = gmail_ids.filter_new(list(dict.fromkeys(inbox_ids)), state_path)
        except RuntimeError as e:
            print(e, file=sys.stderr)
            return EXIT_USAGE
        if warning:
            print(warning, file=sys.stderr)
        messages = fetch_by_id("get_message", "messageId", fresh, {"messageFormat": "PLAIN_TEXT"})
    except RelayError as e:
        print(f"relay 수집 실패({e.kind}): {e.detail}", file=sys.stderr)
        return EXIT_UNAVAILABLE
    collected = [{
        "id": i,
        "thread": messages[i].get("threadId", ""),
        "labelIds": messages[i].get("labelIds") or [],
        "subject": messages[i].get("subject", ""),
        "sender": messages[i].get("sender", ""),
        "date": messages[i].get("date", ""),
        "body": messages[i].get("plaintextBody", ""),
    } for i in fresh if i in messages]
    missing_messages = [i for i in fresh if i not in messages]
    write_text(output_file, json.dumps(collected, ensure_ascii=False, indent=2) + "\n")
    print(f"threads={len(thread_ids)} inbox={len(set(inbox_ids))} already_processed={len(set(inbox_ids)) - len(fresh)} "
          f"collected={len(collected)}", file=sys.stderr)
    if missing_threads or missing_messages:
        print(f"수집 누락: 스레드 {missing_threads} 메시지 {missing_messages}", file=sys.stderr)
        return EXIT_INCOMPLETE
    return EXIT_OK


def archive(ids: list[str], output_file: str | None) -> int:
    ids = list(dict.fromkeys(ids))
    skipped: dict[str, list[str]] = {}
    try:
        before = fetch_by_id("get_message", "messageId", ids, {"messageFormat": "METADATA_ONLY"})
    except RelayError as e:
        print(f"relay 재확인 실패({e.kind}): {e.detail} — 아무것도 실행하지 않았습니다", file=sys.stderr)
        return EXIT_UNAVAILABLE
    targets = []
    for i in ids:
        labels = (before.get(i) or {}).get("labelIds")
        if labels is None:
            reason = "lookup-failed"
        elif "INBOX" not in labels:
            reason = "not-in-inbox"
        elif "STARRED" in labels:
            reason = "starred"
        elif user_labels(labels):
            reason = "user-label"
        else:
            targets.append(i)
            continue
        skipped.setdefault(reason, []).append(i)
    exec_error = None
    try:
        run_calls([("unlabel_message", {"messageId": i, "labelIds": ARCHIVE_LABELS}) for i in targets])
    except RelayError as e:  # 일부는 실행됐을 수 있으니 검증은 그대로 한다
        exec_error = e
    try:
        after = fetch_by_id("get_message", "messageId", targets, {"messageFormat": "METADATA_ONLY"})
    except RelayError as e:
        print(f"relay 검증 실패({e.kind}): {e.detail} — 결과를 확인할 수 없어 처리 완료로 기록하면 안 됩니다", file=sys.stderr)
        return EXIT_UNAVAILABLE
    verified = [i for i in targets if i in after and isinstance(after[i].get("labelIds"), list)
                and all(isinstance(label, str) for label in after[i]["labelIds"])
                and "INBOX" not in after[i]["labelIds"]]
    failed = [i for i in targets if i not in verified]
    result = {"verified": verified, "failed": failed, "skipped": skipped}
    text = json.dumps(result, ensure_ascii=False, indent=2) + "\n"
    if output_file:
        write_text(output_file, text)
    else:
        sys.stdout.write(text)
    print(f"verified={len(verified)} failed={len(failed)} skipped={sum(len(v) for v in skipped.values())}", file=sys.stderr)
    if exec_error:
        print(f"실행 중 relay 오류({exec_error.kind}): {exec_error.detail}", file=sys.stderr)
    return EXIT_INCOMPLETE if failed or exec_error else EXIT_OK


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="connector_relay.py", description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("probe")
    sub.add_parser("list-labels")
    p_fetch = sub.add_parser("fetch")
    p_fetch.add_argument("--query", required=True)
    p_fetch.add_argument("--output-file", required=True)
    p_fetch.add_argument("--state", default=state_cache.DEFAULT_STATE_PATH)
    p_archive = sub.add_parser("archive")
    p_archive.add_argument("--ids", required=True, help="승인된 16진수 messageId JSON 배열(문자열 또는 파일 경로)")
    p_archive.add_argument("--output-file")
    args = parser.parse_args(argv)

    if args.command == "probe":
        return probe()
    if args.command == "list-labels":
        return list_labels()
    if args.command == "fetch":
        return fetch(args.query, args.output_file, args.state)
    try:
        ids = gmail_ids._load_list(args.ids)
    except (OSError, ValueError) as e:
        print(f"--ids를 읽을 수 없습니다: {e}", file=sys.stderr)
        return EXIT_USAGE
    if not ids:
        print("--ids가 비었습니다 — 아카이브할 대상이 없습니다", file=sys.stderr)
        return EXIT_USAGE
    return archive(ids, args.output_file)


if __name__ == "__main__":
    sys.exit(main())
