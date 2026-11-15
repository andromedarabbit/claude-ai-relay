// claude-ai-relay — claude.ai 커넥터(Gmail, Calendar 등)를 인증 불문 세션에 노출하는 relay.
//
// 전송: 키체인 claude.ai OAuth 토큰으로 api.anthropic.com/v1/mcp_servers(발견)와
// mcp-proxy.anthropic.com/v1/mcp/{mcpsrv_id}(도구 호출)에 직접 JSON-RPC.
// 호출 규격 근거: docs/MEASURED-FACTS.md (헤더·UA·키체인 다중 항목·회전 정책 전부 실측).
//
// 토큰 갱신은 CLI 트리거만 한다 — 직접 refresh는 회전으로 구 at·rt를 즉시
// revoke해 CLI 자격증명 체인을 파괴한다(2026-11-15 실측).
//
// 서브커맨드 (v0.1 단위 1 — serve·선언·도구 표면은 후속 단위):
//
//	claude-ai-relay probe       list_labels 1건으로 relay 사용 가능 여부 확인
//	claude-ai-relay selfcheck   오프라인 assert (네트워크·키체인 불필요)
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	userAgent = "claude-cli/2.1.0 (external, cli)" // shortcut: Cloudflare 1010 차단 시 이 상수만 갱신

	discoveryURL   = "https://api.anthropic.com/v1/mcp_servers"
	discoveryBeta  = "mcp-servers-2025-12-04"
	anthropicVer   = "2023-06-01"
	proxyBase      = "https://mcp-proxy.anthropic.com/v1/mcp/"
	keychainSvc    = "Claude Code-credentials"
	requestTimeout = 30 * time.Second
)

// RelayError — kind: usage | auth | upstream | transport.
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

// proxyCall — 프록시 MCP 엔드포인트로 JSON-RPC 1건. 401이면 갱신 사다리 후 1회 재시도.
func proxyCall(cred credentials, mcpsrvID, method string, params any) (json.RawMessage, credentials, error) {
	return withAuthRetry(cred, func(c credentials) (json.RawMessage, error) {
		return proxyCallOnce(c, mcpsrvID, method, params)
	})
}

// proxySessions — 커넥터별 프록시 세션 (mcpsrvID → uuid). 일부 업스트림(Slack 등)은
// initialize로 수립된 세션의 tools/call만 받는다. 서버 루프가 단일 스레드라 잠금 불필요.
var proxySessions = map[string]string{}

var initParams = map[string]any{
	"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
	"clientInfo": map[string]any{"name": "claude-ai-relay", "version": "v0.2"},
}

func proxyCallOnce(cred credentials, mcpsrvID, method string, params any) (json.RawMessage, error) {
	sess, known := proxySessions[mcpsrvID]
	if !known {
		sess = newUUID()
		if _, err := rawProxy(cred, mcpsrvID, sess, "initialize", initParams); err != nil {
			return nil, err
		}
		proxySessions[mcpsrvID] = sess
	}
	raw, err := rawProxy(cred, mcpsrvID, sess, method, params)
	if err == nil {
		return raw, nil
	}
	// 세션 무효화(만료 등)면 1회 재수립 후 재시도
	if known && strings.Contains(err.Error(), "initialize request") {
		delete(proxySessions, mcpsrvID)
		return proxyCallOnce(cred, mcpsrvID, method, params)
	}
	return nil, err
}

func rawProxy(cred credentials, mcpsrvID, sess, method string, params any) (json.RawMessage, error) {
	payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", proxyBase+mcpsrvID, bytes.NewReader(payload))
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

// ensureToken — 401 사다리: ① 키체인 재독(외부 갱신 흡수) ② CLI 트리거 갱신 ③ 재독 → 실패 시 auth 오류.
func ensureToken(stale credentials) (credentials, error) {
	if cred, err := loadToken(); err == nil && cred.AccessToken != stale.AccessToken {
		return cred, nil
	}
	if err := refreshViaCLI(); err != nil {
		return credentials{}, &RelayError{Kind: "auth", Detail: "CLI 갱신 실패: " + err.Error() + " — claude login 후 재시도"}
	}
	if cred, err := loadToken(); err == nil {
		return cred, nil
	}
	return credentials{}, &RelayError{Kind: "auth", Detail: "갱신 후에도 토큰 없음 — claude login 후 재시도"}
}

// --- 선언 (D1: 커넥터 추가 = connectors/*.json 추가) ---

//go:embed connectors/*.json
var declarationFS embed.FS

type WriteConstraint struct {
	Fixed map[string]any    `json:"fixed"` // 값과 완전일치해야 하는 인자
	Types map[string]string `json:"types"` // 타입만 검사하는 인자 ("string")
}

type Declaration struct {
	Name  string                     `json:"name"`
	Match string                     `json:"match"` // 발견 API의 display_name과 대조
	Read  []string                   `json:"read"`
	Write map[string]WriteConstraint `json:"write"`
}

// parseDeclaration — 선언 1건 구조 검증 후 적재. 같은 name이면 나중 것이 덮어쓴다(스캔 오버레이용).
func parseDeclaration(source string, raw []byte, decls map[string]Declaration) error {
	var d Declaration
	if err := json.Unmarshal(raw, &d); err != nil {
		return usageErr("%s 파싱 실패: %v", source, err)
	}
	if d.Name == "" || d.Match == "" {
		return usageErr("%s: name과 match는 필수", source)
	}
	for _, t := range d.Read {
		if _, dup := d.Write[t]; dup {
			return usageErr("%s: %s가 read·write 양쪽에 선언됨", source, t)
		}
	}
	if _, dup := decls[d.Name]; dup {
		fmt.Fprintf(os.Stderr, "relay: 선언 %s를 %s이(가) 덮어씀\n", d.Name, source)
	}
	decls[d.Name] = d
	return nil
}

// loadDeclarations — 내장 선언 + ~/.config/claude-ai-relay/connectors/*.json 오버레이.
// 재빌드 없이 선언 추가·수정을 허용한다 (규약 고정 경로, 설정 변수 없음).
func loadDeclarations() (map[string]Declaration, error) {
	entries, err := declarationFS.ReadDir("connectors")
	if err != nil {
		return nil, usageErr("connectors 디렉터리 읽기 실패: %v", err)
	}
	decls := map[string]Declaration{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := declarationFS.ReadFile("connectors/" + e.Name())
		if err != nil {
			return nil, usageErr("%s 읽기 실패: %v", e.Name(), err)
		}
		if err := parseDeclaration("내장:"+e.Name(), raw, decls); err != nil {
			return nil, err
		}
	}
	// 스캔 경로는 ~/.config/claude-ai-relay/connectors 로 규약 고정 (플랫폼 무관 단일 경로)
	home, herr := os.UserHomeDir()
	cfgRoot := os.Getenv("XDG_CONFIG_HOME")
	if cfgRoot == "" && herr == nil {
		cfgRoot = filepath.Join(home, ".config")
	}
	if cfgRoot != "" {
		scan := filepath.Join(cfgRoot, "claude-ai-relay", "connectors")
		if files, err := os.ReadDir(scan); err == nil {
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(scan, f.Name()))
				if err != nil {
					return nil, usageErr("%s 읽기 실패: %v", f.Name(), err)
				}
				if err := parseDeclaration("스캔:"+f.Name(), raw, decls); err != nil {
					return nil, err // 스캔 선언이 깨지면 fail fast — 조용히 무시하지 않는다
				}
			}
		}
	}
	if len(decls) == 0 {
		return nil, usageErr("선언이 하나도 없음")
	}
	return decls, nil
}

// matchConnectors — 선언 ↔ 발견 매핑. 선언된 커넥터가 발견 목록에 없으면 실패(미연결).
func matchConnectors(servers []serverEntry, decls map[string]Declaration) (map[string]serverEntry, error) {
	matched := map[string]serverEntry{}
	for name, d := range decls {
		found := serverEntry{}
		for _, s := range servers {
			if s.DisplayName == d.Match {
				found = s
				break
			}
		}
		if found.ID == "" {
			return nil, &RelayError{Kind: "auth",
				Detail: "선언된 커넥터가 발견 목록에 없음: " + d.Match + " — claude.ai에서 커넥터를 연결하거나 선언에서 제거"}
		}
		matched[name] = found
	}
	return matched, nil
}

// liveTools — tools/list 결과. 스키마 대조(D5)와 search_tools 상세 표시의 원천.
type liveTools struct {
	schemas      map[string]map[string]any // tool name → inputSchema
	descriptions map[string]string         // tool name → description
}

func fetchLiveTools(cred credentials, mcpsrvID string) (liveTools, error) {
	raw, _, err := proxyCall(cred, mcpsrvID, "tools/list", map[string]any{})
	if err != nil {
		return liveTools{}, err
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
		return liveTools{}, &RelayError{Kind: "upstream", Detail: "tools/list 파싱 실패: " + err.Error()}
	}
	lt := liveTools{schemas: map[string]map[string]any{}, descriptions: map[string]string{}}
	for _, t := range res.Result.Tools {
		lt.schemas[t.Name] = t.InputSchema
		lt.descriptions[t.Name] = t.Description
	}
	return lt, nil
}

// compareDecl — 선언-실제 불일치 목록. 업스트림에 추가 도구가 있는 건 불일치가 아니다(미선언 표면).
func compareDecl(d Declaration, live liveTools) []string {
	var mismatches []string
	for _, t := range d.Read {
		if _, ok := live.schemas[t]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s.%s: 읽기로 선언됐으나 업스트림에 없음", d.Name, t))
		}
	}
	for t := range d.Write {
		if _, ok := live.schemas[t]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("%s.%s: 쓰기로 선언됐으나 업스트림에 없음", d.Name, t))
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

// --- stdio MCP 서버 (도구 표면 4개, D3·D4) ---

type relayState struct {
	cred       credentials
	decls      map[string]Declaration
	matched    map[string]serverEntry
	live       map[string]liveTools // decl name → live
	liveToken  string               // 캐시 시점의 토큰 — 바뀌면 재취득
	mismatches map[string][]string  // decl name → 불일치 (경보, 치명 아님)
}

var state *relayState

// bootstrap — 첫 상위 호출 시 1회 (initialize를 지연시키지 않는다).
func bootstrap() error {
	if state != nil && state.liveToken == state.cred.AccessToken {
		return nil
	}
	cred, err := loadToken()
	if err != nil {
		return err
	}
	decls, err := loadDeclarations()
	if err != nil {
		return err
	}
	servers, cred, err := withAuthRetry(cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		return err
	}
	matched, err := matchConnectors(servers, decls)
	if err != nil {
		return err
	}
	st := &relayState{cred: cred, decls: decls, matched: matched,
		live: map[string]liveTools{}, mismatches: map[string][]string{}}
	for name, d := range decls {
		live, err := fetchLiveTools(cred, matched[name].ID)
		if err != nil {
			return err
		}
		st.live[name] = live
		st.mismatches[name] = compareDecl(d, live)
	}
	st.liveToken = cred.AccessToken
	state = st
	return nil
}

// findTool — "gmail.search_threads" 또는 bare 이름(전 선언에서 유일할 때만).
func findTool(ref string) (Declaration, string, string, error) {
	if decl, tool, ok := strings.Cut(ref, "."); ok {
		d, exists := state.decls[decl]
		if !exists {
			return Declaration{}, "", "", usageErr("알 수 없는 커넥터: %s (선언됨: %v)", decl, declNames())
		}
		if sliceContains(d.Read, tool) {
			return d, tool, "read", nil
		}
		if _, ok := d.Write[tool]; ok {
			return d, tool, "write", nil
		}
		return Declaration{}, "", "", &RelayError{Kind: "not_declared",
			Detail: fmt.Sprintf("%s는 %s 선언에 없음", tool, decl)}
	}
	var hits []string
	for _, name := range declNames() {
		d := state.decls[name]
		if sliceContains(d.Read, ref) {
			hits = append(hits, name+"."+ref)
		}
		if _, ok := d.Write[ref]; ok {
			hits = append(hits, name+"."+ref)
		}
	}
	switch len(hits) {
	case 1:
		return findTool(hits[0])
	case 0:
		return Declaration{}, "", "", &RelayError{Kind: "not_declared", Detail: "선언되지 않은 도구: " + ref}
	default:
		return Declaration{}, "", "", usageErr("%s는 여러 선언에 있음 — 커넥터를 붙여 지정: %v", ref, hits)
	}
}

func declNames() []string {
	names := make([]string, 0, len(state.decls))
	for n := range state.decls {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sliceContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validateArgs — required 존재 + 타입 일치 + 스키마에 없는 키 거부.
// shortcut: enum/format 등 고급 스키마는 검사 안 함 — 필요해지면 그때.
func validateArgs(toolRef string, schema map[string]any, args map[string]any) error {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return nil // 업스트림이 스키마를 안 주면 통과 (대조 경보가 이미 표시)
	}
	required, _ := schema["required"].([]any)
	for _, r := range required {
		if _, ok := args[r.(string)]; !ok {
			return &RelayError{Kind: "constraint", Detail: toolRef + ": 필수 인자 누락 " + r.(string)}
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

// applyWriteConstraint — fixed 키는 호출자가 생략하면 주입, 다른 값을 실으면 거부.
// types 키는 타입만 검사. 미선언 키는 validateArgs가 거부한다.
func applyWriteConstraint(toolRef string, wc WriteConstraint, args map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(args)+len(wc.Fixed))
	for k, v := range args {
		out[k] = v
	}
	for k, want := range wc.Fixed {
		if got, ok := args[k]; ok && !reflect.DeepEqual(got, want) {
			return nil, &RelayError{Kind: "constraint",
				Detail: fmt.Sprintf("%s: 인자 %s는 고정값 %s여야 함 (쓰기 제약)", toolRef, k, fmt.Sprint(want))}
		}
		out[k] = want
	}
	for k, want := range wc.Types {
		if err := checkType(toolRef, k, want, args[k]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// callUpstream — 검증 통과 후 실제 호출. 업스트림 content를 그대로 전달한다.
func callUpstream(d Declaration, tool string, args map[string]any) (json.RawMessage, bool, error) {
	if _, ok := state.live[d.Name].schemas[tool]; !ok {
		return nil, false, &RelayError{Kind: "not_declared",
			Detail: fmt.Sprintf("%s.%s: 업스트림에 없음(선언-실제 불일치)", d.Name, tool)}
	}
	raw, cred, err := proxyCall(state.cred, state.matched[d.Name].ID, "tools/call",
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
	detail, _ := args["detail"].(string)
	if detail == "" {
		detail = "names"
	}
	var results []map[string]any
	for _, name := range declNames() {
		d := state.decls[name]
		live := state.live[name]
		check := func(tool string) {
			if !strings.Contains(tool, query) {
				return
			}
			entry := map[string]any{"tool": name + "." + tool}
			if detail == "descriptions" || detail == "schemas" {
				entry["description"] = live.descriptions[tool]
			}
			if detail == "schemas" {
				entry["inputSchema"] = live.schemas[tool]
			}
			results = append(results, entry)
		}
		for _, t := range d.Read {
			check(t)
		}
		for t := range d.Write {
			check(t)
		}
	}
	return map[string]any{"tools": results}, false, nil
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
	d, tool, gotKind, err := findTool(ref)
	if err != nil {
		return nil, false, err
	}
	if gotKind != kind {
		return nil, false, usageErr("%s는 %s 도구 — call_%s로 호출하세요", ref, gotKind, gotKind)
	}
	if kind == "write" {
		callArgs, err = applyWriteConstraint(ref, d.Write[tool], callArgs)
		if err != nil {
			return nil, false, err
		}
	}
	if err := validateArgs(ref, state.live[d.Name].schemas[tool], callArgs); err != nil {
		return nil, false, err
	}
	content, isErr, err := callUpstream(d, tool, callArgs)
	if err != nil {
		return nil, false, err
	}
	return content, isErr, nil // 업스트림 content 배열을 그대로 전달 (본문 무손실)
}

func toolStatus() (map[string]any, bool, error) {
	servers, _, err := withAuthRetry(state.cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		return nil, false, err
	}
	declared := map[string]bool{}
	var connectors []map[string]any
	for _, name := range declNames() {
		d := state.decls[name]
		declared[state.matched[name].DisplayName] = true
		connectors = append(connectors, map[string]any{
			"name": name, "match": d.Match, "mcpsrv_id": state.matched[name].ID,
			"read": len(d.Read), "write": len(d.Write),
			"live_tools": len(state.live[name].schemas),
			"mismatches": state.mismatches[name],
		})
	}
	var undeclared []string
	for _, s := range servers {
		if !declared[s.DisplayName] {
			undeclared = append(undeclared, s.DisplayName)
		}
	}
	sort.Strings(undeclared)
	return map[string]any{
		"relay":      map[string]any{"token_account": state.cred.Account, "declared": len(state.decls)},
		"connectors": connectors,
		"undeclared": undeclared,
		"discovered": len(servers),
	}, false, nil
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
		{"name": "search_tools", "description": "선언된 claude.ai 커넥터 도구 검색",
			"inputSchema": argsSchema(map[string]any{
				"query":  map[string]any{"type": "string", "description": "도구명 부분일치"},
				"detail": map[string]any{"type": "string", "enum": []string{"names", "descriptions", "schemas"}, "description": "기본 names"},
			}, "query")},
		{"name": "call_read", "description": "읽기로 선언된 커넥터 도구 실행",
			"inputSchema": argsSchema(map[string]any{
				"tool": map[string]any{"type": "string", "description": "예: gmail.list_labels"},
				"args": map[string]any{"type": "object"},
			}, "tool")},
		{"name": "call_write", "description": "쓰기로 선언된 도구 실행 — 인자 제약을 relay가 사전 검증",
			"inputSchema": argsSchema(map[string]any{
				"tool": map[string]any{"type": "string", "description": "예: gmail.unlabel_message"},
				"args": map[string]any{"type": "object"},
			}, "tool")},
		{"name": "status", "description": "커넥터 상태·선언 대조·미선언 커넥터 목록",
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
			"serverInfo": map[string]any{"name": "claude-ai-relay", "version": "0.1.0"},
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

// cmdProbe — 단위 1·2 종료 기준 검증: 토큰·발견·선언 매핑·혼합 호출·스키마 대조를 한 번에.
func cmdProbe() int {
	cred, err := loadToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	decls, err := loadDeclarations()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	servers, cred, err := withAuthRetry(cred, func(c credentials) ([]serverEntry, error) {
		return discoverServers(c.AccessToken)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	matched, err := matchConnectors(servers, decls)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	fmt.Printf("relay=ok acct=%s connectors=%d\n", cred.Account, len(matched))
	failed := false
	for _, name := range []string{"gmail", "gcal"} {
		d, ok := decls[name]
		if !ok {
			continue
		}
		entry := matched[name]
		live, err := fetchLiveTools(cred, entry.ID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s tools/list 실패: %v\n", name, err)
			failed = true
			continue
		}
		mismatches := compareDecl(d, live)
		fmt.Printf("%s: tools=%d mismatches=%d\n", entry.DisplayName, len(live.schemas), len(mismatches))
		for _, m := range mismatches {
			fmt.Println("  " + m)
		}
	}
	// 혼합 호출: 각 커넥터에서 빈 인자 읽기 도구 1건
	for _, call := range []struct{ decl, tool string }{
		{"gmail", "list_labels"}, {"gcal", "list_calendars"},
	} {
		n, err := callEmptyArg(cred, matched[call.decl].ID, call.tool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s 실패: %v\n", call.decl, call.tool, err)
			failed = true
			continue
		}
		fmt.Printf("%s.%s → ok count=%d\n", call.decl, call.tool, n)
	}
	if failed {
		return 3
	}
	return 0
}

// callEmptyArg — 빈 인자 tools/call 1건. 첫 text 블록 안 JSON의 첫 배열 길이를 돌려준다.
func callEmptyArg(cred credentials, mcpsrvID, tool string) (int, error) {
	raw, _, err := proxyCall(cred, mcpsrvID, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
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
	// 내장 선언 구조 (커넥터 수는 선언 추가에 따라 변함 — 핵심 2건 존재만 단정)
	decls, err := loadDeclarations()
	if err := assert(err == nil && len(decls) >= 2, fmt.Sprintf("선언 로드: %v", err)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	gmail := decls["gmail"]
	gcal, gcalOK := decls["gcal"]
	if err := assert(gcalOK && len(gmail.Read) == 4 && gmail.Write["unlabel_message"].Fixed["labelIds"] != nil && len(gcal.Read) == 4,
		"핵심 선언 내용"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// 쓰기 제약: fixed 주입 + 위반 거부
	injected, err := applyWriteConstraint("gmail.unlabel_message",
		gmail.Write["unlabel_message"], map[string]any{"messageId": "m1"})
	if err := assert(err == nil && reflect.DeepEqual(injected["labelIds"], []any{"INBOX"}), "fixed 주입"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if _, err := applyWriteConstraint("gmail.unlabel_message",
		gmail.Write["unlabel_message"], map[string]any{"messageId": "m1", "labelIds": []any{"STARRED"}}); err == nil {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: fixed 위반을 잡지 못함")
		return 2
	}
	// 선언-실제 대조: 없는 도구 감지
	mismatches := compareDecl(
		Declaration{Name: "x", Read: []string{"nonexistent", "list_labels"}},
		liveTools{schemas: map[string]map[string]any{"list_labels": {}}},
	)
	if err := assert(len(mismatches) == 1 && strings.Contains(mismatches[0], "nonexistent"),
		fmt.Sprintf("대조 감지: %v", mismatches)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// 선언-발견 매핑: 미발견 커넥터 감지
	if _, err := matchConnectors([]serverEntry{{ID: "mcpsrv_1", DisplayName: "Gmail"}},
		map[string]Declaration{"gmail": gmail}); err != nil {
		fmt.Fprintln(os.Stderr, "매핑 성공 케이스 실패: "+err.Error())
		return 2
	}
	if _, err := matchConnectors([]serverEntry{}, map[string]Declaration{"gmail": gmail}); err == nil {
		fmt.Fprintln(os.Stderr, "selfcheck 실패: 미발견 커넥터를 잡지 못함")
		return 2
	}
	fmt.Println("selfcheck=ok (키체인 파서·env 제거·SSE 파서·규격 상수·uuid·선언 로드·쓰기 제약 주입·대조·매핑)")
	return 0
}

func main() {
	if len(os.Args) < 2 {
		serve() // 무인자 실행 = stdio MCP 서버 (claude mcp add 용도)
		return
	}
	var code int
	switch os.Args[1] {
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
