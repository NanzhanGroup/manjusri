package wsfiles

// identity_test.go —— 节点身份与 family 解耦（客户单机部署没有 data/family/）
//
// 钉住的不变式：
//  ① 没有 family 目录时，客户端**自动生成**中性节点身份（$WS_PATH/data/node/），零配置可用；
//  ② 我们自己的家族节点（有 data/family/id_ed25519）继续按旧身份工作，且**绝不被扰动**；
//  ③ 本包**绝不往 data/family/ 里写任何东西**（那是家族内部标识，不是本包的管辖范围）；
//  ④ 显式指定的私钥路径不存在 ⇒ 明确报错，不悄悄造新密钥（否则换机器毫无感知）；
//  ⑤ 证书主体是权威口径；与本地私钥不匹配的证书必须被忽略并给出原因。

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// resetIdentityEnv 清空身份相关环境变量，并把 WS_PATH 指向一个干净的临时目录
func resetIdentityEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WS_PATH", dir)
	for _, k := range []string{EnvKeyPath, EnvNodeID, EnvNodeIDShared, EnvCertPath, EnvNodeDir, "WS_FILES_TOKEN"} {
		t.Setenv(k, "")
	}
	return dir
}

func writeTestKey(t *testing.T, path, comment string) ed25519.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, marshalOpenSSHTestKey(priv, pub, comment), 0o600); err != nil {
		t.Fatal(err)
	}
	return pub
}

// ① 客户单机（无 family）：客户端自动生成中性身份，立即可用
func TestClientGeneratesNeutralIdentityWithoutFamily(t *testing.T) {
	dir := resetIdentityEnv(t)
	if _, err := os.Stat(filepath.Join(dir, "data", "family")); err == nil {
		t.Fatal("前置条件不成立：客户单机不应有 data/family 目录")
	}

	c := New("cn")
	if !c.Enabled() {
		t.Fatalf("无 family 目录时应自动生成节点身份并启用，实际: %v", c.Err())
	}
	if got := c.IdentitySourceOf(); got != SourceGenerated {
		t.Errorf("首次应为新生成身份，实际来源 %q", got)
	}
	if !fileExists(NodeKeyPath()) {
		t.Fatalf("应在 %s 生成私钥", NodeKeyPath())
	}
	// 权限：私钥 0600、目录 0700（身份材料不得对他人可读）
	if st, err := os.Stat(NodeKeyPath()); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o600 {
		t.Errorf("私钥权限应为 0600，实际 %o", st.Mode().Perm())
	}
	if st, err := os.Stat(NodeDir()); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o700 {
		t.Errorf("身份目录权限应为 0700，实际 %o", st.Mode().Perm())
	}
	// 节点 id 落在 node.json，签名头与之一致（id 稳定，不受改名影响）
	meta := ReadNodeMeta()
	if meta.ID == "" {
		t.Fatal("应写下 node.json 的 id，供后续稳定引用")
	}
	if c.NodeID() != meta.ID {
		t.Errorf("节点 id 应取 node.json，实际 %q / %q", c.NodeID(), meta.ID)
	}
	// ③ 绝不往 family 里写东西
	if _, err := os.Stat(filepath.Join(dir, "data", "family")); err == nil {
		t.Error("本包不得在 data/family 下创建任何文件")
	}
	// 幂等：再次构造复用同一身份（不换机器）
	c2 := New("cn")
	if c2.NodeID() != c.NodeID() {
		t.Errorf("二次构造不得换身份: %q vs %q", c2.NodeID(), c.NodeID())
	}
	if got := c2.IdentitySourceOf(); got != SourceNodeDir {
		t.Errorf("二次构造应从 data/node 读取，实际来源 %q", got)
	}
}

// 生成的身份必须可解析、指纹与 OpenSSH 口径一致、公钥行可被 ssh 工具读取
func TestGeneratedKeyRoundTripsAndFingerprintMatches(t *testing.T) {
	resetIdentityEnv(t)
	if err := InitNodeIdentity("wsa-unit"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(NodeKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := parseOpenSSHEd25519(raw)
	if err != nil {
		t.Fatalf("生成的私钥必须能被自家解析器读回: %v", err)
	}
	id, err := LoadIdentity("", "")
	if err != nil {
		t.Fatal(err)
	}
	if id.Node != "wsa-unit" {
		t.Errorf("应使用 node.json 里的 id，实际 %q", id.Node)
	}
	if id.FP != KeyFingerprint(pub) {
		t.Errorf("指纹口径应一致: %q vs %q", id.FP, KeyFingerprint(pub))
	}
	line, err := os.ReadFile(NodeKeyPath() + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	want := SSHPubLine(pub, "wsa-unit"+nodeKeyComment)
	if strings.TrimSpace(string(line)) != want {
		t.Errorf("公钥行应为 ssh-ed25519 格式:\n got %q\nwant %q", strings.TrimSpace(string(line)), want)
	}
	if _, _, err := parseOpenSSHEd25519(MarshalNodeKey(priv, pub, "round-trip")); err != nil {
		t.Errorf("MarshalNodeKey 产物必须可被解析: %v", err)
	}
}

// ② 家族老节点：继续用 family 身份，且不得因此生成/改动任何东西
func TestLegacyFamilyIdentityKeptAndNotTouched(t *testing.T) {
	dir := resetIdentityEnv(t)
	famDir := filepath.Join(dir, "data", "family")
	writeTestKey(t, filepath.Join(famDir, "id_ed25519"), "wsa-qingge@wenshu-community")
	if err := os.WriteFile(filepath.Join(famDir, "me.json"), []byte(`{"id":"wsa-qingge"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(famDir, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}

	c := New("cn")
	if got := c.IdentitySourceOf(); got != SourceLegacyFamily {
		t.Errorf("有家族身份时应走旧路径兼容，实际来源 %q", got)
	}
	if c.NodeID() != "wsa-qingge" {
		t.Errorf("节点 id 应取 family/me.json，实际 %q", c.NodeID())
	}
	if fileExists(NodeKeyPath()) {
		t.Error("已有家族身份时不得再生成新身份 —— 否则公钥/节点 id 漂移，名册（L1）立刻对不上")
	}
	after, err := os.ReadFile(filepath.Join(famDir, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("家族私钥被改动了")
	}
}

// 两处都有时：中性节点身份优先（显式放置的才算数）
func TestNodeDirPrecedesLegacyFamily(t *testing.T) {
	dir := resetIdentityEnv(t)
	writeTestKey(t, LegacyFamilyKeyPath(), "wsa-family@wenshu-community")
	writeTestKey(t, NodeKeyPath(), "node-neutral@wenshu-node")
	if err := os.WriteFile(NodeMetaPath(), []byte(`{"v":1,"id":"node-neutral"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = dir
	c := New("cn")
	if got := c.IdentitySourceOf(); got != SourceNodeDir {
		t.Errorf("data/node 应优先于旧 family 路径，实际来源 %q", got)
	}
	if c.NodeID() != "node-neutral" {
		t.Errorf("节点 id 应取 node.json，实际 %q", c.NodeID())
	}
}

// ④ 显式指定路径不存在 ⇒ 报错，且不得顺手在别处生成
func TestExplicitKeyPathIsNotGenerated(t *testing.T) {
	dir := resetIdentityEnv(t)
	missing := filepath.Join(dir, "nope", "id_ed25519")
	t.Setenv(EnvKeyPath, missing)

	c := New("cn")
	if c.Enabled() {
		t.Fatal("显式指定了不存在的私钥时应为禁用（配置错误不能被静默修复）")
	}
	if !strings.Contains(c.Err().Error(), "id_ed25519") {
		t.Errorf("错误信息应指明私钥路径，实际: %v", c.Err())
	}
	if fileExists(missing) {
		t.Error("显式路径不存在时不得生成新密钥")
	}
	if fileExists(NodeKeyPath()) {
		t.Error("显式路径模式下不得顺手在 data/node 生成身份")
	}
}

// ⑤ 证书：主体权威；公钥不匹配的证书必须被忽略并说明原因
func TestCertNodeAuthoritativeAndMismatchIgnored(t *testing.T) {
	resetIdentityEnv(t)
	if err := InitNodeIdentity("node:self"); err != nil {
		t.Fatal(err)
	}
	self, err := LoadIdentity("", "")
	if err != nil {
		t.Fatal(err)
	}
	rootPub, rootPriv, _ := ed25519.GenerateKey(nil)

	mk := func(node string, pub ed25519.PublicKey) string {
		c := SignCert(Cert{
			V: CertVersion, Node: node,
			Pub:   base64.StdEncoding.EncodeToString(pub),
			FP:    KeyFingerprint(pub),
			Iat:   time.Now().Add(-time.Minute).Unix(),
			Exp:   time.Now().Add(time.Hour).Unix(),
			Scope: []string{"upload"}, Iss: "wsa-root", KeyID: KeyFingerprint(rootPub),
		}, rootPriv)
		txt, e := EncodeCert(c)
		if e != nil {
			t.Fatal(e)
		}
		return txt
	}

	// 正常证书（主体 id 与本地不同）：服务端按主体绑定 X-WS-Node ⇒ 客户端随主体
	good := mk("wsa-cert-name", self.Pub)
	if _, err := VerifyCert(mustDecode(t, good), rootPub, time.Now()); err != nil {
		t.Fatalf("测试证书本身应当有效: %v", err)
	}
	if err := writeFileAtomic(NodeCertPath(), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New("cn")
	if !c.HasCert() {
		t.Fatalf("应携带证书，实际: %s", c.CertNote())
	}
	if c.NodeID() != "wsa-cert-name" {
		t.Errorf("证书主体是权威口径，节点 id 应为 wsa-cert-name，实际 %q", c.NodeID())
	}

	// 公钥与本地私钥不符的证书 ⇒ 忽略（发出去必然被拒），回到本地身份
	otherPub, _, _ := ed25519.GenerateKey(nil)
	bad := mk("wsa-cert-name", otherPub)
	if err := writeFileAtomic(NodeCertPath(), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	c2 := New("cn")
	if c2.HasCert() {
		t.Error("公钥不匹配的证书必须被忽略")
	}
	if !strings.Contains(c2.CertNote(), "不匹配") {
		t.Errorf("应说明忽略原因，实际: %q", c2.CertNote())
	}
	if c2.NodeID() != "node:self" {
		t.Errorf("忽略证书后应回到本地身份 id，实际 %q", c2.NodeID())
	}
}

func mustDecode(t *testing.T, s string) Cert {
	t.Helper()
	c, err := DecodeCert(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// node.json 优先级：WS_FILES_NODE_ID > WS_NODE_ID > node.json > 主机名兜底
func TestResolveNodeIDPriority(t *testing.T) {
	resetIdentityEnv(t)
	if got := ResolveNodeID(); !strings.HasPrefix(got, "node:") {
		t.Errorf("无任何配置时应退回 node:<hostname>，实际 %q", got)
	}
	if err := writeFileAtomic(NodeMetaPath(), []byte(`{"v":1,"id":"from-meta"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ResolveNodeID(); got != "from-meta" {
		t.Errorf("应取 node.json，实际 %q", got)
	}
	t.Setenv(EnvNodeIDShared, "from-shared-env")
	if got := ResolveNodeID(); got != "from-shared-env" {
		t.Errorf("WS_NODE_ID 应覆盖 node.json，实际 %q", got)
	}
	t.Setenv(EnvNodeID, "from-files-env")
	if got := ResolveNodeID(); got != "from-files-env" {
		t.Errorf("WS_FILES_NODE_ID 应最优先，实际 %q", got)
	}
}

// InitNodeIdentity 绝不覆盖既有身份
func TestInitNodeIdentityNeverOverwrites(t *testing.T) {
	resetIdentityEnv(t)
	if err := InitNodeIdentity("first"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(NodeKeyPath())
	if err := InitNodeIdentity("second"); err == nil {
		t.Fatal("已存在私钥时必须拒绝覆盖")
	}
	after, _ := os.ReadFile(NodeKeyPath())
	if string(before) != string(after) {
		t.Error("既有身份被覆盖了")
	}
	if ReadNodeMeta().ID != "first" {
		t.Error("node.json 不应被第二次调用改写")
	}
}
