package wsfiles

// nodeauth.go —— 客户端侧节点身份签名（wsauth v1）
//
// 目的：**让文殊节点零配置就能往 ws-files 上传**。做法不是配令牌，而是用每台机器
// **本来就有的** Ed25519 身份（`$WS_PATH/data/family/id_ed25519`，ws-core 启动时自动生成），
// 对每个写请求签名。服务端按 名册（L1）或 官方证书（L2A）验签。
//
// 协议（与服务端 ws-files/nodeauth.go 逐字节一致）：
//
//	X-WS-Auth:  v1
//	X-WS-Node:  wsa-qingge
//	X-WS-Ts:    <unix 秒>
//	X-WS-Nonce: <base64url(16B) 一次性>
//	X-WS-Sig:   <base64(Ed25519(私钥, 待签串))>
//	X-WS-Cert:  <可选 base64url(证书 JSON)>   ← 有 data/family/node-cert.json 时自动带上
//
// 待签串：v1 \n METHOD \n path(URL 转义,不含 query) \n ts \n nonce \n hex(sha256(body))
//
// ⚠️ 两条实现纪律：
//  ① **每次尝试（含 429/5xx 重试）都必须重签** —— ts 与 nonce 会变；复用旧签名会被服务端
//     当作重放拒掉。本包的 postJSON 在每次循环内重新 applyAuth，正是为此。
//  ② 私钥只从文件读，**绝不**写进配置/日志/请求（SECURITY/P0）。

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 协议头名（导出：便于网关/工具与测试引用，避免各写一份字面量）
const (
	HeaderAuth  = "X-WS-Auth"
	HeaderNode  = "X-WS-Node"
	HeaderTS    = "X-WS-Ts"
	HeaderNonce = "X-WS-Nonce"
	HeaderSig   = "X-WS-Sig"
	HeaderCert  = "X-WS-Cert"

	// AuthVersion 协议版本
	AuthVersion = "v1"

	// EnvKeyPath 私钥路径覆盖（默认 $WS_PATH/data/family/id_ed25519）
	EnvKeyPath = "WS_FILES_KEY"
	// EnvNodeID 节点 id 覆盖（默认取 me.json 的 id，再退回主机名）
	EnvNodeID = "WS_FILES_NODE_ID"
	// EnvCertPath 证书路径覆盖（默认 $WS_PATH/data/family/node-cert.json）
	EnvCertPath = "WS_FILES_CERT"
)

// Identity 本节点身份（私钥不出内存、不进日志）
type Identity struct {
	Node    string // wsa-xxx
	Priv    ed25519.PrivateKey
	Pub     ed25519.PublicKey
	FP      string // SHA256:…
	KeyPath string
}

// DefaultKeyPath 默认私钥路径：$WS_PATH/data/family/id_ed25519
func DefaultKeyPath() string { return filepath.Join(wsPath(), "data", "family", "id_ed25519") }

// DefaultCertPath 默认证书路径：$WS_PATH/data/family/node-cert.json
func DefaultCertPath() string { return filepath.Join(wsPath(), "data", "family", "node-cert.json") }

// wsPath 运行根：WS_PATH > /etc/environment 的 WS_PATH > /data/app/ws
func wsPath() string {
	if v := strings.TrimSpace(os.Getenv("WS_PATH")); v != "" {
		return v
	}
	if v := readEnvFile("/etc/environment", "WS_PATH"); v != "" {
		return v
	}
	return "/data/app/ws"
}

// LoadIdentity 读取本节点身份。keyPath 为空时用 DefaultKeyPath()；
// nodeID 为空时按 WS_FILES_NODE_ID → me.json 的 id → 主机名 依次推断。
// **不生成密钥**（生成是 ws-core 的职责）：文件不存在即报错，由调用方决定降级方式。
func LoadIdentity(keyPath, nodeID string) (*Identity, error) {
	if strings.TrimSpace(keyPath) == "" {
		keyPath = DefaultKeyPath()
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("读取节点私钥失败（%s）: %w", keyPath, err)
	}
	priv, pub, err := parseOpenSSHEd25519(raw)
	if err != nil {
		return nil, fmt.Errorf("解析节点私钥失败（%s）: %w", keyPath, err)
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID = configValue(EnvNodeID)
	}
	if strings.TrimSpace(nodeID) == "" {
		nodeID = readMeID()
	}
	if strings.TrimSpace(nodeID) == "" {
		h, _ := os.Hostname()
		nodeID = "node:" + h
	}
	return &Identity{Node: nodeID, Priv: priv, Pub: pub, FP: KeyFingerprint(pub), KeyPath: keyPath}, nil
}

// readMeID 读 $WS_PATH/data/family/me.json 的 id 字段
func readMeID() string {
	b, err := os.ReadFile(filepath.Join(wsPath(), "data", "family", "me.json"))
	if err != nil {
		return ""
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.ID)
}

// AuthPayload 待签串（客户端/服务端必须逐字节一致）
func AuthPayload(node string, ts int64, method, path, nonce string, body []byte) []byte {
	bh := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		AuthVersion,
		strings.ToUpper(method),
		path,
		strconv.FormatInt(ts, 10),
		nonce,
		hex.EncodeToString(bh[:]),
	}, "\n"))
}

// NewNonce 一次性随机值（16 字节 → base64url 无填充）
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// SignRequest 给请求附加签名头（body 必须与真正发送的字节完全一致）。
// certText 非空时一并带上 X-WS-Cert。
func (id *Identity) SignRequest(req *http.Request, body []byte, certText string) error {
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	return id.SignRequestWith(req, body, certText, time.Now().Unix(), nonce)
}

// SignRequestWith 用指定 ts/nonce 签名（测试与时钟校准场景）；
// 正常路径一律用 SignRequest（**每次尝试都要新的 ts/nonce**）。
func (id *Identity) SignRequestWith(req *http.Request, body []byte, certText string, ts int64, nonce string) error {
	if id == nil {
		return fmt.Errorf("未加载节点身份")
	}
	payload := AuthPayload(id.Node, ts, req.Method, req.URL.EscapedPath(), nonce, body)
	sig := ed25519.Sign(id.Priv, payload)
	req.Header.Set(HeaderAuth, AuthVersion)
	req.Header.Set(HeaderNode, id.Node)
	req.Header.Set(HeaderTS, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSig, base64.StdEncoding.EncodeToString(sig))
	if strings.TrimSpace(certText) != "" {
		req.Header.Set(HeaderCert, strings.TrimSpace(certText))
	}
	return nil
}

// ── OpenSSH ed25519 私钥解析（零第三方依赖）──
//
// 只支持未加密格式（cipher=none / kdf=none）—— 这正是 ws-core 生成的格式。
// 加密私钥（bcrypt/argon2 等）报错而不是「装作解开了」：宁可不签名，也不静默降级。

const openSSHMagic = "openssh-key-v1\x00"

// ParsePrivateKey 解析私钥字节（支持 OpenSSH 未加密 ed25519 与裸 base64 的 64 字节私钥）。
// 导出给 wst-nodecert 等工具用，避免各处重复实现同一解析。
func ParsePrivateKey(raw []byte) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	return parseOpenSSHEd25519(raw)
}

func parseOpenSSHEd25519(pemBytes []byte) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	text := string(pemBytes)
	const begin = "-----BEGIN OPENSSH PRIVATE KEY-----"
	const end = "-----END OPENSSH PRIVATE KEY-----"
	i := strings.Index(text, begin)
	j := strings.Index(text, end)
	if i < 0 || j < 0 || j <= i {
		return parseRawEd25519(pemBytes) // 兼容裸 base64 的 64 字节私钥
	}
	b64 := strings.Join(strings.Fields(text[i+len(begin):j]), "")
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, nil, fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(blob) < len(openSSHMagic) || string(blob[:len(openSSHMagic)]) != openSSHMagic {
		return nil, nil, fmt.Errorf("不是 OpenSSH 私钥格式")
	}
	p := &blobReader{b: blob, off: len(openSSHMagic)}
	cipherName, err := p.str()
	if err != nil {
		return nil, nil, err
	}
	kdfName, err := p.str()
	if err != nil {
		return nil, nil, err
	}
	if _, err := p.str(); err != nil { // kdfoptions
		return nil, nil, err
	}
	if string(cipherName) != "none" || string(kdfName) != "none" {
		return nil, nil, fmt.Errorf("不支持加密私钥（cipher=%q kdf=%q）—— 请用未加密的 id_ed25519", cipherName, kdfName)
	}
	n, err := p.u32()
	if err != nil {
		return nil, nil, err
	}
	if n != 1 {
		return nil, nil, fmt.Errorf("不支持多密钥（n=%d）", n)
	}
	if _, err := p.str(); err != nil { // public blob
		return nil, nil, err
	}
	privBlob, err := p.str()
	if err != nil {
		return nil, nil, err
	}
	q := &blobReader{b: privBlob}
	if _, err := q.u32(); err != nil { // checkint1
		return nil, nil, err
	}
	if _, err := q.u32(); err != nil { // checkint2
		return nil, nil, err
	}
	keyType, err := q.str()
	if err != nil {
		return nil, nil, err
	}
	if string(keyType) != "ssh-ed25519" {
		return nil, nil, fmt.Errorf("密钥类型非 ssh-ed25519（%q）", keyType)
	}
	pubRaw, err := q.str()
	if err != nil {
		return nil, nil, err
	}
	privRaw, err := q.str()
	if err != nil {
		return nil, nil, err
	}
	if len(pubRaw) != ed25519.PublicKeySize || len(privRaw) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("密钥长度异常（pub=%d priv=%d）", len(pubRaw), len(privRaw))
	}
	return ed25519.PrivateKey(append([]byte{}, privRaw...)), ed25519.PublicKey(append([]byte{}, pubRaw...)), nil
}

func parseRawEd25519(b []byte) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	s := strings.TrimSpace(string(b))
	if k, err := base64.StdEncoding.DecodeString(s); err == nil && len(k) == ed25519.PrivateKeySize {
		return ed25519.PrivateKey(k), ed25519.PublicKey(k[32:]), nil
	}
	return nil, nil, fmt.Errorf("无法识别的私钥格式（应为 OpenSSH 未加密 ed25519）")
}

type blobReader struct {
	b   []byte
	off int
}

func (r *blobReader) u32() (uint32, error) {
	if r.off+4 > len(r.b) {
		return 0, fmt.Errorf("密钥数据截断")
	}
	v := uint32(r.b[r.off])<<24 | uint32(r.b[r.off+1])<<16 | uint32(r.b[r.off+2])<<8 | uint32(r.b[r.off+3])
	r.off += 4
	return v, nil
}

func (r *blobReader) str() ([]byte, error) {
	n, err := r.u32()
	if err != nil {
		return nil, err
	}
	if r.off+int(n) > len(r.b) {
		return nil, fmt.Errorf("密钥数据截断")
	}
	v := r.b[r.off : r.off+int(n)]
	r.off += int(n)
	return v, nil
}
