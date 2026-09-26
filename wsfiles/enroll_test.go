package wsfiles

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// newTestEnrollFixture 造一份干净的节点身份 + 申请材料（申请 → 官方签发 → 回信 → 解密全套）
type enrollFixture struct {
	id      *Identity
	encPriv []byte
	encPub  []byte
	reqText string
	req     *EnrollRequest
}

func newTestEnrollFixture(t *testing.T) *enrollFixture {
	t.Helper()
	resetIdentityEnv(t)
	id, _, err := OpenIdentity("", "")
	if err != nil {
		t.Fatalf("生成节点身份失败: %v", err)
	}
	encPriv, encPub, _, _, err := OpenEnrollKey()
	if err != nil {
		t.Fatalf("生成领证密钥失败: %v", err)
	}
	text, err := BuildEnrollRequest(id, encPub, "清歌", "qingge@wsai.chat", []string{"upload", "delete"}, 365, "qingge")
	if err != nil {
		t.Fatalf("生成申请失败: %v", err)
	}
	req, err := ParseEnrollRequest(text)
	if err != nil {
		t.Fatalf("解析申请失败: %v", err)
	}
	return &enrollFixture{id: id, encPriv: encPriv, encPub: encPub, reqText: text, req: req}
}

// ① 身份码自包含可验：BuildIdentityCode 产出的码，按 wst-ident 的载荷公式能验通
func TestEnrollIdentityCodeSelfContained(t *testing.T) {
	f := newTestEnrollFixture(t)
	code, err := BuildIdentityCode(f.id, "清歌", "qingge@wsai.chat")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, IdentHeader) {
		t.Fatalf("身份码应以 %s 开头，实际: %q", IdentHeader, code[:min(30, len(code))])
	}
	id, name, email, pubLine, fp, err := VerifyIdentityCode(code)
	if err != nil {
		t.Fatalf("身份码验签应通过: %v", err)
	}
	if id != f.id.Node || name != "清歌" || email != "qingge@wsai.chat" {
		t.Fatalf("身份码字段不符: %s/%s/%s", id, name, email)
	}
	if fp != f.id.FP {
		t.Fatalf("身份码指纹不符: %s != %s", fp, f.id.FP)
	}
	// 与 wst-ident 工具**逐字符同构**：载荷 = id|name|email|key|fp，签名为裸 Ed25519 base64
	pub, _, _ := ParsePubKeyValue(pubLine)
	var raw struct{ sig string }
	for _, ln := range strings.Split(code, "\n") {
		if strings.HasPrefix(ln, "sig: ") {
			raw.sig = strings.TrimPrefix(ln, "sig: ")
		}
	}
	sig, _ := base64.StdEncoding.DecodeString(raw.sig)
	want := []byte(fmt.Sprintf("%s|%s|%s|%s|%s", id, name, email, pubLine, fp))
	if !ed25519.Verify(pub, want, sig) {
		t.Fatal("身份码签名与 wst-ident 的载荷公式不一致（互操作会断）")
	}
}

// ② 身份码被改动 ⇒ 必须验不过
func TestEnrollIdentityCodeTamperRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	code, _ := BuildIdentityCode(f.id, "清歌", "qingge@wsai.chat")
	for _, tc := range []struct{ name, from, to string }{
		{"改名称", "name: 清歌", "name: 观音"},
		{"改邮箱", "qingge@wsai.chat", "evil@example.com"},
		{"改指纹", "fp: " + f.id.FP, "fp: SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		bad := strings.Replace(code, tc.from, tc.to, 1)
		if _, _, _, _, _, err := VerifyIdentityCode(bad); err == nil {
			t.Fatalf("%s：身份码被改动却验签通过", tc.name)
		}
	}
}

// ③ 申请信封：签名覆盖全部关键字段；任一字段被改都要拒
func TestEnrollRequestTamperRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	if err := VerifyEnrollRequest(f.req, time.Now()); err != nil {
		t.Fatalf("合法申请应通过: %v", err)
	}
	// 逐字段篡改（只改**请求段**内的字段，段①身份码保持原样）
	cases := []struct{ name, from, to string }{
		{"换回信公钥(enc)", "enc: " + f.req.Enc, "enc: " + base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"扩权(want)", "want: " + strings.Join(f.req.Want, ","), "want: upload,delete,admin"},
		{"改有效期", fmt.Sprintf("days: %d", f.req.Days), "days: 36500"},
		{"改节点", "id: " + f.req.ID, "id: node:someone-else"},
		{"改回信邮箱", "email: qingge@wsai.chat", "email: attacker@example.com"},
	}
	for _, tc := range cases {
		bad := tamperReqSection(t, f.reqText, tc.from, tc.to)
		req, err := ParseEnrollRequest(bad)
		if err != nil {
			continue // 解析阶段就拒了，也算通过
		}
		if err := VerifyEnrollRequest(req, time.Now()); err == nil {
			t.Fatalf("%s：字段被改却验签通过", tc.name)
		}
	}
	// 附了一段**伪造的身份码**（段②合法）⇒ 整体必须拒，不能静默忽略段①
	other, _, _ := OpenIdentity("", "node:another")
	otherCode, _ := BuildIdentityCode(other, "别人", "other@wsai.chat")
	idx := strings.Index(f.reqText, EnrollHeader)
	mixed := otherCode + "\n\n" + f.reqText[idx:]
	if req, err := ParseEnrollRequest(mixed); err == nil {
		if err := VerifyEnrollRequest(req, time.Now()); err == nil {
			t.Fatal("附了伪造身份码却被放过")
		}
	}
}

// tamperReqSection 只在 WSA-NODECERT 请求段内做替换（段①身份码不动）
func tamperReqSection(t *testing.T, text, from, to string) string {
	t.Helper()
	idx := strings.Index(text, EnrollHeader)
	if idx < 0 {
		t.Fatal("测试用例失效：文本里没有请求段")
	}
	head, body := text[:idx], text[idx:]
	if !strings.Contains(body, from) {
		t.Fatalf("测试用例的原文 %q 不在请求段内，用例本身失效", from)
	}
	return head + strings.Replace(body, from, to, 1)
}

// ④ 超出时间窗的申请必须拒（防重放旧申请）。
// 注意：必须构造一份**签名有效**、只是 ts 很旧的申请 —— 否则先撞上"签名无效"，
// 就测不到时间窗这一层（签名里包含 ts，改 ts 必然破坏签名）。
func TestEnrollRequestStaleRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	oldTS := time.Now().Add(-90 * 24 * time.Hour).Unix()
	text := buildSignedRequestWithTS(t, f, oldTS)
	req, err := ParseEnrollRequest(text)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnrollRequest(req, time.Now()); err == nil {
		t.Fatal("90 天前的申请仍被接受")
	} else if !strings.Contains(err.Error(), "时间戳") {
		t.Fatalf("错误信息应点明时间戳问题，实际: %v", err)
	}
	// 反证：同一份构造逻辑、ts 新鲜 ⇒ 必须通过（否则说明是构造错了而非窗口生效）
	fresh := buildSignedRequestWithTS(t, f, time.Now().Unix())
	req2, err := ParseEnrollRequest(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnrollRequest(req2, time.Now()); err != nil {
		t.Fatalf("新鲜签名的申请应通过（说明上面拒的是时间，不是签名）: %v", err)
	}
}

// buildSignedRequestWithTS 按协议格式手写一份申请文本（仅 ts 可控），签名有效。
// 测试侧独立拼串，避免"改实现改坏签名逻辑、测试跟着一起坏"。
func buildSignedRequestWithTS(t *testing.T, f *enrollFixture, ts int64) string {
	t.Helper()
	pubLine := SSHPubLine(f.id.Pub, f.id.Node+nodeKeyComment)
	enc := base64.StdEncoding.EncodeToString(f.encPub)
	want := []string{"delete", "upload"} // 排序后口径（与 normalizeWant 一致）
	payload := []byte(strings.Join([]string{
		"wsa-nodecert/v1",
		f.id.Node, "清歌", "qingge@wsai.chat", pubLine, f.id.FP, enc,
		strings.Join(want, ","), "365", "qingge", fmt.Sprintf("%d", ts),
	}, "\n"))
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(f.id.Priv, payload))
	return strings.Join([]string{
		EnrollHeader,
		"id: " + f.id.Node,
		"name: 清歌",
		"email: qingge@wsai.chat",
		"pub: " + pubLine,
		"fp: " + f.id.FP,
		"enc: " + enc,
		"want: " + strings.Join(want, ","),
		"days: 365",
		"host: qingge",
		"ts: " + fmt.Sprintf("%d", ts),
		"sig: " + sig,
	}, "\n")
}

// ⑤ 非法授权范围必须拒（不许申请超出服务端能力的权限）
func TestEnrollRequestBadScopeRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	_, err := BuildEnrollRequest(f.id, f.encPub, "n", "a@b.c", []string{"upload", "root"}, 365, "h")
	if err == nil {
		t.Fatal("构造阶段就该拒绝非法授权范围")
	}
	if !strings.Contains(err.Error(), "授权范围") {
		t.Fatalf("错误信息应点明授权范围，实际: %v", err)
	}
}

// ⑥ 身份码与请求块指向不同节点 ⇒ 拒（不许"半真半假"）
func TestEnrollMismatchedIdentityCodeRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	other, _, _ := OpenIdentity("", "node:another")
	otherCode, _ := BuildIdentityCode(other, "另一个", "other@wsai.chat")
	// 把段①换成别人的身份码，段②保持原样
	idx := strings.Index(f.reqText, EnrollHeader)
	mixed := otherCode + "\n\n" + f.reqText[idx:]
	req, err := ParseEnrollRequest(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnrollRequest(req, time.Now()); err == nil {
		t.Fatal("身份码与请求块节点不一致却通过")
	}
}

// ⑦ 回信加解密往返：证书全文逐字节一致，且只有本机私钥能解
func TestEnrollReplyEncryptDecryptRoundTrip(t *testing.T) {
	f := newTestEnrollFixture(t)
	certText := `{"v":1,"node":"` + f.id.Node + `","pub":"AAAA","fp":"SHA256:x","sig":"zzz"}`
	reply, err := EncryptEnrollReply(f.id.Node, f.encPub, certText, "SHA256:cert", "wsa-root", "SHA256:root", time.Now().AddDate(1, 0, 0).Unix())
	if err != nil {
		t.Fatalf("加密回信失败: %v", err)
	}
	if strings.Contains(reply, "AAAA") {
		t.Fatal("回信里出现了证书明文 —— 明文凭据不得裸传")
	}
	got, meta, err := DecryptEnrollReply(f.encPriv, reply)
	if err != nil {
		t.Fatalf("解密回信失败: %v", err)
	}
	if got != certText {
		t.Fatalf("解密结果与原文不符:\n got=%s\nwant=%s", got, certText)
	}
	if meta.Iss != "wsa-root" || meta.FP != "SHA256:cert" {
		t.Fatalf("回信元信息不符: %+v", meta)
	}

	// 换一把私钥（模拟"回信不是发给本机"）⇒ 必须解不开
	otherPriv, _, err := NewEnrollKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecryptEnrollReply(otherPriv, reply); err == nil {
		t.Fatal("用别人的私钥竟能解密回信")
	}
}

// ⑧ 回信被改动（密文/nonce/sha/节点）⇒ 拒
func TestEnrollReplyTamperRejected(t *testing.T) {
	f := newTestEnrollFixture(t)
	reply, err := EncryptEnrollReply(f.id.Node, f.encPub, `{"v":1}`, "SHA256:c", "wsa-root", "SHA256:r", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctLine := regexp.MustCompile(`(?m)^ct: .*$`).FindString(reply)
	if ctLine == "" {
		t.Fatal("测试用例失效：回信里没有 ct 行")
	}
	// 改密文：把密文首字节改掉
	raw := strings.TrimPrefix(ctLine, "ct: ")
	raw = strings.TrimSpace(raw)
	blob, _ := base64.StdEncoding.DecodeString(raw)
	blob[0] ^= 0xff
	tampered := strings.Replace(reply, ctLine, "ct: "+base64.StdEncoding.EncodeToString(blob), 1)
	if _, _, err := DecryptEnrollReply(f.encPriv, tampered); err == nil {
		t.Fatal("密文被改动却解密成功（AEAD 失效）")
	}
	// 改声明 sha：即便能解开也必须因摘要不符而拒
	bad := strings.Replace(reply, "sha: ", "sha: 00", 1)
	if _, _, err := DecryptEnrollReply(f.encPriv, bad); err == nil {
		t.Fatal("摘要与内容不符却通过")
	}
	// 改 node 主体：AAD 变化 ⇒ 解不开
	bad2 := strings.Replace(reply, "node: "+f.id.Node, "node: node:hijacked", 1)
	if _, _, err := DecryptEnrollReply(f.encPriv, bad2); err == nil {
		t.Fatal("回信主体被改却仍能解密（AAD 未生效）")
	}
}

// ⑨ 领证密钥：首次生成、再次读取同一把、绝不覆盖
func TestOpenEnrollKeyPersistentAndNeverOverwrites(t *testing.T) {
	resetIdentityEnv(t)
	priv1, pub1, path1, gen1, err := OpenEnrollKey()
	if err != nil {
		t.Fatal(err)
	}
	if !gen1 {
		t.Fatal("首次调用应标记为『新生成』")
	}
	if len(priv1) != 32 || len(pub1) != 32 {
		t.Fatalf("X25519 密钥长度异常: %d/%d", len(priv1), len(pub1))
	}
	if st, _ := os.Stat(path1); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("领证私钥权限应为 0600，实际 %v", st.Mode().Perm())
	}
	priv2, pub2, path2, gen2, err := OpenEnrollKey()
	if err != nil {
		t.Fatal(err)
	}
	if gen2 || path1 != path2 {
		t.Fatal("第二次调用不应重新生成")
	}
	if string(priv1) != string(priv2) || string(pub1) != string(pub2) {
		t.Fatal("重复调用应读到同一把密钥（否则之前发出的回信会解不开）")
	}
}

// ⑩ 未附身份码也能用（仅请求块签名）；空邮箱必须拒（回信无处可寄）
func TestEnrollWithoutIdentityCodeStillValid(t *testing.T) {
	f := newTestEnrollFixture(t)
	idx := strings.Index(f.reqText, EnrollHeader)
	bare := f.reqText[idx:]
	req, err := ParseEnrollRequest(bare)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEnrollRequest(req, time.Now()); err != nil {
		t.Fatalf("仅有请求块（已签名）时应通过: %v", err)
	}
	if _, err := BuildEnrollRequest(f.id, f.encPub, "n", "", []string{"upload"}, 365, "h"); err == nil {
		t.Fatal("空邮箱应被拒（没有回信地址）")
	}
}

// ⑪ 邮件里带引文/围栏：解析必须仍然正确（否则官方侧会误判）
func TestEnrollParseThroughMailQuoting(t *testing.T) {
	f := newTestEnrollFixture(t)
	mail := FormatEnrollRequestMail(f.reqText, "hello@wsai.chat")
	quoted := "看到一封领证申请，转给你：\n\n" +
		"    > 下面这段是引用，不该被当成有效内容\n" +
		"    > " + strings.ReplaceAll(f.reqText, "\n", "\n    > ") + "\n\n" +
		mail
	req, err := ParseEnrollRequest(quoted)
	if err != nil {
		t.Fatalf("带引文的邮件应能解析出申请: %v", err)
	}
	if err := VerifyEnrollRequest(req, time.Now()); err != nil {
		t.Fatalf("带引文的申请应验签通过: %v", err)
	}
	if req.ID != f.id.Node {
		t.Fatalf("解析到错误的节点: %s", req.ID)
	}
}

// ⑫ 申请摘要要给人看得懂的内容（审批界面/回执用）
func TestEnrollRequestSummaryMentionsKeyFacts(t *testing.T) {
	f := newTestEnrollFixture(t)
	s := EnrollRequestSummary(f.req)
	for _, want := range []string{f.req.ID, f.req.Email, f.req.FP, "upload", "365"} {
		if !strings.Contains(s, want) {
			t.Fatalf("摘要缺少关键信息 %q:\n%s", want, s)
		}
	}
}

// ⑬ 申请文本里**绝不能**出现任何私钥材料（SECURITY/P0）
func TestEnrollRequestCarriesNoPrivateMaterial(t *testing.T) {
	f := newTestEnrollFixture(t)
	r := f.reqText
	if strings.Contains(r, "PRIVATE KEY") {
		t.Fatal("申请里出现了私钥")
	}
	privB64 := base64.StdEncoding.EncodeToString(f.id.Priv)
	if strings.Contains(r, privB64) {
		t.Fatal("申请里出现了 Ed25519 私钥的 base64")
	}
	if strings.Contains(r, base64.StdEncoding.EncodeToString(f.encPriv)) {
		t.Fatal("申请里出现了 X25519 私钥的 base64")
	}
	// 也不应是 JSON 里夹带私钥（防止将来重构时退化成"整包 JSON 传输"）
	var anyJSON map[string]interface{}
	if json.Unmarshal([]byte(r), &anyJSON) == nil && anyJSON["priv"] != nil {
		t.Fatal("申请里出现了 priv 字段")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
