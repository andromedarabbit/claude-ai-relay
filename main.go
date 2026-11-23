// claude-ai-relay — claude.ai 커넥터(Gmail, Calendar 등)를 인증 불문 세션에 노출하는 relay.
//
// 전송: 키체인 claude.ai OAuth 토큰으로 api.anthropic.com/v1/mcp_servers(발견)와
// mcp-proxy.anthropic.com/v1/mcp/{mcpsrv_id}(도구 호출)에 직접 JSON-RPC.
// 호출 규격 근거: docs/MEASURED-FACTS.md (헤더·UA·키체인 다중 항목·회전 정책 전부 실측).
//
// 토큰 갱신은 CLI 트리거만 한다 — 직접 refresh는 회전으로 구 at·rt를 즉시
// revoke해 CLI 자격증명 체인을 파괴한다(2026-11-15 실측).
//
// 서브커맨드:
//
//	claude-ai-relay probe       발견→live 취득→읽기 호출 1건으로 relay 사용 가능 여부 확인
//	claude-ai-relay selfcheck   오프라인 assert (네트워크·키체인 불필요)
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	userAgent = "claude-cli/2.1.0 (external, cli)" // shortcut: Cloudflare 1010 차단 시 이 상수만 갱신
	version   = "0.3.1"                            // 릴리스마다 같이 올린다 — serverInfo·clientInfo에 그대로 노출된다

	discoveryURL   = "https://api.anthropic.com/v1/mcp_servers"
	discoveryBeta  = "mcp-servers-2025-12-04"
	anthropicVer   = "2023-06-01"
	proxyBase      = "https://mcp-proxy.anthropic.com/v1/mcp/"
	keychainSvc    = "Claude Code-credentials"
	requestTimeout = 30 * time.Second
	sweepTimeout   = 10 * time.Second // 커넥터당 live 취득 마감 (D8)
)

// RelayError — kind: usage | auth | upstream | transport | constraint | not_found.
type RelayError struct {
	Kind   string
	Detail string
}

func (e *RelayError) Error() string { return e.Kind + ": " + e.Detail }

func usageErr(format string, a ...any) error {
	return &RelayError{Kind: "usage", Detail: fmt.Sprintf(format, a...)}
}

// --- 키체인 ---

type credentials struct {
	AccessToken string
	Account     string
}

var acctRe = regexp.MustCompile(`"acct"<blob>="([^"]*)"`)
var svcRe = regexp.MustCompile(`"svce"<blob>="([^"]*)"`)

// parseKeychainDump — dump-keychain 출력을 항목 블록으로 쪼개 서비스가 일치하는 acct 목록을 뽑는다.
// 같은 서비스 이름의 항목이 여러 개다(빈 레거시 + 살아 있는 항목) — 전부 돌려준다.
func parseKeychainDump(dump string, service string) []string {
	var accts []string
	for _, block := range strings.Split(dump, "keychain: ") {
		if m := svcRe.FindStringSubmatch(block); m == nil || m[1] != service {
			continue
		}
		if m := acctRe.FindStringSubmatch(block); m != nil {
			accts = append(accts, m[1])
		}
	}
	return accts
}

func readKeychain(acct string) (credentials, error) {
	out, err := exec.Command("security", "find-generic-password", "-s", keychainSvc, "-a", acct, "-w").Output()
	if err != nil {
		return credentials{}, &RelayError{Kind: "auth", Detail: fmt.Sprintf("키체인 항목(%s) 조회 실패: %v", acct, err)}
	}
	var parsed struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		return credentials{}, &RelayError{Kind: "auth", Detail: fmt.Sprintf("키체인 항목(%s) 파싱 실패: %v", acct, err)}
	}
	return credentials{AccessToken: parsed.ClaudeAiOauth.AccessToken, Account: acct}, nil
}

// loadToken — accessToken이 있는 첫 항목. 빈 레거시 항목은 스킵 (MEASURED-FACTS: 다중 항목).
func loadToken() (credentials, error) {
	dump, err := exec.Command("security", "dump-keychain", "login.keychain-db").Output()
	if err != nil {
		return credentials{}, &RelayError{Kind: "auth", Detail: "키체인 열거 실패: " + err.Error()}
	}
	for _, acct := range parseKeychainDump(string(dump), keychainSvc) {
		cred, err := readKeychain(acct)
		if err != nil || cred.AccessToken == "" {
			continue
		}
		return cred, nil
	}
	return credentials{}, &RelayError{Kind: "auth", Detail: "유효한 claude.ai 토큰이 없습니다 — claude login 후 재시도"}
}

// refreshViaCLI — CLI 트리거 갱신. env를 제거한 claude -p를 띄우면 CLI가
// refresh를 수행해 키체인에 기록한다. 직접 refresh는 금지(회전이 체인을 파괴).
func refreshViaCLI() error {
	cmd := exec.Command("claude", "-p", "hi", "--model", "haiku", "--setting-sources", "",
		"--disable-slash-commands", "--no-session-persistence")
	cmd.Env = stripAuthEnv(os.Environ())
	cmd.Stdout, cmd.Stderr = nil, nil
	return cmd.Run()
}

func stripAuthEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_CODE_USE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// --- 전송 ---

type serverEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

func newHTTPClient() *http.Client { return &http.Client{Timeout: requestTimeout} }

func discoverServers(tok string) ([]serverEntry, error) {
	req, _ := http.NewRequest("GET", discoveryURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", discoveryBeta)
	req.Header.Set("anthropic-version", anthropicVer)
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return nil, &RelayError{Kind: "transport", Detail: "발견 API 요청 실패: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("발견 API", resp.StatusCode, body)
	}
	var parsed struct {
		Data []serverEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, &RelayError{Kind: "upstream", Detail: "발견 API 응답 파싱 실패: " + err.Error()}
	}
	return parsed.Data, nil
}

func httpError(what string, code int, body []byte) error {
	kind := "upstream"
	if code == http.StatusUnauthorized {
		kind = "auth"
	}
	detail := string(body)
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return &RelayError{Kind: kind, Detail: fmt.Sprintf("%s %d: %s", what, code, detail)}
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// withAuthRetry — auth 오류 1회만 갱신 사다리(재독 → CLI 트리거 → 재독)로 재시도한다.
// 발견 API와 프록시 호출 양쪽에 적용한다. 갱신됐으면 새 cred를 돌려준다.
func withAuthRetry[T any](cred credentials, fn func(credentials) (T, error)) (T, credentials, error) {
	v, err := fn(cred)
	if err == nil {
		return v, cred, nil
	}
	if re, ok := err.(*RelayError); !ok || re.Kind != "auth" {
		return v, cred, err
	}
	next, lerr := ensureToken(cred)
	if lerr != nil {
		return v, cred, lerr
	}
	v, err = fn(next)
	return v, next, err
}

// withSweepRetry — 전 커넥터 tools/list 스윕용 401 처리. 미연결 커넥터의 401은 토큰 만료와
// 상태코드가 같아서 withAuthRetry를 타면 커넥터당 claude -p가 한 번씩 뜬다(D8). 발견 호출이
// 방금 같은 토큰을 검증했으므로 reread(키체인 재독)가 바뀐 토큰을 줄 때만 1회 재시도하고, 같은
// 토큰이면 업스트림 오류를 그대로 돌려준다 — unconnected에 사유로 남는다.
func withSweepRetry[T any](cred credentials, reread func() (credentials, error), fn func(credentials) (T, error)) (T, credentials, error) {
	v, err := fn(cred)
	if err == nil {
		return v, cred, nil
	}
	if re, ok := err.(*RelayError); !ok || re.Kind != "auth" {
		return v, cred, err
	}
	next, lerr := reread()
	if lerr != nil || next.AccessToken == cred.AccessToken {
		return v, cred, err
	}
	v, err = fn(next)
	return v, next, err
}

// proxyCall — 프록시 MCP 엔드포인트로 JSON-RPC 1건. 401이면 갱신 사다리 후 1회 재시도.
func proxyCall(ctx context.Context, cred credentials, mcpsrvID, method string, params any) (json.RawMessage, credentials, error) {
	return withAuthRetry(cred, func(c credentials) (json.RawMessage, error) {
		return proxyCallOnce(ctx, c, mcpsrvID, method, params)
	})
}

// proxySessions — 커넥터별 프록시 세션 (mcpsrvID → uuid). 일부 업스트림(Slack 등)은
// initialize로 수립된 세션의 tools/call만 받는다. 서버 루프가 단일 스레드라 잠금 불필요.
var proxySessions = map[string]string{}

var initParams = map[string]any{
	"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
	"clientInfo": map[string]any{"name": "claude-ai-relay", "version": version},
}

func proxyCallOnce(ctx context.Context, cred credentials, mcpsrvID, method string, params any) (json.RawMessage, error) {
	sess, known := proxySessions[mcpsrvID]
	if !known {
		sess = newUUID()
		if _, err := rawProxy(ctx, cred, mcpsrvID, sess, "initialize", initParams); err != nil {
			return nil, err
		}
		proxySessions[mcpsrvID] = sess
	}
	raw, err := rawProxy(ctx, cred, mcpsrvID, sess, method, params)
	if err == nil {
		return raw, nil
	}
	// 세션 무효화(만료 등)면 1회 재수립 후 재시도
	if known && strings.Contains(err.Error(), "initialize request") {
		delete(proxySessions, mcpsrvID)
		return proxyCallOnce(ctx, cred, mcpsrvID, method, params)
	}
	return nil, err
}

func rawProxy(ctx context.Context, cred credentials, mcpsrvID, sess, method string, params any) (json.RawMessage, error) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequestWithContext(ctx, "POST", proxyBase+mcpsrvID, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Mcp-Client-Session-Id", sess)
	req.Header.Set("anthropic-version", anthropicVer)
	resp, err := newHTTPClient().Do(req)
	if err != nil {
		return nil, &RelayError{Kind: "transport", Detail: "프록시 요청 실패: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, httpError("프록시 "+mcpsrvID, resp.StatusCode, body)
	}
	return parseSSEJSON(body)
}

// parseSSEJSON — 응답이 SSE(data: 줄) 또는 plain JSON이다. 첫 JSON 페이로드를 돌려준다.
func parseSSEJSON(body []byte) (json.RawMessage, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if strings.HasPrefix(line, "{") {
			return json.RawMessage(line), nil
		}
	}
	return nil, &RelayError{Kind: "upstream", Detail: "응답에서 JSON 페이로드를 찾지 못함"}
}

var noCLIRefresh bool

// ensureToken — 401 사다리: 키체인 재독 → CLI 갱신 허용 시에만 갱신 트리거.
func ensureToken(stale credentials) (credentials, error) {
	if cred, err := loadToken(); err == nil && cred.AccessToken != stale.AccessToken {
		return cred, nil
	}
	if noCLIRefresh {
		return credentials{}, &RelayError{Kind: "auth", Detail: "토큰이 만료되었습니다 — Claude Code에서 로그인한 뒤 재시도"}
	}
	if err := refreshViaCLI(); err != nil {
		return credentials{}, &RelayError{Kind: "auth", Detail: "CLI 갱신 실패: " + err.Error() + " — claude login 후 재시도"}
	}
	if cred, err := loadToken(); err == nil {
		return cred, nil
	}
	return credentials{}, &RelayError{Kind: "auth", Detail: "갱신 후에도 토큰 없음 — claude login 후 재시도"}
}

// --- 이름 규약 (D7: 선언 없는 커넥터 접두사와 읽기/쓰기 분류) ---

// slugify — 발견 DisplayName → 도구 접두사 slug. 규약 고정, 축약 테이블 없음(D7).
// 비알파벳 연속·앞뒤는 '_' 하나로 수렴한다.
func slugify(display string) string {
	s := strings.ToLower(display)
	var b strings.Builder
	pending := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if pending && b.Len() > 0 {
				b.WriteByte('_')
			}
			pending = false
			b.WriteByte(c)
		} else {
			pending = true
		}
	}
	return b.String()
}

// readVerbs — 읽기로 분류할 동사 토큰. 확장은 코드 수정(별도 지시 시, D7).
var readVerbs = map[string]bool{
	"get": true, "list": true, "search": true, "find": true, "read": true,
	"describe": true, "download": true, "query": true, "fetch": true,
	"lookup": true, "discover": true, "whoami": true,
}

// writeVerbs — 토큰 하나라도 있으면 읽기 동사와 섞여 있어도 쓰기로 판정한다(D8).
// 실측: claude_data.certify_query가 조직 단위 쓰기인데 query 토큰 하나로 읽기가 됐다.
var writeVerbs = map[string]bool{
	"add": true, "apply": true, "approve": true, "archive": true, "assign": true,
	"cancel": true, "certify": true, "create": true, "delete": true, "disable": true,
	"edit": true, "enable": true, "execute": true, "forward": true, "insert": true,
	"invoke": true, "invite": true, "label": true, "mark": true, "modify": true,
	"move": true, "post": true, "publish": true, "remove": true, "rename": true,
	"reply": true, "replace": true, "respond": true, "revoke": true, "schedule": true,
	"send": true, "set": true, "share": true, "submit": true, "trash": true,
	"trigger": true, "uncertify": true, "update": true, "upload": true, "write": true,
}

// toolTokens — 도구명을 '_'와 카멜케이스 경계로 토큰 분리(atlassianUserInfo → atlassian·user·info).
func toolTokens(name string) []string {
	var toks []string
	for _, part := range strings.Split(name, "_") {
		start := 0
		for i := 1; i < len(part); i++ {
			c, p := part[i], part[i-1]
			up, prevUp := c >= 'A' && c <= 'Z', p >= 'A' && p <= 'Z'
			lo := c >= 'a' && c <= 'z'
			if up && !prevUp { // 소문자→대문자 경계 (userInfo의 U)
				toks = append(toks, part[start:i])
				start = i
			} else if lo && prevUp && i-1 > start { // 대문자 연쇄 뒤 소문자 (HTTPServer의 S)
				toks = append(toks, part[start:i-1])
				start = i - 1
			}
		}
		toks = append(toks, part[start:])
	}
	return toks
}

// classifyTool — 쓰기 동사 토큰이 하나라도 있으면 쓰기(D8), 아니면 읽기 동사 토큰이
// 하나라도 있으면 읽기, 그 밖은 쓰기(D7). 토큰 정확일치만(부분문자열 금지 — update_target의
// "get" 함정, list_labels의 labels≠label). 미확실 → 쓰기: 호스트 승인을 거치는 안전한 쪽.
func classifyTool(name string) string {
	toks := toolTokens(name)
	for _, tok := range toks {
		if writeVerbs[strings.ToLower(tok)] {
			return "write"
		}
	}
	for _, tok := range toks {
		if readVerbs[strings.ToLower(tok)] {
			return "read"
		}
	}
	return "write"
}

// liveTools — tools/list 결과. search_tools 상세 표시와 validateArgs의 유일한 원천이다.
type liveTools struct {
	schemas      map[string]map[string]any // tool name → inputSchema
	descriptions map[string]string         // tool name → description
}

// fetchLiveTools — 커넥터의 tools/list. 스윕 전용이라 401에 CLI 갱신 사다리를 타지 않는다(D8).
// 스윕 중 토큰이 바뀌면 바뀐 cred를 돌려주므로 호출자는 다음 커넥터에 이어서 써야 한다.
func fetchLiveTools(ctx context.Context, cred credentials, mcpsrvID string) (liveTools, credentials, error) {
	raw, cred, err := withSweepRetry(cred, loadToken, func(c credentials) (json.RawMessage, error) {
		return proxyCallOnce(ctx, c, mcpsrvID, "tools/list", map[string]any{})
	})
	if err != nil {
		return liveTools{}, cred, err
	}
	var res struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return liveTools{}, cred, &RelayError{Kind: "upstream", Detail: "tools/list 파싱 실패: " + err.Error()}
	}
	lt := liveTools{schemas: map[string]map[string]any{}, descriptions: map[string]string{}}
	for _, t := range res.Result.Tools {
		lt.schemas[t.Name] = t.InputSchema
		lt.descriptions[t.Name] = t.Description
	}
	return lt, cred, nil
}

// --- stdio MCP 서버 (도구 표면 4개, D3·D4) ---

// conn — 노출 커넥터 1건: slug 접두사, 발견 DisplayName, 프록시 id, live 도구.
type conn struct {
	displayName string
	id          string // mcpsrv_ 형식
	live        liveTools
}

type relayState struct {
	cred        credentials
	connectors  map[string]*conn  // slug → conn
	unconnected map[string]string // 발견 DisplayName → tools/list 실패 요약 (status가 표시)
	liveToken   string            // 캐시 시점의 토큰 — 바뀌면 재취득
}

var state *relayState

func connectorSlugs() []string {
	slugs := make([]string, 0, len(state.connectors))
	for s := range state.connectors {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	return slugs
}

// bootstrap — 첫 상위 호출 시 1회 (initialize를 지연시키지 않는다).
// 전역 실패(키체인·발견)만 fail fast — 커넥터별 tools/list 실패(미연결 401 등)는
// unconnected에 기록하고 계속한다(D7).
func bootstrap() error {
	if state != nil && state.liveToken == state.cred.AccessToken {
		return nil
	}
	cred, err := loadToken()
	if err != nil {
		return err
	}
	servers, cred, err := withAuthRetry(cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		return err
	}
	st := &relayState{connectors: map[string]*conn{}, unconnected: map[string]string{}}
	for _, s := range servers {
		if cred, err = st.addServer(cred, s, fetchLiveTools); err != nil {
			return err
		}
	}
	st.cred, st.liveToken = cred, cred.AccessToken // 스윕 중 바뀐 토큰을 반영한다(D8)
	state = st
	return nil
}

// fetchLive — addServer의 취득 함수. selfcheck가 가짜를 넣어 오프라인으로 검증한다.
type fetchLive func(ctx context.Context, cred credentials, mcpsrvID string) (liveTools, credentials, error)

// addServer — 발견된 서버 1건을 state에 편입한다. bootstrap·status(실패 재시도)·probe가 쓰는 단일 규칙.
// 커넥터별 실패는 unconnected로 흡수하고(D7), slug 충돌만 잘못된 커넥터로 라우팅될 수 있어
// 전역 실패로 돌려준다(D8 — fail fast 유지, 사용자 결정).
func (st *relayState) addServer(cred credentials, s serverEntry, fetch fetchLive) (credentials, error) {
	slug := slugify(s.DisplayName)
	if slug == "" {
		st.unconnected[s.DisplayName] = "usage: DisplayName이 slug 규약으로 수렴하지 않음"
		return cred, nil
	}
	if prev, dup := st.connectors[slug]; dup {
		return cred, usageErr("커넥터 slug 충돌: %s — DisplayName %q와 %q가 규약으로 수렴", slug,
			prev.displayName, s.DisplayName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sweepTimeout)
	defer cancel()
	live, cred, err := fetch(ctx, cred, s.ID)
	if err != nil {
		st.unconnected[s.DisplayName] = err.Error()
		return cred, nil
	}
	st.connectors[slug] = &conn{displayName: s.DisplayName, id: s.ID, live: live}
	delete(st.unconnected, s.DisplayName)
	return cred, nil
}

// findTool — "gmail.list_labels" 또는 bare 이름(전 커넥터에서 유일할 때만).
func findTool(ref string) (*conn, string, error) {
	if slug, tool, ok := strings.Cut(ref, "."); ok {
		c, exists := state.connectors[slug]
		if !exists {
			return nil, "", &RelayError{Kind: "not_found",
				Detail: fmt.Sprintf("알 수 없는 커넥터: %s (연결됨: %v)", slug, connectorSlugs())}
		}
		if _, ok := c.live.schemas[tool]; !ok {
			return nil, "", &RelayError{Kind: "not_found",
				Detail: fmt.Sprintf("%s.%s: 업스트림에 없는 도구", slug, tool)}
		}
		return c, tool, nil
	}
	var hits []string
	for _, slug := range connectorSlugs() {
		if _, ok := state.connectors[slug].live.schemas[ref]; ok {
			hits = append(hits, slug+"."+ref)
		}
	}
	switch len(hits) {
	case 1:
		return findTool(hits[0])
	case 0:
		return nil, "", &RelayError{Kind: "not_found", Detail: "존재하지 않는 도구: " + ref}
	default:
		return nil, "", usageErr("%s는 여러 커넥터에 있음 — 커넥터를 붙여 지정: %v", ref, hits)
	}
}

// validateArgs — required 존재 + 타입 일치 + 스키마에 없는 키 거부.
// shortcut: enum/format 등 고급 스키마는 검사 안 함 — 필요해지면 그때.
func validateArgs(toolRef string, schema map[string]any, args map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return nil // 업스트림이 스키마를 안 주면 통과 — live 스키마가 유일 원천이라 비교할 다른 근거가 없다
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		name, ok := r.(string) // 타사 커넥터 스키마를 그대로 믿지 않는다 — 단언이 틀리면 stdio 서버가 죽는다
		if !ok {
			return &RelayError{Kind: "upstream", Detail: toolRef + ": 스키마 required 항목이 문자열이 아님"}
		}
		if _, ok := args[name]; !ok {
			return &RelayError{Kind: "constraint", Detail: toolRef + ": 필수 인자 누락 " + name}
		}
	}
	for k, v := range args {
		p, ok := props[k].(map[string]any)
		if !ok {
			return &RelayError{Kind: "constraint", Detail: toolRef + ": 스키마에 없는 인자 " + k}
		}
		if err := checkType(toolRef, k, p["type"], v); err != nil {
			return err
		}
	}
	return nil
}

func checkType(toolRef, key string, schemaType, value any) error {
	switch schemaType {
	case nil, "any":
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return typeErr(toolRef, key, "string")
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return typeErr(toolRef, key, "number")
		}
	case "integer":
		f, ok := value.(float64)
		if !ok || f != float64(int64(f)) {
			return typeErr(toolRef, key, "integer")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return typeErr(toolRef, key, "boolean")
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return typeErr(toolRef, key, "array")
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return typeErr(toolRef, key, "object")
		}
	}
	return nil
}

func typeErr(toolRef, key, want string) error {
	return &RelayError{Kind: "constraint", Detail: fmt.Sprintf("%s: 인자 %s는 %s여야 함", toolRef, key, want)}
}

// callUpstream — 검증 통과 후 실제 호출. 업스트림 content를 그대로 전달한다.
func callUpstream(c *conn, tool string, args map[string]any) (json.RawMessage, bool, error) {
	raw, cred, err := proxyCall(context.Background(), state.cred, c.id, "tools/call",
		map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return nil, false, err
	}
	if cred.AccessToken != state.cred.AccessToken {
		state.cred = cred // 사다리로 갱신된 토큰 반영
	}
	var res struct {
		Result struct {
			Content json.RawMessage `json:"content"`
			IsError bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, false, &RelayError{Kind: "upstream", Detail: "tools/call 응답 파싱 실패"}
	}
	return res.Result.Content, res.Result.IsError, nil
}

func toolSearchTools(args map[string]any) (map[string]any, bool, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return nil, false, usageErr("query는 필수")
	}
	query = strings.ToLower(query)
	detail, _ := args["detail"].(string)
	if detail == "" {
		detail = "names"
	}
	results := []map[string]any{}
	for _, slug := range connectorSlugs() {
		live := state.connectors[slug].live
		for _, tool := range sortedToolNames(live) { // live 맵 순회가 랜덤 — 결정적 출력 유지
			ref := slug + "." + tool
			if !strings.Contains(strings.ToLower(ref), query) { // 접두사까지 맞춘다 — "gmail"·"calendar" 질의(D8)
				continue
			}
			entry := map[string]any{"tool": ref, "kind": classifyTool(tool)}
			if detail == "descriptions" || detail == "schemas" {
				entry["description"] = live.descriptions[tool]
			}
			if detail == "schemas" {
				entry["inputSchema"] = live.schemas[tool]
			}
			results = append(results, entry)
		}
	}
	return map[string]any{"tools": results}, false, nil
}

// sortedToolNames — live 스키마 맵의 도구명 정렬 목록.
func sortedToolNames(live liveTools) []string {
	names := make([]string, 0, len(live.schemas))
	for n := range live.schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// sortedReadTools — live 도구 중 읽기로 분류된 것의 정렬 목록.
func sortedReadTools(live liveTools) []string {
	var out []string
	for _, t := range sortedToolNames(live) {
		if classifyTool(t) == "read" {
			out = append(out, t)
		}
	}
	return out
}

func toolCall(kind string, args map[string]any) (any, bool, error) {
	ref, _ := args["tool"].(string)
	if ref == "" {
		return nil, false, usageErr("tool는 필수")
	}
	callArgs, _ := args["args"].(map[string]any)
	if callArgs == nil {
		callArgs = map[string]any{}
	}
	c, tool, err := findTool(ref)
	if err != nil {
		return nil, false, err
	}
	if got := classifyTool(tool); got != kind {
		return nil, false, usageErr("%s는 %s 도구 — call_%s로 호출하세요", ref, got, got)
	}
	if err := validateArgs(ref, c.live.schemas[tool], callArgs); err != nil {
		return nil, false, err
	}
	content, isErr, err := callUpstream(c, tool, callArgs)
	if err != nil {
		return nil, false, err
	}
	return content, isErr, nil // 업스트림 content 배열을 그대로 전달 (본문 무손실)
}

// toolStatus — 발견을 다시 돌린 뒤, 아직 편입되지 않은 커넥터(실패했거나 새로 연결된 것)의 취득을
// 재시도한다. bootstrap 캐시는 토큰이 바뀌기 전까지 유지되므로 status가 유일한 회복 경로다(D8).
func toolStatus() (map[string]any, bool, error) {
	servers, cred, err := withAuthRetry(state.cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		return nil, false, err
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].DisplayName < servers[j].DisplayName })
	connected := map[string]bool{}
	for _, c := range state.connectors {
		connected[c.displayName] = true
	}
	for _, s := range servers {
		if connected[s.DisplayName] {
			continue
		}
		if cred, err = state.addServer(cred, s, fetchLiveTools); err != nil {
			return nil, false, err
		}
	}
	state.cred = cred
	connectors := []map[string]any{}
	for _, slug := range connectorSlugs() {
		c := state.connectors[slug]
		read, write := countKinds(c.live)
		connectors = append(connectors, map[string]any{
			"slug": slug, "display_name": c.displayName, "mcpsrv_id": c.id,
			"live_tools": len(c.live.schemas), "read": read, "write": write,
			"read_tools": sortedReadTools(c.live), // 호스트가 승인 없이 실행하는 집합 — 감사 경로(D8)
		})
	}
	unconnected := []map[string]any{}
	for _, s := range servers {
		if reason, failed := state.unconnected[s.DisplayName]; failed {
			unconnected = append(unconnected, map[string]any{"display_name": s.DisplayName, "error": reason})
		}
	}
	return map[string]any{
		"relay":       map[string]any{"token_account": state.cred.Account, "connectors": len(state.connectors)},
		"connectors":  connectors,
		"unconnected": unconnected,
		"discovered":  len(servers),
	}, false, nil
}

// countKinds — 커넥터 live 도구의 읽기/쓰기 개수.
func countKinds(live liveTools) (int, int) {
	read := 0
	for t := range live.schemas {
		if classifyTool(t) == "read" {
			read++
		}
	}
	return read, len(live.schemas) - read
}

// --- stdio JSON-RPC 루프 ---

func toolDescriptors() []map[string]any {
	argsSchema := func(props map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 { // 빈 required는 키를 생략 — 클라이언트 검증이 null/[]을 거부함
			schema["required"] = required
		}
		return schema
	}
	return []map[string]any{
		{"name": "search_tools", "description": "claude.ai 커넥터 live 도구 검색 (결과에 read/write 분류 포함)",
			"inputSchema": argsSchema(map[string]any{
				"query":  map[string]any{"type": "string", "description": "도구명·커넥터 접두사 부분일치 (대소문자 무시, '.' = 전체)"},
				"detail": map[string]any{"type": "string", "enum": []string{"names", "descriptions", "schemas"}, "description": "기본 names"},
			}, "query")},
		{"name": "call_read", "description": "읽기로 분류된 커넥터 도구 실행",
			"inputSchema": argsSchema(map[string]any{
				"tool": map[string]any{"type": "string", "description": "예: gmail.list_labels"},
				"args": map[string]any{"type": "object"},
			}, "tool")},
		{"name": "call_write", "description": "쓰기로 분류된 도구 실행 — 호스트 승인 대상",
			"inputSchema": argsSchema(map[string]any{
				"tool": map[string]any{"type": "string", "description": "예: google_calendar.create_event"},
				"args": map[string]any{"type": "object"},
			}, "tool")},
		{"name": "status", "description": "커넥터 상태·미연결 커넥터 목록. 실패했던 커넥터의 취득을 재시도하고 읽기로 분류된 도구명(read_tools)도 싣는다",
			"inputSchema": argsSchema(map[string]any{})},
	}
}

func serve() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if resp := dispatch(line); resp != nil {
			out.Write(resp)
			out.WriteByte('\n')
			out.Flush()
		}
	}
}

func dispatch(line []byte) []byte {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
			Protocol  string         `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return rpcError(nil, -32700, "parse error")
	}
	if len(msg.ID) == 0 {
		return nil // notification — 응답 없음
	}
	switch msg.Method {
	case "initialize":
		proto := msg.Params.Protocol
		if proto == "" {
			proto = "2024-11-05"
		}
		return rpcResult(msg.ID, map[string]any{
			"protocolVersion": proto, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "claude-ai-relay", "version": version},
		})
	case "ping":
		return rpcResult(msg.ID, map[string]any{})
	case "tools/list":
		return rpcResult(msg.ID, map[string]any{"tools": toolDescriptors()})
	case "tools/call":
		if err := bootstrap(); err != nil {
			return toolError(msg.ID, err)
		}
		var result any
		var isErr bool
		var err error
		switch msg.Params.Name {
		case "search_tools":
			result, isErr, err = toolSearchTools(msg.Params.Arguments)
		case "call_read":
			result, isErr, err = toolCall("read", msg.Params.Arguments)
		case "call_write":
			result, isErr, err = toolCall("write", msg.Params.Arguments)
		case "status":
			result, isErr, err = toolStatus()
		default:
			return rpcError(msg.ID, -32601, "unknown tool: "+msg.Params.Name)
		}
		if err != nil {
			return toolError(msg.ID, err)
		}
		if msg.Params.Name == "call_read" || msg.Params.Name == "call_write" {
			// 업스트림 content 배열을 MCP 결과 content로 그대로 실운다.
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{"content": json.RawMessage(result.(json.RawMessage)), "isError": isErr}})
			return b
		}
		return rpcResult(msg.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": marshalJSON(result)}},
			"isError": isErr,
		})
	default:
		return rpcError(msg.ID, -32601, "unknown method: "+msg.Method)
	}
}

// toolError — 도구 오류는 JSON-RPC error가 아니라 isError 결과다 (MCP 규격).
func toolError(id json.RawMessage, err error) []byte {
	return rpcResult(id, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": err.Error()}},
		"isError": true,
	})
}

func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal 실패"}`
	}
	return string(b)
}

func rpcResult(id json.RawMessage, result any) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return b
}

func rpcError(id json.RawMessage, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg}})
	return b
}

// --- 서브커맨드 ---

// cmdProbe — 토큰·발견·전 커넥터 live 취득·분류·읽기 호출을 한 번에 검증한다.
// bootstrap과 같은 편입 규칙(addServer)으로 스윕한다 — 커넥터별 실패는 흡수하고 경고만 출력하되(D7),
// slug 충돌이면 bootstrap처럼 실패한다(D8). 커넥터별 소요 시간과 읽기 분류 도구명을 함께 찍는다.
func cmdProbe() int {
	cred, err := loadToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	servers, cred, err := withAuthRetry(cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	fmt.Printf("relay=ok acct=%s discovered=%d\n", cred.Account, len(servers))
	st := &relayState{cred: cred, connectors: map[string]*conn{}, unconnected: map[string]string{}}
	gmailRead, gmailID := "", "" // gmail.list_labels 우선
	fallbackRead, fallbackID := "", ""
	for _, s := range servers {
		slug := slugify(s.DisplayName)
		started := time.Now()
		if cred, err = st.addServer(cred, s, fetchLiveTools); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		added, ok := st.connectors[slug]
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: tools/list 실패(흡수) — %s\n", s.DisplayName, st.unconnected[s.DisplayName])
			continue
		}
		read, write := countKinds(added.live)
		fmt.Printf("%s (%s): tools=%d read=%d write=%d %s\n", s.DisplayName, slug,
			len(added.live.schemas), read, write, time.Since(started).Round(time.Millisecond))
		reads := sortedReadTools(added.live)
		fmt.Printf("  읽기 분류: %s\n", strings.Join(reads, ", "))
		if s.DisplayName == "Gmail" {
			gmailID = s.ID
			for _, t := range reads {
				if t == "list_labels" || gmailRead == "" {
					gmailRead = t
					if t == "list_labels" {
						break
					}
				}
			}
		}
		if fallbackRead == "" && len(reads) > 0 {
			fallbackRead, fallbackID = reads[0], s.ID
		}
	}
	tool, id := gmailRead, gmailID
	if tool == "" {
		tool, id = fallbackRead, fallbackID
	}
	if tool == "" {
		fmt.Fprintln(os.Stderr, "읽기 도구가 하나도 없어 probe 종료")
		return 3
	}
	n, err := callEmptyArg(cred, id, tool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 실패: %v\n", tool, err)
		return 3
	}
	fmt.Printf("%s → ok count=%d\n", tool, n)
	return 0
}

// callEmptyArg — 빈 인자 tools/call 1건. 첫 text 블록 안 JSON의 첫 배열 길이를 돌려준다.
func callEmptyArg(cred credentials, mcpsrvID, tool string) (int, error) {
	raw, _, err := proxyCall(context.Background(), cred, mcpsrvID, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
	if err != nil {
		return 0, err
	}
	var res struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.Result.IsError || len(res.Result.Content) == 0 {
		return 0, &RelayError{Kind: "upstream", Detail: tool + " 응답 오류: " + string(raw[:min(200, len(raw))])}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(res.Result.Content[0].Text), &parsed); err != nil {
		return 0, nil // JSON이 아니면 개수 없이 성공만
	}
	for _, v := range parsed {
		if arr, ok := v.([]any); ok {
			return len(arr), nil
		}
	}
	return 0, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func assert(cond bool, msg string) error {
	if !cond {
		return fmt.Errorf("selfcheck 실패: %s", msg)
	}
	return nil
}

func cmdSelfcheck() int {
	// 키체인 파서: 다중 항목 + 서비스 불일치 항목
	dump := "keychain: \n\"acct\"<blob>=\"unknown\"\n\"svce\"<blob>=\"Claude Code-credentials\"\n" +
		"keychain: \n\"acct\"<blob>=\"keaton\"\n\"svce\"<blob>=\"Claude Code-credentials\"\n" +
		"keychain: \n\"acct\"<blob>=\"other\"\n\"svce\"<blob>=\"Claude Safe Storage\"\n"
	accts := parseKeychainDump(dump, keychainSvc)
	if err := assert(len(accts) == 2 && accts[0] == "unknown" && accts[1] == "keaton",
		fmt.Sprintf("키체인 파서 다중 항목: %v", accts)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// env 제거
	env := stripAuthEnv([]string{"ANTHROPIC_AUTH_TOKEN=x", "CLAUDE_CODE_USE_BEDROCK=1", "PATH=/usr/bin", "HOME=/u"})
	if err := assert(len(env) == 2, fmt.Sprintf("env 제거: %v", env)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// SSE/JSON 파서
	raw, err := parseSSEJSON([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\"}\n\n"))
	if err := assert(err == nil && strings.HasPrefix(string(raw), "{\"jsonrpc\""), "SSE 파서"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// 호출 규격 상수 (MEASURED-FACTS와 표면 대조)
	if err := assert(userAgent != "Go-http-client" && strings.Contains(proxyBase, "mcp-proxy.anthropic.com/v1/mcp/"),
		"호출 규격 상수"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	uuid := newUUID()
	if err := assert(len(uuid) == 36 && uuid[14] == '4', "uuid v4 형식"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// 이름 규약: slug (D7)
	for _, c := range [][2]string{
		{"Gmail", "gmail"}, {"Google Calendar", "google_calendar"},
		{"Zoom for Claude", "zoom_for_claude"}, {"Atlassian Rovo", "atlassian_rovo"},
		{"Claude Data", "claude_data"}, {"A  --B!", "a_b"},
	} {
		if err := assert(slugify(c[0]) == c[1], fmt.Sprintf("slug %s → %s (실제 %s)", c[0], c[1], slugify(c[0]))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	// 이름 규약: 읽기/쓰기 분류 경계 사례
	for _, t := range []string{"list_events", "search_threads", "read_doc", "download_file_content",
		"whoami", "capabilities_discover", "get_event", "list_labels", "get_certified_query_source",
		"getHTTPResponse"} {
		if err := assert(classifyTool(t) == "read", "분류 읽기: "+t+" (실제 "+classifyTool(t)+")"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	for _, t := range []string{"unlabel_message", "suggest_time", "export_artifact", "set_status",
		"update_target", "follow_meeting_transcript", "atlassianUserInfo", "certify_query",
		"uncertify_query", "set_certified_query_source", "find_or_create_contact", "search_and_replace",
		"capabilities_invoke_read", "setHTTPCode"} {
		if err := assert(classifyTool(t) == "write", "분류 쓰기: "+t+" (실제 "+classifyTool(t)+")"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	}
	// findTool — 합성 state로 오프라인 검증
	state = &relayState{cred: credentials{AccessToken: "t"},
		connectors: map[string]*conn{
			"gmail": {displayName: "Gmail", id: "mcpsrv_g", live: liveTools{
				schemas:      map[string]map[string]any{"list_labels": {}, "get_event": {}},
				descriptions: map[string]string{}}},
			"gcal": {displayName: "Google Calendar", id: "mcpsrv_c", live: liveTools{
				schemas:      map[string]map[string]any{"get_event": {}, "list_events": {}},
				descriptions: map[string]string{}}},
		}, unconnected: map[string]string{}}
	defer func() { state = nil }()
	if c, tool, err := findTool("gmail.list_labels"); err != nil || tool != "list_labels" || c.id != "mcpsrv_g" {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: findTool 정확 지정")
		return 2
	}
	if _, _, err := findTool("nosuch.tool"); err == nil || !strings.Contains(err.Error(), "연결됨") {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: 알 수 없는 커넥터 감지")
		return 2
	}
	if _, _, err := findTool("gmail.nothing"); err == nil {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: 없는 도구 감지")
		return 2
	}
	if c, tool, err := findTool("list_labels"); err != nil || tool != "list_labels" || c.id != "mcpsrv_g" {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: bare 이름 유일 매칭")
		return 2
	}
	if _, _, err := findTool("get_event"); err == nil || !strings.Contains(err.Error(), "여러 커넥터") {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: bare 이름 중복 감지")
		return 2
	}
	if _, _, err := toolCall("write", map[string]any{"tool": "gmail.list_labels", "args": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "call_read로") {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: kind 불일치 안내")
		return 2
	}
	if read, write := countKinds(state.connectors["gmail"].live); read != 2 || write != 0 {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("selfcheck 실패: countKinds read=%d write=%d", read, write))
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "selfcheck 실패: "+format+"\n", a...)
		return 2
	}
	// search_tools — 합성 state로 결정적 정렬·kind·접두사 질의·빈 결과 검증 (D8)
	search := func(args map[string]any) ([]map[string]any, error) {
		res, _, err := toolSearchTools(args)
		tools, _ := res["tools"].([]map[string]any)
		return tools, err
	}
	if got, _ := search(map[string]any{"query": "list"}); len(got) != 2 || got[0]["tool"] != "gcal.list_events" ||
		got[1]["tool"] != "gmail.list_labels" || got[0]["kind"] != "read" {
		return fail("search_tools 정렬·kind: %v", got)
	}
	if got, _ := search(map[string]any{"query": "GMAIL"}); len(got) != 2 || got[0]["tool"] != "gmail.get_event" ||
		got[1]["tool"] != "gmail.list_labels" {
		return fail("search_tools 접두사 질의: %v", got)
	}
	if got, _ := search(map[string]any{"query": "zzz"}); got == nil || len(got) != 0 {
		return fail("search_tools 빈 결과는 null이 아니라 []: %v", got)
	}
	if _, err := search(map[string]any{}); err == nil {
		return fail("search_tools query 누락 감지")
	}
	// addServer — 편입 규칙: 성공·실패 흡수·빈 slug·slug 충돌(전역 실패)·재시도 회복 (D7·D8)
	fake := func(failFetch bool, names ...string) fetchLive {
		return func(_ context.Context, cred credentials, _ string) (liveTools, credentials, error) {
			if failFetch {
				return liveTools{}, cred, &RelayError{Kind: "auth", Detail: "401"}
			}
			lt := liveTools{schemas: map[string]map[string]any{}, descriptions: map[string]string{}}
			for _, n := range names {
				lt.schemas[n] = map[string]any{}
			}
			return lt, cred, nil
		}
	}
	st := &relayState{connectors: map[string]*conn{}, unconnected: map[string]string{}}
	cred, aerr := st.addServer(credentials{AccessToken: "t"}, serverEntry{ID: "g", DisplayName: "Gmail"}, fake(false, "list_labels"))
	if aerr != nil || st.connectors["gmail"] == nil || len(st.unconnected) != 0 {
		return fail("addServer 성공 편입: %v %v", aerr, st.unconnected)
	}
	if _, aerr = st.addServer(cred, serverEntry{ID: "z", DisplayName: "Zapier"}, fake(true)); aerr != nil ||
		st.connectors["zapier"] != nil || !strings.Contains(st.unconnected["Zapier"], "auth: 401") {
		return fail("addServer 실패 흡수: %v %v", aerr, st.unconnected)
	}
	if _, aerr = st.addServer(cred, serverEntry{ID: "x", DisplayName: "!!!"}, fake(false)); aerr != nil ||
		!strings.Contains(st.unconnected["!!!"], "slug") {
		return fail("addServer 빈 slug 흡수: %v %v", aerr, st.unconnected)
	}
	if _, aerr = st.addServer(cred, serverEntry{ID: "c", DisplayName: "Google Calendar"}, fake(false, "list_events")); aerr != nil {
		return fail("addServer slug 정상 편입: %v", aerr)
	}
	if _, aerr = st.addServer(cred, serverEntry{ID: "c2", DisplayName: "Google, Calendar"}, fake(false, "list_events")); aerr == nil ||
		!strings.Contains(aerr.Error(), "slug 충돌") || st.connectors["google_calendar"].id != "c" {
		return fail("addServer slug 충돌은 전역 실패: %v", aerr)
	}
	if _, aerr = st.addServer(cred, serverEntry{ID: "z", DisplayName: "Zapier"}, fake(false, "list_events")); aerr != nil ||
		st.connectors["zapier"] == nil || len(st.unconnected) != 1 {
		return fail("addServer 재시도 회복: %v %v", aerr, st.unconnected)
	}
	// withSweepRetry — 같은 토큰이면 재시도 없이 업스트림 오류를, 바뀐 토큰이면 1회 재시도 (D8)
	sweepCalls := 0
	sweep401 := func(credentials) (int, error) { sweepCalls++; return 0, &RelayError{Kind: "auth", Detail: "401"} }
	reread := func(tok string) func() (credentials, error) {
		return func() (credentials, error) { return credentials{AccessToken: tok}, nil }
	}
	stale := credentials{AccessToken: "t"}
	if _, _, serr := withSweepRetry(stale, reread("t"), sweep401); serr == nil || sweepCalls != 1 {
		return fail("withSweepRetry 같은 토큰에는 재시도하지 않는다 (calls=%d, err=%v)", sweepCalls, serr)
	}
	sweepCalls = 0
	_, got, serr := withSweepRetry(stale, reread("t2"), func(c credentials) (int, error) {
		sweepCalls++
		if c.AccessToken != "t2" {
			return 0, &RelayError{Kind: "auth", Detail: "옛 토큰"}
		}
		return 7, nil
	})
	if serr != nil || sweepCalls != 2 || got.AccessToken != "t2" {
		return fail("withSweepRetry 바뀐 토큰으로 1회 재시도 (calls=%d, err=%v)", sweepCalls, serr)
	}
	// validateArgs — required가 문자열이 아닌 타사 스키마는 거부한다 (단언 패닉 방지)
	if verr := validateArgs("x.y", map[string]any{"properties": map[string]any{}, "required": []any{1}},
		map[string]any{}); verr == nil || !strings.Contains(verr.Error(), "문자열이 아님") {
		return fail("validateArgs 비문자열 required: %v", verr)
	}
	fmt.Println("selfcheck=ok (키체인 파서·env 제거·SSE 파서·규격 상수·uuid·slug·분류·findTool·kind 안내·countKinds·search_tools·addServer·withSweepRetry·validateArgs)")
	return 0
}

func main() {
	if len(os.Args) < 2 {
		serve() // 무인자 실행 = stdio MCP 서버 (claude mcp add 용도)
		return
	}
	var code int
	switch os.Args[1] {
	case "--no-cli-refresh":
		noCLIRefresh = true
		serve()
		return
	case "probe":
		code = cmdProbe()
	case "selfcheck":
		code = cmdSelfcheck()
	default:
		fmt.Fprintln(os.Stderr, "알 수 없는 서브커맨드: "+os.Args[1]+" (probe | selfcheck | 무인자=serve)")
		code = 2
	}
	os.Exit(code)
}
