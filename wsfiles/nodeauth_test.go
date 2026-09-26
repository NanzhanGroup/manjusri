package wsfiles

// nodeauth_test.go —— 客户端侧节点签名（wsauth v1）不变式测试
//
// 钉住的关键点：
//  ① 从 OpenSSH 私钥能正确加载身份（ws-core 生成的正是这个格式）；
//  ② 每个写请求都带齐签名头，且**签名可被对端用公钥验证通过**（不是"随便填了几个头"）；
//  ③ **重试必须重签**（ts/nonce 变化）—— 复用旧签名会被服务端当重放拒掉，这是最容易埋雷的一处；
//  ④ 有证书就带 X-WS-Cert；
//  ⑤ 读不到私钥且无令牌 ⇒ 禁用，并且给出**可操作**的原因。

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── 测试用 OpenSSH 私钥序列化（与 ws-core 生成的格式一致：cipher=none / kdf=none）──

func marshalOpenSSHTestKey(priv ed25519.PrivateKey, pub ed25519.PublicKey, comment string) []byte {
	var b []byte
	b = append(b, []byte(openSSHMagic)...)
	b = appendSSHStr(b, []byte("none"))
	b = appendSSHStr(b, []byte("none"))
	b = appendSSHStr(b, nil)
	b = binary.BigEndian.AppendUint32(b, 1)
	blob := appendSSHStr(nil, []byte("ssh-ed25519"))
	blob = appendSSHStr(blob, pub)
	b = appendSSHStr(b, blob)

	var p []byte
	p = binary.BigEndian.AppendUint32(p, 0x12345678)
	p = binary.BigEndian.AppendUint32(p, 0x12345678)
	p = appendSSHStr(p, []byte("ssh-ed25519"))
	p = appendSSHStr(p, pub)
	p = appendSSHStr(p, priv)
	p = appendSSHStr(p, []byte(comment))
	for i := 1; len(p)%8 != 0; i++ {
		p = append(p, byte(i))
	}
	b = appendSSHStr(b, p)

	var out bytes.Buffer
	out.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	enc := base64.StdEncoding.EncodeToString(b)
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n-----END OPENSSH PRIVATE KEY-----\n")
	return out.Bytes()
}

// installTestIdentity 在临时目录里造一把节点私钥，并把环境变量指向它。
// 返回公钥与节点 id，供假服务端验签。
func installTestIdentity(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyPath, marshalOpenSSHTestKey(priv, pub, "test@wenshu-community"), 0o600); err != nil {
		t.Fatal(err)
	}
	node := "wsa-testnode"
	t.Setenv(EnvKeyPath, keyPath)
	t.Setenv(EnvNodeID, node)
	t.Setenv(EnvCertPath, filepath.Join(dir, "not-exist-cert.json"))
	t.Setenv("WS_FILES_TOKEN", "")
	return pub, node
}

// verifyNodeSig 按协议校验一次请求的签名（测试侧独立实现，避免"自己验自己"的假通过）
func verifyNodeSig(r *http.Request, body []byte, pub ed25519.PublicKey) (node, nonce string, ok bool) {
	node = r.Header.Get(HeaderNode)
	nonce = r.Header.Get(HeaderNonce)
	if r.Header.Get(HeaderAuth) != AuthVersion || node == "" || nonce == "" {
		return node, nonce, false
	}
	tsStr := r.Header.Get(HeaderTS)
	var ts int64
	for _, ch := range tsStr {
		if ch < '0' || ch > '9' {
			return node, nonce, false
		}
		ts = ts*10 + int64(ch-'0')
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSig))
	if err != nil {
		return node, nonce, false
	}
	payload := AuthPayload(node, ts, r.Method, r.URL.EscapedPath(), nonce, body)
	return node, nonce, ed25519.Verify(pub, payload, sig)
}

// ── ① 身份加载 ──

func TestLoadIdentityFromOpenSSHKey(t *testing.T) {
	pub, node := installTestIdentity(t)
	id, err := LoadIdentity(os.Getenv(EnvKeyPath), "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Node != node {
		t.Fatalf("节点 id 应为 %s，实际 %s", node, id.Node)
	}
	if !bytes.Equal(id.Pub, pub) {
		t.Fatal("解析出的公钥与源公钥不一致（OpenSSH 解析有误）")
	}
	if id.FP != KeyFingerprint(pub) || !strings.HasPrefix(id.FP, "SHA256:") {
		t.Fatalf("指纹异常: %s", id.FP)
	}
	if len(id.Priv) != ed25519.PrivateKeySize {
		t.Fatalf("私钥长度异常: %d", len(id.Priv))
	}
}

func TestLoadIdentityMissingKeyIsActionable(t *testing.T) {
	t.Setenv(EnvKeyPath, filepath.Join(t.TempDir(), "nope")) // 不存在的私钥
	t.Setenv(EnvNodeID, "wsa-x")
	t.Setenv("WS_FILES_TOKEN", "")
	c := New("cn")
	if c.Enabled() {
		t.Fatal("无身份、无令牌时应为禁用")
	}
	err := c.Err()
	if err == nil || !strings.Contains(err.Error(), "id_ed25519") {
		t.Fatalf("禁用原因应指明私钥路径，实际: %v", err)
	}
}

func TestParseRawBase64Key(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	if _, _, err := parseOpenSSHEd25519([]byte(base64.StdEncoding.EncodeToString(priv))); err != nil {
		t.Fatalf("裸 base64 私钥应被接受: %v", err)
	}
	if _, _, err := parseOpenSSHEd25519([]byte("这不是密钥")); err == nil {
		t.Fatal("垃圾输入应报错，而不能静默返回空密钥")
	}
}

// ── ② 签名头齐全且可验证 ──

func TestSignRequestProducesVerifiableHeaders(t *testing.T) {
	pub, node := installTestIdentity(t)
	id, err := LoadIdentity(os.Getenv(EnvKeyPath), "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"filename":"a.txt"}`)
	req, _ := http.NewRequest(http.MethodPost, "https://cn.dl.xiusoft.cn/api/upload/raw", bytes.NewReader(body))
	if err := id.SignRequest(req, body, ""); err != nil {
		t.Fatal(err)
	}
	gotNode, nonce, ok := verifyNodeSig(req, body, pub)
	if !ok {
		t.Fatal("签名头未能通过独立验签")
	}
	if gotNode != node || nonce == "" {
		t.Fatalf("节点 id/nonce 异常: %q %q", gotNode, nonce)
	}
	// 体被改一个字节 ⇒ 必须验不过（签名覆盖面含 body 摘要）
	if _, _, ok := verifyNodeSig(req, []byte(`{"filename":"b.txt"}`), pub); ok {
		t.Fatal("请求体被改动后签名竟然仍通过 —— 签名未覆盖 body")
	}
	// 另一把公钥 ⇒ 必须验不过
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if _, _, ok := verifyNodeSig(req, body, otherPub); ok {
		t.Fatal("用别人的公钥竟然验过了")
	}
}

func TestSignaturesAreUniquePerRequest(t *testing.T) {
	id, err := LoadIdentity("", "")
	if err != nil {
		t.Fatal(err) // 本机真实身份（测试机上有 data/family/id_ed25519）
	}
	body := []byte("x")
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest(http.MethodPost, "https://cn.dl.xiusoft.cn/api/upload/raw", bytes.NewReader(body))
		if err := id.SignRequest(req, body, ""); err != nil {
			t.Fatal(err)
		}
		n := req.Header.Get(HeaderNonce)
		if seen[n] {
			t.Fatal("nonce 重复 —— 服务端会判为重放")
		}
		seen[n] = true
	}
}

// ── ③ 重试必须重签（本包最容易埋雷的一处）──

func TestRetryResignsRequest(t *testing.T) {
	pub, _ := installTestIdentity(t)
	f := newFake()
	f.requireSig = true
	f.pub = pub
	f.failChunk = 1 // 第一块先回 429，逼出重试
	f.infoChunk = 400000

	srv := newHTTPTestServer(t, f)
	t.Setenv("WS_FILES_BASE_CN", srv)
	t.Setenv("WS_FILES_BASE_HK", srv)
	t.Setenv("WS_FILES_REGION", "cn")
	c := New("cn")
	if !c.Enabled() {
		t.Fatal("有节点身份时应启用")
	}
	p := writeTemp(t, "retry.bin", SingleShotRawMax+1000)
	if _, err := c.Upload(t.Context(), p, "retry.bin"); err != nil {
		t.Fatalf("带重试的上传应成功: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.badSigs != 0 {
		t.Fatalf("有 %d 个请求签名不合法（重试时未重签？）", f.badSigs)
	}
	if len(f.seenNonces) < 3 {
		t.Fatalf("请求数异常（应 init + 2 块 + complete）: %d", len(f.seenNonces))
	}
	uniq := map[string]bool{}
	for _, n := range f.seenNonces {
		if n == "" {
			t.Fatal("有请求没带 nonce")
		}
		if uniq[n] {
			t.Fatal("nonce 被复用（重试时复用了旧签名 ⇒ 服务端会判重放）")
		}
		uniq[n] = true
	}
}

// ── ④ 证书随请求携带 ──

func TestCertIsCarriedWhenPresent(t *testing.T) {
	pub, node := installTestIdentity(t)
	// 造一张假根签发的证书（内容不重要，这里只验证"带上去了"）
	_, root, _ := ed25519.GenerateKey(nil)
	cert := SignCert(Cert{
		Node: node, Pub: base64.StdEncoding.EncodeToString(pub), FP: KeyFingerprint(pub),
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(), Scope: []string{"upload"}, Iss: "wsa-root",
	}, root)
	txt, err := EncodeCert(cert)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(t.TempDir(), "node-cert.json")
	if err := os.WriteFile(certPath, []byte(txt), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvCertPath, certPath)

	f := newFake()
	f.requireSig = true
	f.pub = pub
	srv := newHTTPTestServer(t, f)
	t.Setenv("WS_FILES_BASE_CN", srv)
	t.Setenv("WS_FILES_BASE_HK", srv)
	c := New("cn")
	if !c.HasCert() {
		t.Fatal("存在证书文件时 HasCert() 应为 true")
	}
	if _, err := c.Upload(t.Context(), writeTemp(t, "c.txt", 100), "c.txt"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seenCerts) == 0 || f.seenCerts[0] == "" {
		t.Fatal("证书未被携带")
	}
	if f.seenCerts[0] != txt {
		t.Fatal("携带的证书文本与文件不一致")
	}
}

// ── 证书本身的编解码/验签（客户端 API）──

func TestCertEncodeDecodeAndVerify(t *testing.T) {
	rootPub, rootPriv, _ := ed25519.GenerateKey(nil)
	nodePub, _, _ := ed25519.GenerateKey(nil)
	c := SignCert(Cert{
		Node: "wsa-a", Pub: base64.StdEncoding.EncodeToString(nodePub), FP: KeyFingerprint(nodePub),
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(), Scope: []string{"upload"}, Iss: "wsa-root",
	}, rootPriv)
	txt, err := EncodeCert(c)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCert(txt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCert(back, rootPub, time.Now()); err != nil {
		t.Fatalf("合法证书应通过: %v", err)
	}
	// 篡改 node ⇒ 失效
	back.Node = "wsa-b"
	if _, err := VerifyCert(back, rootPub, time.Now()); err == nil {
		t.Fatal("篡改后的证书竟然通过验签")
	}
	// 过期 ⇒ 失效
	fresh, _ := DecodeCert(txt)
	fresh.Exp = time.Now().Add(-time.Hour).Unix()
	// 注意：改 Exp 后签名也随之失效，这里单独构造一张"合法签发但已过期"的证书
	expired := SignCert(Cert{
		Node: "wsa-a", Pub: fresh.Pub, FP: fresh.FP,
		Iat: time.Now().Add(-2 * time.Hour).Unix(), Exp: time.Now().Add(-time.Hour).Unix(),
		Scope: []string{"upload"}, Iss: "wsa-root",
	}, rootPriv)
	if _, err := VerifyCert(expired, rootPub, time.Now()); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期证书应报过期，实际: %v", err)
	}
}

func TestSSHPubLineRoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	line := SSHPubLine(pub, "wsa-a")
	got, fp, err := ParsePubKeyValue(line)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pub) || fp != KeyFingerprint(pub) {
		t.Fatal("ssh 公钥行往返不一致")
	}
	if _, _, err := ParsePubKeyValue(base64.StdEncoding.EncodeToString(pub)); err != nil {
		t.Fatalf("裸 base64 公钥应被接受: %v", err)
	}
}

// newHTTPTestServer 起一个假 ws-files 站点，返回其 URL
func newHTTPTestServer(t *testing.T, f *fakeFiles) string {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	f.mu.Lock()
	f.url = ts.URL
	f.mu.Unlock()
	return ts.URL
}

// TestKeyFingerprintMatchesOpenSSHVector 指纹口径必须与 OpenSSH / family/registry.json 一致。
//
// 价值同服务端：指纹是**人机共用**的核对值（ssh-keygen -lf 的输出、吊销名单里的写法）。
// 若被"顺手优化"成 sha256(32 字节裸公钥)，客户端内部仍自洽、测试也不报错，
// 但与官方登记值对不上 —— 那种错要到生产排障时才暴露。故钉死真实向量。
func TestKeyFingerprintMatchesOpenSSHVector(t *testing.T) {
	const pubB64 = "bSfxh4rpU6nwHbEA5yoQ3Ysds/UeBF8R1bK6KC7DQyQ="
	const wantFP = "SHA256:4gzqnarwjJcXcUU7IpoWmApIHbbaqlK/8sq0TBmwX7E"
	pub, fp, err := ParsePubKeyValue(pubB64)
	if err != nil {
		t.Fatal(err)
	}
	if fp != wantFP {
		t.Fatalf("指纹口径与 OpenSSH/registry.json 不一致：got %s want %s", fp, wantFP)
	}
	if _, fp2, err := ParsePubKeyValue(SSHPubLine(pub, "")); err != nil || fp2 != wantFP {
		t.Fatalf("ssh 行解析的指纹应一致：got %s err=%v", fp2, err)
	}
}
