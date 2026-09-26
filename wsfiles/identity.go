package wsfiles

// identity.go —— 节点身份的「路径与生命周期」（**与 family 解耦**）
//
// 背景（必须记住的边界）：
//
//	`$WS_PATH/data/family/` 是**文殊家族内部的标识** —— 家族成员名册（registry.json）、
//	家族私钥（id_ed25519）、家族邮箱/peer 记录。**它只存在于我们这套家族节点上**，
//	不是每套文殊系统都会有：客户单机部署只有 ws-core + 网关，根本没有 family 目录。
//
//	而 ws-files 是**面向所有文殊用户**的公共文件服务 ⇒ 它的节点身份
//	**不能**建立在 family 之上。因此本包改用一套中性的节点身份目录：
//
//	$WS_PATH/data/node/id_ed25519       节点私钥（0600；首次使用时自动生成）
//	$WS_PATH/data/node/id_ed25519.pub   公钥（0644）
//	$WS_PATH/data/node/node.json        节点 id / 名字 / 创建时间（生成时写下，保证 id 稳定）
//	$WS_PATH/data/node/node-cert.json   官方签发的节点证书（wst-nodecert install 落盘）
//
// 兼容策略（**family 只读、绝不写入**）：
//
//	选私钥的顺序：WS_FILES_KEY(env) > data/node/ > data/family/（旧路径，家族老节点）
//	选证书的顺序：WS_FILES_CERT(env) > data/node/node-cert.json > data/family/node-cert.json
//
// 家族老节点早就把公钥登记进了名册（L1），继续用 family 私钥即可，行为不变；
// 客户节点没有 family ⇒ 落在 data/node/ 并在首次使用时生成，**零配置**，
// 再由 wst-nodecert 领一张官方证书（L2A）即可上传。

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// EnvNodeDir 节点身份目录覆盖（默认 $WS_PATH/data/node）
	EnvNodeDir = "WS_NODE_DIR"
	// EnvNodeIDShared 节点 id 覆盖（与 ws-core 共用同一变量名）
	EnvNodeIDShared = "WS_NODE_ID"

	nodeKeyName  = "id_ed25519"
	nodeCertName = "node-cert.json"
	nodeMetaName = "node.json"

	// nodeKeyComment 公钥行/私钥注释后缀（纯注释，无安全含义）
	nodeKeyComment = "@wenshu-node"
)

// IdentitySource 私钥来源（日志/排障用：区分「家族旧身份」与「中性节点身份」）
type IdentitySource string

const (
	SourceEnv          IdentitySource = "env"            // WS_FILES_KEY 显式指定
	SourceNodeDir      IdentitySource = "node"           // $WS_PATH/data/node/id_ed25519
	SourceLegacyFamily IdentitySource = "family-legacy"  // $WS_PATH/data/family/id_ed25519（旧，只读）
	SourceGenerated    IdentitySource = "node-generated" // 本次在节点目录新生成
)

// IsLegacyFamily 是否走了旧的 family 路径（仅家族节点会为 true）
func (s IdentitySource) IsLegacyFamily() bool { return s == SourceLegacyFamily }

// NodeDir 节点身份目录：WS_NODE_DIR > $WS_PATH/data/node
func NodeDir() string {
	if v := strings.TrimSpace(configValue(EnvNodeDir)); v != "" {
		return v
	}
	return filepath.Join(wsPath(), "data", "node")
}

// NodeKeyPath 节点私钥路径（中性路径，客户/家族节点通用）
func NodeKeyPath() string { return filepath.Join(NodeDir(), nodeKeyName) }

// NodeCertPath 节点证书路径（中性路径）
func NodeCertPath() string { return filepath.Join(NodeDir(), nodeCertName) }

// NodeMetaPath 节点元数据路径
func NodeMetaPath() string { return filepath.Join(NodeDir(), nodeMetaName) }

// LegacyFamilyKeyPath 旧路径：文殊家族内部标识（**只读**，客户机不存在）
func LegacyFamilyKeyPath() string { return filepath.Join(wsPath(), "data", "family", nodeKeyName) }

// LegacyFamilyCertPath 旧路径：家族节点上的证书
func LegacyFamilyCertPath() string { return filepath.Join(wsPath(), "data", "family", nodeCertName) }

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ResolveKeyPath 选私钥路径（**只读探测，不生成**）：env > node 目录 > 旧 family 目录。
// 三处都没有时返回 ("", "")，由 EnsureKeyPath 决定是否生成。
func ResolveKeyPath() (string, IdentitySource) {
	if v := strings.TrimSpace(configValue(EnvKeyPath)); v != "" {
		return v, SourceEnv
	}
	if fileExists(NodeKeyPath()) {
		return NodeKeyPath(), SourceNodeDir
	}
	if fileExists(LegacyFamilyKeyPath()) {
		return LegacyFamilyKeyPath(), SourceLegacyFamily
	}
	return "", ""
}

// EnsureKeyPath 选私钥路径；三处都没有 ⇒ 在**节点目录**生成一份新身份。
// 这是「客户零配置」的落点：不需要任何人预先放置密钥。
func EnsureKeyPath() (string, IdentitySource, error) {
	if p, src := ResolveKeyPath(); p != "" {
		return p, src, nil
	}
	if err := InitNodeIdentity(""); err != nil {
		return "", "", err
	}
	return NodeKeyPath(), SourceGenerated, nil
}

// NodeMeta node.json 的内容
type NodeMeta struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Created int64  `json:"created,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// ReadNodeMeta 读节点元数据（不存在/损坏都返回零值，不报错 —— 调用方退回主机名口径）
func ReadNodeMeta() NodeMeta {
	b, err := os.ReadFile(NodeMetaPath())
	if err != nil {
		return NodeMeta{}
	}
	var m NodeMeta
	if json.Unmarshal(b, &m) != nil {
		return NodeMeta{}
	}
	m.ID = strings.TrimSpace(m.ID)
	return m
}

// readLegacyFamilyMeID 读旧的 family/me.json 的 id（家族节点兼容用；只读）
func readLegacyFamilyMeID() string {
	b, err := os.ReadFile(filepath.Join(wsPath(), "data", "family", "me.json"))
	if err != nil {
		return ""
	}
	var m struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.ID)
}

// ResolveNodeID 推断本节点 id：
//
//	WS_FILES_NODE_ID > WS_NODE_ID > node.json > 旧 family/me.json > node:<hostname>
//
// 前两个是显式覆盖；node.json 保证「改主机名/迁移机器后 id 不漂移」；
// family/me.json 只对家族老节点生效（**只读**）；最后才是主机名兜底。
func ResolveNodeID() string {
	for _, k := range []string{EnvNodeID, EnvNodeIDShared} {
		if v := strings.TrimSpace(configValue(k)); v != "" {
			return v
		}
	}
	if m := ReadNodeMeta(); m.ID != "" {
		return m.ID
	}
	if id := readLegacyFamilyMeID(); id != "" {
		return id
	}
	return hostFallbackID()
}

func hostFallbackID() string {
	h, _ := os.Hostname()
	h = strings.TrimSpace(h)
	if h == "" {
		return "node:unknown"
	}
	return "node:" + h
}

// InitNodeIdentity 在节点目录生成一份新的 Ed25519 身份（0600）。
// **绝不覆盖既有私钥** —— 身份一旦生成就是这台机器的长期标识，覆盖等于换机器。
// name 为空时用 ResolveNodeID()。
func InitNodeIdentity(name string) error {
	dir := NodeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建节点身份目录失败（%s）: %w", dir, err)
	}
	keyPath := NodeKeyPath()
	if fileExists(keyPath) {
		return fmt.Errorf("节点私钥已存在，拒绝覆盖（%s）", keyPath)
	}
	id := strings.TrimSpace(name)
	if id == "" {
		id = ResolveNodeID()
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成 Ed25519 密钥失败: %w", err)
	}
	if err := writeFileAtomic(keyPath, marshalOpenSSHEd25519(priv, pub, id+nodeKeyComment), 0o600); err != nil {
		return fmt.Errorf("写入节点私钥失败（%s）: %w", keyPath, err)
	}
	// 公钥/元数据失败不算致命（私钥才是身份本体），但仍要尽力写全
	_ = writeFileAtomic(keyPath+".pub", []byte(SSHPubLine(pub, id+nodeKeyComment)+"\n"), 0o644)
	meta := NodeMeta{
		V:       1,
		ID:      id,
		Name:    id,
		Created: time.Now().Unix(),
		Comment: "ws-files 节点身份（本机自动生成；私钥 0600，公钥可公开）",
	}
	if b, mErr := json.MarshalIndent(meta, "", "  "); mErr == nil {
		_ = writeFileAtomic(NodeMetaPath(), append(b, '\n'), 0o644)
	}
	return nil
}

// writeFileAtomic 同目录临时文件 + rename，避免半截文件被当成有效私钥。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ── OpenSSH ed25519 私钥序列化（零第三方依赖，与 ws-core/bustls 同格式）──

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendBlobStr(b, blob []byte) []byte { return appendSSHStr(b, blob) }

// MarshalNodeKey 序列化为 OpenSSH 明文私钥（cipher=none/kdf=none）。
// 导出：wst-nodecert / ws-core 需要与 ws-core 生成格式逐字节一致的私钥文件。
func MarshalNodeKey(priv ed25519.PrivateKey, pub ed25519.PublicKey, comment string) []byte {
	var b []byte
	b = append(b, []byte(openSSHMagic)...)
	b = appendSSHStr(b, []byte("none"))
	b = appendSSHStr(b, []byte("none"))
	b = appendSSHStr(b, nil)
	b = appendU32(b, 1)
	b = appendBlobStr(b, sshPubBlob(pub))
	var p []byte
	p = appendU32(p, 0x12345678)
	p = appendU32(p, 0x12345678)
	p = appendSSHStr(p, []byte("ssh-ed25519"))
	p = appendBlobStr(p, pub)
	p = appendBlobStr(p, priv)
	p = appendSSHStr(p, []byte(comment))
	for i := 1; len(p)%8 != 0; i++ {
		p = append(p, byte(i))
	}
	b = appendBlobStr(b, p)

	enc := base64.StdEncoding.EncodeToString(b)
	var sb strings.Builder
	sb.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for i := 0; i < len(enc); i += 70 {
		j := i + 70
		if j > len(enc) {
			j = len(enc)
		}
		sb.WriteString(enc[i:j] + "\n")
	}
	sb.WriteString("-----END OPENSSH PRIVATE KEY-----\n")
	return []byte(sb.String())
}

// sshPubBlob ssh-ed25519 线格式公钥（与 KeyFingerprint 同口径）
func sshPubBlob(pub ed25519.PublicKey) []byte {
	return appendSSHStr(appendSSHStr(nil, []byte("ssh-ed25519")), pub)
}

// marshalOpenSSHEd25519 包内调用简化名
func marshalOpenSSHEd25519(priv ed25519.PrivateKey, pub ed25519.PublicKey, comment string) []byte {
	return MarshalNodeKey(priv, pub, comment)
}
