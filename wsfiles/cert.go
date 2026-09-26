package wsfiles

// cert.go —— 节点证书（wsauth v1 / L2A）与服务端一致的规范定义
//
// 本文件是**协议的唯一客户端实现**：签发（官方根私钥）、验签、编解码公钥都在这里，
// 供 6 个网关、wst-media/wst-video（自动签名）以及 wst-nodecert（领证/签发工具）共用。
// 服务端（ws-files 仓的 cert.go）是同一协议的 Go 副本 —— 改协议时**两边一起改**。
//
// 证书本体（JSON；字段固定，但**签名不覆盖 JSON 文本**，故可安全加字段）：
//
//	{"v":1,"node":"wsa-xxx","pub":"<base64 32B>","fp":"SHA256:…","iat":…,"exp":…,
//	 "scope":["upload"],"iss":"wsa-root","key_id":"SHA256:…","sig":"<base64>"}
//
// 签名载荷（LF 分隔）：
//
//	wsa-cert/v1
//	<node>
//	<pub>
//	<iat>
//	<exp>
//	<scope,逗号分隔且排序>
//	<iss>
//	<key_id>

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CertVersion 当前证书版本
const CertVersion = 1

// Cert 节点证书
type Cert struct {
	V     int      `json:"v"`
	Node  string   `json:"node"`
	Pub   string   `json:"pub"` // base64(std) 32 字节 Ed25519 公钥
	FP    string   `json:"fp"`  // SHA256:…（人工核对/吊销用）
	Iat   int64    `json:"iat"`
	Exp   int64    `json:"exp"` // 0 = 不过期（不推荐）
	Scope []string `json:"scope,omitempty"`
	Iss   string   `json:"iss"`
	KeyID string   `json:"key_id,omitempty"`
	Sig   string   `json:"sig"`
}

// CertPayload 证书签名载荷（固定顺序；scope 排序，保证同一集合只有一种写法）
func CertPayload(c Cert) []byte {
	sc := append([]string(nil), c.Scope...)
	sort.Strings(sc)
	return []byte(strings.Join([]string{
		"wsa-cert/v1",
		c.Node,
		c.Pub,
		strconv.FormatInt(c.Iat, 10),
		strconv.FormatInt(c.Exp, 10),
		strings.Join(sc, ","),
		c.Iss,
		c.KeyID,
	}, "\n"))
}

// SignCert 用根私钥签发证书（写入侧；不校验输入）
func SignCert(c Cert, rootPriv ed25519.PrivateKey) Cert {
	c.V = CertVersion
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(rootPriv, CertPayload(c)))
	return c
}

// VerifyCert 离线验签：根公钥 → 证书 → 节点公钥
func VerifyCert(c Cert, rootPub ed25519.PublicKey, now time.Time) (ed25519.PublicKey, error) {
	if c.V != CertVersion {
		return nil, fmt.Errorf("证书版本不支持（%d，本实现支持 %d）", c.V, CertVersion)
	}
	if strings.TrimSpace(c.Node) == "" {
		return nil, fmt.Errorf("证书缺少 node 字段")
	}
	if len(rootPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("根公钥非法")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.Sig))
	if err != nil {
		return nil, fmt.Errorf("证书签名解码失败")
	}
	if !ed25519.Verify(rootPub, CertPayload(c), sig) {
		return nil, fmt.Errorf("证书签名无效（非该根签发，或内容被改动）")
	}
	if c.Exp > 0 && now.Unix() > c.Exp {
		return nil, fmt.Errorf("证书已过期（exp=%s）", time.Unix(c.Exp, 0).Format(time.RFC3339))
	}
	pub, fp, err := ParsePubKeyValue(c.Pub)
	if err != nil {
		return nil, fmt.Errorf("证书公钥非法: %w", err)
	}
	if c.FP != "" && c.FP != fp {
		return nil, fmt.Errorf("证书公钥指纹不符（声明 %s，实算 %s）", c.FP, fp)
	}
	return pub, nil
}

// EncodeCert 证书 → HTTP 头里的紧凑文本（base64url，无填充）
func EncodeCert(c Cert) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeCert 解析 HTTP 头里的证书文本（宽容接受标准 base64）
func DecodeCert(s string) (Cert, error) {
	var c Cert
	s = strings.TrimSpace(s)
	if s == "" {
		return c, fmt.Errorf("证书为空")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(s); err != nil {
			return c, fmt.Errorf("证书解码失败（应为 base64url(JSON)）")
		}
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("证书 JSON 解析失败: %w", err)
	}
	return c, nil
}

// LoadCertFile 读证书文件（容忍 JSON 文件里带空白），返回可直接塞进请求头的文本
func LoadCertFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	txt := strings.TrimSpace(string(b))
	if txt == "" {
		return "", fmt.Errorf("证书文件为空: %s", path)
	}
	var c Cert
	if json.Unmarshal([]byte(txt), &c) != nil {
		// 已经是 base64url 文本
		if _, derr := DecodeCert(txt); derr != nil {
			return "", fmt.Errorf("证书文件格式无法识别（应为证书 JSON 或 base64url 文本）: %s", path)
		}
		return txt, nil
	}
	return EncodeCert(c)
}

// ── 公钥材料 ──

// ParsePubKeyValue 解析公钥：base64(std) 32 字节裸公钥，或 ssh-ed25519 公钥行
func ParsePubKeyValue(v string) (ed25519.PublicKey, string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, "", fmt.Errorf("公钥为空")
	}
	if strings.HasPrefix(v, "ssh-ed25519 ") || strings.HasPrefix(v, "ecdsa-") || strings.HasPrefix(v, "ssh-") {
		parts := strings.Fields(v)
		if len(parts) < 2 {
			return nil, "", fmt.Errorf("ssh 公钥行格式非法")
		}
		blob, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, "", fmt.Errorf("ssh 公钥 base64 解码失败")
		}
		pub, err := sshEd25519BlobToPub(blob)
		if err != nil {
			return nil, "", err
		}
		return pub, KeyFingerprint(pub), nil
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, "", fmt.Errorf("公钥 base64 解码失败")
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("公钥长度非法（%d，应为 %d）", len(b), ed25519.PublicKeySize)
	}
	pub := ed25519.PublicKey(b)
	return pub, KeyFingerprint(pub), nil
}

// KeyFingerprint 公钥指纹。
//
// ⚠️ 口径是 **OpenSSH 的**：SHA256 覆盖的是 ssh 线格式公钥 blob（"ssh-ed25519" + 32 字节公钥），
// 不是那 32 字节本身。这是因为指纹要能被 `ssh-keygen -lf` 的输出、以及
// family/registry.json 里登记的值直接对上 —— 运维核对/吊销名单都按那个值写。
// （同口径实例：wsa-qingge → SHA256:4gzqnarwjJcXcUU7IpoWmApIHbbaqlK/8sq0TBmwX7E）
func KeyFingerprint(pub ed25519.PublicKey) string {
	blob := appendSSHStr(nil, []byte("ssh-ed25519"))
	blob = appendSSHStr(blob, pub)
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// SSHPubLine 公钥 → ssh-ed25519 公钥行
func SSHPubLine(pub ed25519.PublicKey, comment string) string {
	blob := appendSSHStr(nil, []byte("ssh-ed25519"))
	blob = appendSSHStr(blob, pub)
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		line += " " + comment
	}
	return line
}

func sshEd25519BlobToPub(blob []byte) (ed25519.PublicKey, error) {
	algo, rest, ok := readSSHStr(blob)
	if !ok || string(algo) != "ssh-ed25519" {
		return nil, fmt.Errorf("不是 ssh-ed25519 公钥")
	}
	key, _, ok := readSSHStr(rest)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ssh-ed25519 公钥长度非法")
	}
	return ed25519.PublicKey(key), nil
}

func readSSHStr(b []byte) ([]byte, []byte, bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if n < 0 || len(b) < 4+n {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

func appendSSHStr(b, s []byte) []byte {
	n := len(s)
	b = append(b, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(b, s...)
}
