package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"litepanel/internal/api"
	"litepanel/internal/settings"
)

// 设置接口直接跑在真实 settings.Store 上（不 mock）：这张表与 auth 共用，
// password_hash 就躺在同一张 settings 表里。用假 store 测"不泄露"永远为真，
// 只有真表 + 真密码才能证明注册表白名单真的挡住了它。

func newSetEnv(t *testing.T, apply api.SettingsApply) *pwEnv {
	t.Helper()
	return newPWEnvWith(t, "initial-pass-123", func(d *api.AuthDeps) {
		d.Settings = settings.NewStore(d.DB, nil, nil)
		d.SettingsApply = apply
		// 设置接口受"必须先改初始密码"的闸门管（那是中间件对**所有**非白名单
		// 路径的正确行为，见 TestMustChangePasswordBlocksOtherAPI）。这里要测
		// 的是闸门之后的事，所以直接清掉标记而不是每次改密。
		if _, err := d.DB.SqlDB().Exec(
			`UPDATE settings SET value='0' WHERE key='must_change_password'`,
		); err != nil {
			t.Fatalf("清除强制改密标记失败: %v", err)
		}
	})
}

// findItem 在 GET 响应里按 key 找单项。
func findItem(t *testing.T, body map[string]any, key string) (map[string]any, bool) {
	t.Helper()
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		items, _ := gm["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["key"] == key {
				return m, true
			}
		}
	}
	return nil, false
}

// GET 只能暴露注册表里的键。
//
// 这条测试故意**不**列"哪些键不能出现"，而是断言"出现的键必须全在注册表
// 里"。因为 settings 表里躺着的不止 password_hash —— 还有 must_change_password
// 等等（auth 与设置共用一张表），而将来任何模块都可能往这张表里塞私有行。
// 按黑名单写测试的话，每多一个敏感行就得记得回来加一条，忘了就是一次泄露；
// 反过来断言"白名单之外无所有"，新增的行默认就是泄不了的。
func TestSettingsGetOnlyExposesRegistryKeys(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	// 表里确实有非注册的行（auth 写的），否则这条测试是空转。
	var n int
	if err := e.db.SqlDB().QueryRow(
		`SELECT count(*) FROM settings WHERE key NOT IN (` +
			quoteList(settings.Defs()) + `)`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("前置条件失败：settings 表里没有非注册的键，这条什么都没测")
	}
	code, body := e.do("GET", "/api/settings", "")
	if code != http.StatusOK {
		t.Fatalf("got %d %+v", code, body)
	}
	allowed := map[string]bool{}
	for _, d := range settings.Defs() {
		allowed[string(d.Key)] = true
	}
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		items, _ := gm["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			k, _ := m["key"].(string)
			if !allowed[k] {
				t.Errorf("暴露了未注册的设置项 %q", k)
			}
		}
	}
	// 兜底：连字符串都不该出现（防止哪天有人把整行原样塞进某个字段）。
	raw, _ := json.Marshal(body)
	for _, secret := range []string{"password_hash", "must_change_password", "$2"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("响应里出现了 %q", secret)
		}
	}
}

// quoteList 把注册表键拼成 SQL IN 列表（只用于测试里数非注册行）。
func quoteList(defs []settings.Def) string {
	parts := make([]string, 0, len(defs))
	for _, d := range defs {
		parts = append(parts, "'"+string(d.Key)+"'")
	}
	return strings.Join(parts, ",")
}

// GET 的密钥项只暴露 has_value，绝不回显原值。
func TestSettingsGetMasksSecret(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	// 先真设一个密钥。
	code, body := e.do("PUT", "/api/settings", `{"aria2_rpc_secret":"s3cr3t"}`)
	if code != http.StatusOK {
		t.Fatalf("设置密钥应成功: %d %+v", code, body)
	}
	// PUT 的响应同样不得带出密钥（它就是刚刚提交的那个值）。
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "s3cr3t") {
		t.Error("PUT 响应回显了密钥")
	}
	code, body = e.do("GET", "/api/settings", "")
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	item, ok := findItem(t, body, "aria2_rpc_secret")
	if !ok {
		t.Fatal("响应里应能**看到**这项（只是值被抹掉）")
	}
	if item["secret"] != true {
		t.Errorf("应标记 secret=true, got %+v", item)
	}
	if item["has_value"] != true || item["set"] != true {
		t.Errorf("设过密钥后 has_value/set 应为 true, got %+v", item)
	}
	if v, _ := item["value"].(string); v != "" {
		t.Errorf("密钥的 value 必须是空串，got %q", v)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "s3cr3t") {
		t.Error("GET 泄露了密钥原值")
	}
}

// 数字项必须是 JSON number，否则前端的 type=number 输入框拿到字符串会
// 出现"框里显示 5 但表单模型是 '5'"这类只有人眼能发现的错位。
func TestSettingsGetIntIsNumber(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	_, body := e.do("GET", "/api/settings", "")
	item, ok := findItem(t, body, "session_ttl_days")
	if !ok {
		t.Fatal("找不到 session_ttl_days")
	}
	if _, isNum := item["value"].(float64); !isNum {
		t.Errorf("int 项的 value 应是 JSON number，得 %T %v", item["value"], item["value"])
	}
}

// PUT 一项非法整批 400，且错误信息里必须带上是哪一项。
// 只回 "failed" 是这个项目明令禁止的失效模式。
func TestSettingsPutInvalidReturnsReason(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	code, body := e.do("PUT", "/api/settings", `{"session_ttl_days":9999}`)
	if code != http.StatusBadRequest {
		t.Fatalf("越界值应 400, got %d %+v", code, body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "365") && !strings.Contains(msg, "天") {
		t.Errorf("错误信息应告诉用户合法范围, got %q", msg)
	}
}

// 未知键必须 400 而不是"忽略并保存其余" —— 那样用户会看到"已保存"而
// 那一项永远不生效。
func TestSettingsPutUnknownKey(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	code, body := e.do("PUT", "/api/settings", `{"made_up_key":"x","login_max_fails":7}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知键应整批 400, got %d %+v", code, body)
	}
	if body["code"] != "unknown_key" {
		t.Errorf("code 应是 unknown_key, got %+v", body)
	}
	// 同一批里合法的项**不得**被写进去。
	_, got := e.do("GET", "/api/settings", "")
	item, _ := findItem(t, got, "login_max_fails")
	if v, _ := item["value"].(float64); v != 5 {
		t.Errorf("整批应回滚，login_max_fails 不该变成 7，得 %v", item["value"])
	}
}

// 密钥项的空串 = "不修改"（因为 GET 不回显，没动过的输入框就是空的）。
// 照字面清空的后果：改下载目录顺手清掉 RPC 密钥，"改了个目录，下载全坏了"。
func TestSettingsPutSecretEmptyMeansKeep(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	if code, _ := e.do("PUT", "/api/settings", `{"aria2_rpc_secret":"k1"}`); code != http.StatusOK {
		t.Fatalf("先设密钥失败: %d", code)
	}
	// 提交一个空密钥 + 一个正常改动。
	code, body := e.do("PUT", "/api/settings", `{"aria2_rpc_secret":"","aria2_split":8}`)
	if code != http.StatusOK {
		t.Fatalf("got %d %+v", code, body)
	}
	_, got := e.do("GET", "/api/settings", "")
	item, _ := findItem(t, got, "aria2_rpc_secret")
	if item["has_value"] != true {
		t.Error("空串提交后密钥必须还在（has_value 应保持 true）")
	}
}

// 保存成功后热生效回调必须被调用；回调失败报 200+applied:false 而不是 500
// —— 设置已经写库，说"失败"会诱导用户重复提交一个其实已生效的改动。
func TestSettingsPutTriggersApply(t *testing.T) {
	var hits int
	e := newSetEnv(t, func(ctx context.Context) error { hits++; return nil })
	e.login("initial-pass-123")
	code, body := e.do("PUT", "/api/settings", `{"login_max_fails":9}`)
	if code != http.StatusOK {
		t.Fatalf("got %d %+v", code, body)
	}
	if hits != 1 {
		t.Errorf("applier 应被调用一次，得 %d", hits)
	}
	if body["applied"] != true {
		t.Errorf("applied 应为 true, got %+v", body)
	}
}

func TestSettingsPutApplyFailureNot500(t *testing.T) {
	e := newSetEnv(t, func(ctx context.Context) error { return context.DeadlineExceeded })
	e.login("initial-pass-123")
	code, body := e.do("PUT", "/api/settings", `{"login_max_fails":9}`)
	if code != http.StatusOK {
		t.Fatalf("applier 失败不该 500（值已写库）, got %d", code)
	}
	if body["applied"] != false {
		t.Errorf("applied 应为 false, got %+v", body)
	}
	if r, _ := body["reason"].(string); r == "" {
		t.Error("applied:false 必须带上原因，界面才能如实解释")
	}
}

// 库里留下的坏数字要原样吐给前端 + valid:false，而不是夹成 0。
// 夹成 0 会让用户看到一个从没人填过的合法值，一保存就把损坏固化下来。
func TestSettingsGetInvalidStoredValue(t *testing.T) {
	e := newSetEnv(t, nil)
	// 绕过 API 直接往库里塞坏值（模拟旧版本/手改库）。
	if _, err := e.db.SqlDB().Exec(
		`INSERT INTO settings(key,value,updated_at) VALUES('login_max_fails','o3',0)`,
	); err != nil {
		t.Fatal(err)
	}
	e.login("initial-pass-123")
	_, body := e.do("GET", "/api/settings", "")
	item, ok := findItem(t, body, "login_max_fails")
	if !ok {
		t.Fatal("坏值项也要出现在响应里")
	}
	if item["valid"] != false {
		t.Errorf("坏值应标 valid:false, got %+v", item)
	}
	if v, _ := item["value"].(string); v != "o3" {
		t.Errorf("坏值应原样返回（让用户能看见并改掉），得 %v", item["value"])
	}
}

// revoke-all 吊销一切会话，包括当前这一个。
func TestRevokeAllKillsCurrentSession(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	code, body := e.do("POST", "/api/sessions/revoke-all", "")
	if code != http.StatusOK {
		t.Fatalf("got %d %+v", code, body)
	}
	if body["reauth_required"] != true {
		t.Errorf("应要求重新登录, got %+v", body)
	}
	// 当前 cookie 已随响应被清；再访问受保护接口必须 401。
	if code, _ := e.do("GET", "/api/settings", ""); code != http.StatusUnauthorized {
		t.Errorf("revoke-all 之后当前会话应失效，得 %d", code)
	}
}

// 未登录访问设置接口一律 401（GET 与 PUT 都要）。
func TestSettingsRequireAuth(t *testing.T) {
	e := newSetEnv(t, nil)
	if code, _ := e.do("GET", "/api/settings", ""); code != http.StatusUnauthorized {
		t.Errorf("GET 未登录应 401, got %d", code)
	}
	// do() 在非 GET 时会自动带上 CSRF 头，但**不带**会话 cookie。
	if code, _ := e.do("PUT", "/api/settings", `{"login_max_fails":9}`); code != http.StatusUnauthorized {
		t.Errorf("PUT 未登录应 401, got %d", code)
	}
}

// api.requiredKeys 必须与"字符串型设置里提交空串会被拒的那些"完全一致。
//
// 只探字符串型：数字项提交空串被拒是因为"空串不是数字"，与"必填"是两件
// 事 —— 把它们混为一谈会让 required 这个字段对前端毫无意义（每项都 true）。
// 密钥型也不探：它的空串按契约就是"不修改"（见 TestSettingsPutSecretEmpty
// MeansKeep），拿"空串被接受"去反推"非必填"会得出反的结论。
//
// handlers_settings.go 的注释承诺了这条测试，这里是兑现处：它保证前端渲染
// 出"可不填"的输入框而后端拒收空串这种事不会发生。
func TestRequiredMatchesValidator(t *testing.T) {
	e := newSetEnv(t, nil)
	e.login("initial-pass-123")
	_, body := e.do("GET", "/api/settings", "")
	flagged := map[string]bool{}
	groups, _ := body["groups"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		items, _ := gm["items"].([]any)
		for _, it := range items {
			m, _ := it.(map[string]any)
			if m["required"] == true {
				flagged[m["key"].(string)] = true
			}
		}
	}
	probed := 0
	for _, d := range settings.Defs() {
		if d.Kind != settings.KindStr || d.Secret {
			continue
		}
		probed++
		// 用一个独立的临时环境探空串：这会污染真实库里该项的值。
		probed0, _ := probeEmpty(t, string(d.Key))
		isFlagged := flagged[string(d.Key)]
		if probed0 && !isFlagged {
			t.Errorf("%s 提交空串被拒，但响应里没标 required", d.Key)
		}
		if !probed0 && isFlagged {
			t.Errorf("%s 标了 required 但空串能通过校验", d.Key)
		}
	}
	if probed == 0 {
		t.Fatal("前置条件：没有任何字符串型设置可探，这条什么都没测")
	}
}

// probeEmpty 返回"给这个键并提交空串会不会被拒"。
// 每个键都用全新的环境，避免上一个键的写入影响下一个键的判断。
func probeEmpty(t *testing.T, key string) (rejected bool, _ error) {
	t.Helper()
	e := newSetEnv(t, nil)
	if _, err := e.db.SqlDB().Exec(
		`UPDATE settings SET value='0' WHERE key='must_change_password'`); err != nil {
		return false, err
	}
	e.login("initial-pass-123")
	code, _ := e.do("PUT", "/api/settings", `{"`+key+`":""}`)
	return code == http.StatusBadRequest, nil
}
