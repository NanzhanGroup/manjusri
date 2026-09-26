package wsfiles

// enroll.go —— 节点领证（L2A）的「信封」层：**申请 → 人工审批 → 加密回信 → 安装**
//
// 为什么是这样（09-17 定的方案 C，本文件是它的落地）：
//
//	新节点手上没有任何凭据，却要证明「我是正经文殊节点」。不新开公网写入面，
//	而是复用已经跑在生产上的三套体系：
//
//	  ① 身份码 WSA-IDENT:v1 —— 与 `wst-ident` 工具**同一格式**：官方把它粘进
//	     `wst-ident verify --code …` 就能得到 签名通过/注册表比对 的判定，无需新工具。
//	  ② 邮件通道           —— 申请与回信都是**普通邮件**（正文即信封），
//	     零新增公网面；客户用自己的邮箱发即可，不要求他有 @wsai.chat 信箱。
//	  ③ 人工审批           —— 官方侧必须显式 approve 才签发（`ws-approve` 可用时同步登记）。
//
// 三条硬要求（09-17 原文）在本文件的落点：
//
//	① 明文凭据不裸传：节点**首次使用即本地生成** X25519 密钥对（data/node/enroll_x25519），
//	   申请里只带**公钥**；回信用它加密（X25519 ECDH + HKDF-SHA256 + AES-256-GCM）。
//	   私钥不出机、官方侧不存任何明文。
//	② 白名单/名册随批准生效：签发即授权，官方侧 approve 时才产出证书（ws-files 服务端
//	   用 --trust-root 离线验签，无需逐节点登记）；批准失败的申请什么都不产出。
//	③ 可续期可吊销：证书带 exp（续期＝再走一次 apply→install）；吊销＝服务端吊销名单
//	   写证书指纹（fp），热重载即生效。
//
// 信封（邮件正文，两段都是「键: 值」行格式，可放在 ``` 围栏里）：
//
//	WSA-IDENT:v1                       ← 段①：通用身份码（wst-ident 同格式），既有工具可直接验
//	id: node:qingge
//	name: 清歌
//	email: qingge@wsai.chat
//	key: ssh-ed25519 AAAA… 清歌@wenshu-node
//	fp: SHA256:…
//	sig: <base64 Ed25519(私钥, "id|name|email|key|fp")>
//
//	WSA-NODECERT:v1                    ← 段②：领证请求（含回信加密公钥与授权范围）
//	id: node:qingge
//	name: 清歌
//	email: qingge@wsai.chat
//	pub: ssh-ed25519 AAAA… 清歌@wenshu-node
//	fp: SHA256:…
//	enc: <base64(std) 32B X25519 公钥>
//	want: upload,delete
//	days: 365
//	host: qingge
//	ts: 1789…
//	sig: <base64 Ed25519(私钥, 见 EnrollPayload())>
//
// 回信（官方 → 节点）：
//
//	WSA-NODECERT-REPLY:v1
//	node: node:qingge
//	fp: SHA256:…            ← 证书主体指纹
//	iss: wsa-root
//	root_fp: SHA256:…       ← 签发根指纹（与本地 --trust-root 比对）
//	exp: 2027-…
//	eph: <base64 32B 服务端临时 X25519 公钥>
//	nonce: <base64 12B>
//	sha: <hex sha256(证书明文)>
//	ct: <base64 AES-256-GCM 密文>
//	ts: 1789…
//
// ⚠️ 本包**只用标准库**（manjusri 的依赖面刻意保持最小）：crypto/ecdh、crypto/hkdf、
// crypto/aes、crypto/cipher 都是标准库，无第三方加密依赖。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 信封与主题（导出：wst-nodecert / wst-mail-handle / 文档共用同一字面量）
const (
	// EnrollHeader 领证申请信封头
	EnrollHeader = "WSA-NODECERT:v1"
	// EnrollReplyHeader 领证回信信封头
	EnrollReplyHeader = "WSA-NODECERT-REPLY:v1"
	// IdentHeader 通用身份码头（与 wst-ident 工具逐字符一致）
	IdentHeader = "WSA-IDENT:v1"
	// EnrollSubject 申请邮件主题（wst-mail-handle 也按它路由）
	EnrollSubject = "[NODECERT] 文殊节点领证申请"
	// EnrollReplySubject 回信主题
	EnrollReplySubject = "[NODECERT] 领证结果（用 wst-nodecert install 安装）"
	// EnrollIssuerDefault 默认签发者标识
	EnrollIssuerDefault = "wsa-root"
	// EnrollDefaultDays 默认证书有效期（天）
	EnrollDefaultDays = 365
	// EnrollMaxAge 申请/回信可接受的时间偏移（邮件会慢、审批可能隔天，故放宽到 30 天）
	EnrollMaxAge = 30 * 24 * time.Hour
)

// EnrollWantAllowed 允许申请的授权范围（与 ws-files 服务端一致）
var EnrollWantAllowed = []string{"upload", "delete"}

// enrollKeyName X25519 领证密钥文件名（与身份私钥同目录，中性路径）
const enrollKeyName = "enroll_x25519"

// EnrollKeyPath 领证加密私钥路径：$WS_PATH/data/node/enroll_x25519
func EnrollKeyPath() string { return filepath.Join(NodeDir(), enrollKeyName) }

// ── 信封解析（通用的「头 + 键:值」块）──

// stripFences 去掉 markdown 代码围栏行，并把引用行（>）视为不存在（邮件回复常带引文）
func stripFences(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	for _, ln := range strings.Split(text, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			continue
		}
		if strings.HasPrefix(t, ">") { // 引用的原文：不算有效信封内容
			continue
		}
		out = append(out, ln)
	}
	return out
}

// envelopeKV 取出 header 段（到下一个已知信封头/空行结尾为止）的键值对。
// 找不到 header 时 ok=false。重复键取**第一次**出现（引文已由 stripFences 剥掉）。
func envelopeKV(text, header string, stops []string) (map[string]string, bool) {
	lines := stripFences(text)
	start := -1
	for i, ln := range lines {
		if strings.TrimSpace(ln) == header || strings.Contains(ln, header) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}
	kv := map[string]string{}
	for _, ln := range lines[start+1:] {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		for _, s := range stops {
			if strings.Contains(t, s) && !strings.Contains(t, ":") {
				return kv, true
			}
			if strings.HasPrefix(t, s) {
				return kv, true
			}
		}
		i := strings.Index(t, ":")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(t[:i])
		v := strings.TrimSpace(t[i+1:])
		if _, dup := kv[k]; !dup {
			kv[k] = v
		}
	}
	return kv, true
}

// sanitizeField 去掉会破坏「行/管道」结构的字符（换行、回车、竖线）
func sanitizeField(v string) string {
	r := strings.NewReplacer("\r", " ", "\n", " ", "|", "/")
	v = r.Replace(v)
	return strings.TrimSpace(v)
}

func splitWant(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func normalizeWant(w []string) ([]string, error) {
	var out []string
	for _, p := range w {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		ok := false
		for _, a := range EnrollWantAllowed {
			if p == a {
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("不支持的授权范围 %q（只允许 %s）", p, strings.Join(EnrollWantAllowed, "/"))
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		out = append([]string(nil), EnrollWantAllowed...)
	}
	sort.Strings(out)
	// 去重
	dedup := out[:0]
	for i, v := range out {
		if i == 0 || v != out[i-1] {
			dedup = append(dedup, v)
		}
	}
	return dedup, nil
}

// ── ① 身份码（WSA-IDENT:v1，与 wst-ident 同格式）──

// IdentityCodePayload 身份码签名载荷（与 wst-ident 的 gen/verify 逐字符一致）
func IdentityCodePayload(id, name, email, pubLine, fp string) []byte {
	return []byte(strings.Join([]string{id, name, email, pubLine, fp}, "|"))
}

// BuildIdentityCode 用**本节点身份**生成一段标准 WSA-IDENT:v1 身份码。
// 官方可以直接 `wst-ident parse|verify --code …` 处理它 —— 这就是「复用现成身份码」。
func BuildIdentityCode(id *Identity, name, email string) (string, error) {
	if id == nil || len(id.Priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("节点身份不可用")
	}
	name, email = sanitizeField(name), sanitizeField(email)
	pubLine := SSHPubLine(id.Pub, id.Node+nodeKeyComment)
	sig := base64.StdEncoding.EncodeToString(
		ed25519.Sign(id.Priv, IdentityCodePayload(id.Node, name, email, pubLine, id.FP)))
	return strings.Join([]string{
		IdentHeader,
		"id: " + id.Node,
		"name: " + name,
		"email: " + email,
		"key: " + pubLine,
		"fp: " + id.FP,
		"sig: " + sig,
	}, "\n"), nil
}

// VerifyIdentityCode 校验身份码段（自包含：只用码里的公钥验签，不看名册）
func VerifyIdentityCode(text string) (id, name, email, pubLine, fp string, err error) {
	kv, ok := envelopeKV(text, IdentHeader, []string{EnrollHeader, EnrollReplyHeader})
	if !ok {
		return "", "", "", "", "", fmt.Errorf("未找到 %s 段", IdentHeader)
	}
	id, name, email = kv["id"], kv["name"], kv["email"]
	pubLine, fp, sigB64 := kv["key"], kv["fp"], kv["sig"]
	if id == "" || pubLine == "" || sigB64 == "" {
		return "", "", "", "", "", fmt.Errorf("身份码不完整（缺 id/key/sig）")
	}
	pub, calcFP, perr := ParsePubKeyValue(pubLine)
	if perr != nil {
		return "", "", "", "", "", fmt.Errorf("身份码公钥非法: %w", perr)
	}
	if fp != "" && fp != calcFP {
		return "", "", "", "", "", fmt.Errorf("身份码公钥指纹不符（声明 %s，实算 %s）", fp, calcFP)
	}
	sig, derr := base64.StdEncoding.DecodeString(sigB64)
	if derr != nil {
		return "", "", "", "", "", fmt.Errorf("身份码签名解码失败")
	}
	if !ed25519.Verify(pub, IdentityCodePayload(id, name, email, pubLine, fp), sig) {
		return "", "", "", "", "", fmt.Errorf("身份码签名无效（被篡改或伪造）")
	}
	return id, name, email, pubLine, calcFP, nil
}

// ── ② 回信加密密钥（X25519；本地生成、私钥不出机）──

// NewEnrollKeyPair 生成一对 X25519 领证密钥（priv 32B / pub 32B）
func NewEnrollKeyPair() (priv, pub []byte, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 X25519 密钥失败: %w", err)
	}
	return k.Bytes(), k.PublicKey().Bytes(), nil
}

// OpenEnrollKey 读取本机领证加密私钥；不存在则**在 data/node 生成一份**（0600）。
// 与节点身份一样：绝不覆盖既有私钥（换了私钥，之前未取回的回信就解不开了）。
func OpenEnrollKey() (priv []byte, pub []byte, path string, generated bool, err error) {
	path = EnrollKeyPath()
	if b, rerr := os.ReadFile(path); rerr == nil {
		raw, derr := decodeKey32(string(b))
		if derr != nil || len(raw) != 32 {
			return nil, nil, path, false, fmt.Errorf("领证私钥文件损坏（%s）: 期望 32 字节 X25519 私钥", path)
		}
		p, perr := ecdh.X25519().NewPrivateKey(raw)
		if perr != nil {
			return nil, nil, path, false, fmt.Errorf("领证私钥非法（%s）: %w", path, perr)
		}
		return raw, p.PublicKey().Bytes(), path, false, nil
	}
	priv, pub, gerr := NewEnrollKeyPair()
	if gerr != nil {
		return nil, nil, path, false, gerr
	}
	if werr := writeFileAtomic(path, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); werr != nil {
		return nil, nil, path, false, fmt.Errorf("写入领证私钥失败（%s）: %w", path, werr)
	}
	_ = writeFileAtomic(path+".pub", []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644)
	return priv, pub, path, true, nil
}

// decodeB64Any 解码 base64（std/rawurl）或 hex —— 长度不设限（nonce 等短值也用）
func decodeB64Any(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return hex.DecodeString(s)
}

// decodeKey32 解码并校验 32 字节的密钥材料（X25519 公钥/私钥）
func decodeKey32(s string) ([]byte, error) {
	b, err := decodeB64Any(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("期望 32 字节密钥，实际 %d 字节", len(b))
	}
	return b, nil
}

// ── ③ 申请信封 ──

// EnrollRequest 一条领证申请（解析后的形态）
type EnrollRequest struct {
	ID    string
	Name  string
	Email string
	Pub   string // ssh-ed25519 行
	FP    string
	Enc   string // base64(std) 32B X25519 公钥
	Want  []string
	Days  int
	Host  string
	TS    int64
	Sig   string

	IdentCode    string // 段①原文（保留，便于人工/既有工具核对）
	IdentName    string
	IdentEmail   string
	IdentPresent bool // 邮件里是否附了 WSA-IDENT 段
	IdentFPValid bool // 段①签名是否通过
}

// EnrollPayload 领证请求的签名载荷（固定顺序；want 排序，保证同一集合只有一种写法）
func EnrollPayload(id, name, email, pubLine, fp, enc string, want []string, days int, host string, ts int64) []byte {
	w, _ := normalizeWant(want)
	return []byte(strings.Join([]string{
		"wsa-nodecert/v1",
		id, name, email, pubLine, fp, enc,
		strings.Join(w, ","),
		strconv.Itoa(days),
		host,
		strconv.FormatInt(ts, 10),
	}, "\n"))
}

// BuildEnrollRequest 生成申请信封（含身份码段）。encPub 为本机 X25519 公钥（32B）。
func BuildEnrollRequest(id *Identity, encPub []byte, name, email string, want []string, days int, host string) (string, error) {
	if id == nil {
		return "", fmt.Errorf("节点身份不可用")
	}
	if len(encPub) != 32 {
		return "", fmt.Errorf("领证加密公钥非法（期望 32 字节）")
	}
	name, email, host = sanitizeField(name), sanitizeField(email), sanitizeField(host)
	if email == "" {
		return "", fmt.Errorf("必须提供回信邮箱（申请邮件里要写清零信地址）")
	}
	w, err := normalizeWant(want)
	if err != nil {
		return "", err
	}
	if days == 0 {
		days = EnrollDefaultDays
	}
	if days < 0 {
		return "", fmt.Errorf("有效期天数不能为负")
	}
	pubLine := SSHPubLine(id.Pub, id.Node+nodeKeyComment)
	enc := base64.StdEncoding.EncodeToString(encPub)
	ts := time.Now().Unix()
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(id.Priv,
		EnrollPayload(id.Node, name, email, pubLine, id.FP, enc, w, days, host, ts)))
	ident, err := BuildIdentityCode(id, name, email)
	if err != nil {
		return "", err
	}
	return ident + "\n\n" + strings.Join([]string{
		EnrollHeader,
		"id: " + id.Node,
		"name: " + name,
		"email: " + email,
		"pub: " + pubLine,
		"fp: " + id.FP,
		"enc: " + enc,
		"want: " + strings.Join(w, ","),
		"days: " + strconv.Itoa(days),
		"host: " + host,
		"ts: " + strconv.FormatInt(ts, 10),
		"sig: " + sig,
	}, "\n"), nil
}

// ParseEnrollRequest 解析申请信封（不做可信性判断，校验请用 VerifyEnrollRequest）
func ParseEnrollRequest(text string) (*EnrollRequest, error) {
	kv, ok := envelopeKV(text, EnrollHeader, []string{IdentHeader, EnrollReplyHeader})
	if !ok {
		return nil, fmt.Errorf("未找到 %s 段（这不像一封领证申请）", EnrollHeader)
	}
	ts, _ := strconv.ParseInt(strings.TrimSpace(kv["ts"]), 10, 64)
	days, _ := strconv.Atoi(strings.TrimSpace(kv["days"]))
	req := &EnrollRequest{
		ID: kv["id"], Name: kv["name"], Email: kv["email"],
		Pub: kv["pub"], FP: kv["fp"], Enc: kv["enc"],
		Want: splitWant(kv["want"]), Days: days, Host: kv["host"], TS: ts, Sig: kv["sig"],
	}
	if req.Days == 0 {
		req.Days = EnrollDefaultDays
	}
	// 段①（身份码）为可选但**推荐**：有就一并校验，让申请自带一份既有工具可读的身份码
	req.IdentPresent = strings.Contains(text, IdentHeader)
	if id, name, email, _, _, err := VerifyIdentityCode(text); err == nil {
		req.IdentCode, req.IdentName, req.IdentEmail, req.IdentFPValid = id, name, email, true
	}
	return req, nil
}

// VerifyEnrollRequest 校验申请的可信性（签名/字段/时间窗）。返回错误即**必须拒绝**。
func VerifyEnrollRequest(req *EnrollRequest, now time.Time) error {
	if req == nil {
		return fmt.Errorf("申请为空")
	}
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("申请缺少 node id")
	}
	if strings.TrimSpace(req.Enc) == "" {
		return fmt.Errorf("申请缺少回信加密公钥（enc）")
	}
	if _, err := decodeKey32(req.Enc); err != nil {
		return fmt.Errorf("回信加密公钥非法: %w", err)
	}
	if _, err := normalizeWant(req.Want); err != nil {
		return err
	}
	if req.Days < 0 {
		return fmt.Errorf("有效期天数非法（%d）", req.Days)
	}
	pub, calcFP, err := ParsePubKeyValue(req.Pub)
	if err != nil {
		return fmt.Errorf("节点公钥非法: %w", err)
	}
	if req.FP != "" && req.FP != calcFP {
		return fmt.Errorf("节点公钥指纹不符（声明 %s，实算 %s）—— 申请可能被篡改", req.FP, calcFP)
	}
	if strings.TrimSpace(req.Sig) == "" {
		return fmt.Errorf("申请缺少签名")
	}
	sig, err := base64.StdEncoding.DecodeString(req.Sig)
	if err != nil {
		return fmt.Errorf("申请签名解码失败")
	}
	if !ed25519.Verify(pub, EnrollPayload(req.ID, req.Name, req.Email, req.Pub, req.FP, req.Enc, req.Want, req.Days, req.Host, req.TS), sig) {
		return fmt.Errorf("申请签名无效（被篡改或伪造）—— 拒绝")
	}
	if req.TS <= 0 {
		return fmt.Errorf("申请缺少时间戳")
	}
	if d := now.Sub(time.Unix(req.TS, 0)); d > EnrollMaxAge || d < -EnrollMaxAge {
		return fmt.Errorf("申请时间戳超出可接受窗口（±%s）：%s", EnrollMaxAge,
			time.Unix(req.TS, 0).Format(time.RFC3339))
	}
	// 段①（身份码）：**存在就必须有效**。附了一段验不过的身份码 = 申请被动过手脚，直接拒
	// （否则攻击者可以塞一段伪造身份码来误导人工审批，而机器侧却静默放过）。
	if req.IdentPresent && !req.IdentFPValid {
		return fmt.Errorf("申请附带了身份码，但身份码签名校验失败 —— 申请已被篡改或伪造，拒绝")
	}
	// 段①与段②必须指向**同一节点**（不许"半真半假"）
	if req.IdentCode != "" && req.IdentFPValid && req.IdentCode != req.ID {
		return fmt.Errorf("身份码节点（%s）与请求节点（%s）不一致", req.IdentCode, req.ID)
	}
	return nil
}

// EnrollRequestSummary 人类可读的申请摘要（审批时给管理员看）
func EnrollRequestSummary(req *EnrollRequest) string {
	ident := "未附身份码"
	if req.IdentFPValid {
		ident = "身份码签名通过（" + req.IdentCode + "）"
	} else if req.IdentCode == "" {
		ident = "未附身份码（仅请求块签名，已足够证明持有私钥）"
	} else {
		ident = "⚠ 身份码存在但**校验失败**"
	}
	return fmt.Sprintf("节点 %s / 名称 %s / 回信邮箱 %s\n公钥指纹 %s\n申请范围 %s / 有效期 %d 天\n主机 %s\n身份码：%s",
		req.ID, req.Name, req.Email, req.FP,
		strings.Join(req.Want, ","), req.Days, req.Host, ident)
}

// ── ④ 回信加密/解密 ──

// hkdfKey 用 HKDF-SHA256 从 ECDH 共享密钥派生 32 字节密钥。
// salt = 双方公钥拼接（绑定上下文）；info = 协议标签。
func hkdfKey(shared, nodePub, ephPub []byte) ([]byte, error) {
	salt := append(append([]byte(nil), nodePub...), ephPub...)
	return hkdf.Key(sha256.New, shared, salt, "ws-files nodecert reply v1", 32)
}

func ecdhShared(privRaw, peerPubRaw []byte) ([]byte, error) {
	priv, err := ecdh.X25519().NewPrivateKey(privRaw)
	if err != nil {
		return nil, fmt.Errorf("私钥非法: %w", err)
	}
	peer, err := ecdh.X25519().NewPublicKey(peerPubRaw)
	if err != nil {
		return nil, fmt.Errorf("对端公钥非法: %w", err)
	}
	return priv.ECDH(peer)
}

// EncryptEnrollReply 用节点公钥加密证书，产出回信信封。
// node：节点 id；nodeEncPub：申请里的 enc（32B）；certText：证书全文；
// fp/iss/rootFP/exp：人工核对用元信息。
func EncryptEnrollReply(node string, nodeEncPub []byte, certText, fp, iss, rootFP string, exp int64) (string, error) {
	if len(nodeEncPub) != 32 {
		return "", fmt.Errorf("节点加密公钥非法（期望 32 字节）")
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("生成临时密钥失败: %w", err)
	}
	shared, err := eph.ECDH(mustPub(nodeEncPub))
	if err != nil {
		return "", fmt.Errorf("ECDH 失败: %w", err)
	}
	key, err := hkdfKey(shared, nodeEncPub, eph.PublicKey().Bytes())
	if err != nil {
		return "", fmt.Errorf("派生密钥失败: %w", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	aad := []byte(EnrollReplyHeader + "|" + node)
	ct := gcm.Seal(nil, nonce, []byte(certText), aad)
	sum := sha256.Sum256([]byte(certText))
	expStr := ""
	if exp > 0 {
		expStr = time.Unix(exp, 0).Format(time.RFC3339)
	}
	return strings.Join([]string{
		EnrollReplyHeader,
		"node: " + node,
		"fp: " + fp,
		"iss: " + iss,
		"root_fp: " + rootFP,
		"exp: " + expStr,
		"eph: " + base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		"nonce: " + base64.StdEncoding.EncodeToString(nonce),
		"sha: " + hex.EncodeToString(sum[:]),
		"ct: " + base64.StdEncoding.EncodeToString(ct),
		"ts: " + strconv.FormatInt(time.Now().Unix(), 10),
	}, "\n"), nil
}

func mustPub(raw []byte) *ecdh.PublicKey {
	p, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil
	}
	return p
}

// EnrollReplyMeta 回信里的明文字段（用于人工核对/展示）
type EnrollReplyMeta struct {
	Node   string
	FP     string
	Iss    string
	RootFP string
	Exp    string
	TS     int64
	Sha    string
}

// DecryptEnrollReply 用本机领证私钥解密回信，返回证书全文与元信息。
// 校验：节点主体一致、sha256(证书) 与明文声明一致（GCM 已保证完整性，这里做双重核对）。
func DecryptEnrollReply(nodeEncPriv []byte, text string) (string, *EnrollReplyMeta, error) {
	kv, ok := envelopeKV(text, EnrollReplyHeader, nil)
	if !ok {
		return "", nil, fmt.Errorf("未找到 %s 段（这不是领证回信）", EnrollReplyHeader)
	}
	meta := &EnrollReplyMeta{
		Node: kv["node"], FP: kv["fp"], Iss: kv["iss"], RootFP: kv["root_fp"],
		Exp: kv["exp"], Sha: kv["sha"],
	}
	meta.TS, _ = strconv.ParseInt(strings.TrimSpace(kv["ts"]), 10, 64)

	eph, err := decodeKey32(kv["eph"])
	if err != nil {
		return "", nil, fmt.Errorf("回信临时公钥非法: %w", err)
	}
	nonce, err := decodeB64Any(kv["nonce"])
	if err != nil {
		return "", nil, fmt.Errorf("回信 nonce 非法")
	}
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kv["ct"]))
	if err != nil {
		return "", nil, fmt.Errorf("回信密文解码失败")
	}
	shared, err := ecdhShared(nodeEncPriv, eph)
	if err != nil {
		return "", nil, fmt.Errorf("ECDH 失败: %w", err)
	}
	// salt 用「本机公钥 || 服务端临时公钥」，故需先算出本机公钥
	myPub, perr := pubFromPriv(nodeEncPriv)
	if perr != nil {
		return "", nil, perr
	}
	key, err := hkdfKey(shared, myPub, eph)
	if err != nil {
		return "", nil, fmt.Errorf("派生密钥失败: %w", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return "", nil, fmt.Errorf("回信 nonce 长度非法（%d）", len(nonce))
	}
	aad := []byte(EnrollReplyHeader + "|" + meta.Node)
	pt, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", nil, fmt.Errorf("解密失败（回信不是发给本机、或已被改动）: %w", err)
	}
	sum := sha256.Sum256(pt)
	if meta.Sha != "" && !strings.EqualFold(meta.Sha, hex.EncodeToString(sum[:])) {
		return "", nil, fmt.Errorf("证书内容与回信声明的 sha256 不符 —— 拒绝安装")
	}
	return string(pt), meta, nil
}

func pubFromPriv(privRaw []byte) ([]byte, error) {
	p, err := ecdh.X25519().NewPrivateKey(privRaw)
	if err != nil {
		return nil, fmt.Errorf("领证私钥非法: %w", err)
	}
	return p.PublicKey().Bytes(), nil
}

// EnrollRequestID 申请的稳定短编号（审批台账/回信主题用）：
// sha256(签名载荷) 前 4 字节 hex —— 同一条申请永远得到同一个编号（幂等），
// 且任何字段（含 enc/权限/有效期）被改动都会得到不同编号。
func EnrollRequestID(req *EnrollRequest) string {
	if req == nil {
		return ""
	}
	sum := sha256.Sum256(EnrollPayload(req.ID, req.Name, req.Email, req.Pub, req.FP, req.Enc, req.Want, req.Days, req.Host, req.TS))
	return hex.EncodeToString(sum[:4])
}

// FormatEnrollRequestMail 把申请信封包装成一封可直接发出的邮件（纯文本正文）
func FormatEnrollRequestMail(reqText, to string) string {
	var sb strings.Builder
	sb.WriteString("这是一封文殊节点领证申请（ws-files 文件服务写权限）。\n")
	sb.WriteString("申请内容由本机节点私钥签名，回信请加密给申请里的 enc 公钥。\n\n")
	sb.WriteString("```\n" + reqText + "\n```\n\n")
	sb.WriteString("说明：\n")
	sb.WriteString("- 身份码段（WSA-IDENT:v1）可用 `wst-ident verify --code …` 直接校验；\n")
	sb.WriteString("- 请求段（WSA-NODECERT:v1）签名覆盖节点公钥/加密公钥/授权范围/有效期；\n")
	sb.WriteString("- 本申请不含任何私钥。\n")
	if to != "" {
		sb.WriteString("\n收件人：" + to + "\n")
	}
	return sb.String()
}

// FormatEnrollReplyMail 把回信信封包装成一封可直接发出的邮件（纯文本正文）
func FormatEnrollReplyMail(replyText, node, id string) string {
	var sb strings.Builder
	sb.WriteString("领证结果如下：证书已用你申请里的加密公钥加密，只有该节点的私钥能解开。\n")
	sb.WriteString("在节点上执行 `wst-nodecert install` 并附上本封邮件正文即可安装（会先验签）。\n\n")
	sb.WriteString("```\n" + replyText + "\n```\n\n")
	if id != "" {
		sb.WriteString("申请编号：" + id + "\n")
	}
	if node != "" {
		sb.WriteString("节点：" + node + "\n")
	}
	sb.WriteString("\n提示：本封邮件**不含明文凭据**，改一个字符都会导致解密/验签失败。\n")
	return sb.String()
}
