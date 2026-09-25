// Package wsfiles —— 文殊文件服务（ws-files）客户端。
//
// 用途：把本机文件投递到文殊公共文件服务，取回**公网可下载的 URL**（下载侧无需令牌）。
// 典型场景：
//   - 网关发文件：QQ 开放平台**只收 URL**（由平台服务器自行回源抓取），微信/企业微信/
//     飞书/TG 原生上传失败时，把 URL 作为链接消息发给用户兜底；
//   - LLM 图像/视频识别：多数视觉模型（如商汤）**只接受可公网下载的 http(s) URL**，
//     data URI / 裸 base64 一律报错。
//
// 服务两端（见 ws-files README）：
//
//	境内 https://cn.dl.xiusoft.cn      境外 https://hk.dl.xiusoft.cn
//
// 站点限流：每 IP 600 请求/分钟，超限回 429；接口全部幂等，重试安全（本包已内置退避重试）。
//
// 配置全部走环境变量（SECURITY/P0：令牌只注入环境，不落盘、不进代码）：
//
//	WS_FILES_TOKEN    写操作令牌；为空 ⇒ Enabled()=false，调用方应跳过或降级
//	                  （**只从进程环境读**，SECURITY/P0：密钥不落盘、不进代码）
//	WS_FILES_REGION   cn | hk | auto，默认 auto（按 New 传入的 defaultRegion，另一站兜底）
//	WS_FILES_BASE_CN  默认 https://cn.dl.xiusoft.cn
//	WS_FILES_BASE_HK  默认 https://hk.dl.xiusoft.cn
//	WS_FILES_TTL_MIN  保留分钟数，默认 1440（24h，服务端上限 10080）
//	WS_FILES_TIMEOUT  单请求超时秒数，默认 60
//
// 除令牌外的配置项读取顺序：进程环境 > /etc/environment > $WS_PATH/.env。
package wsfiles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// 默认站点与协议常量（与服务端 /api/info 一致）
const (
	DefaultBaseCN = "https://cn.dl.xiusoft.cn"
	DefaultBaseHK = "https://hk.dl.xiusoft.cn"

	// SingleShotRawMax 经反代「单发」上传的裸数据上限（服务端 single_shot_max_raw）。
	// 超过它必须走分块协议（init → chunk ×N → complete），否则请求体过 Ma 反代会被静默清空。
	SingleShotRawMax = 786048

	// MaxFileSize 服务端单文件上限（2 GiB）
	MaxFileSize = 2 << 30

	// defaultChunkSize 拿不到 /api/info 时的兜底块大小
	defaultChunkSize = 512 << 10

	tokenHeader = "X-WS-Files-Token"
)

// ErrDisabled 未配置令牌（Enabled()=false）
var ErrDisabled = errors.New("未配置 WS_FILES_TOKEN，文殊文件服务不可用")

// Result 一次投递的结果
type Result struct {
	URL       string
	ID        string
	Name      string
	Size      int64
	SHA256    string
	ExpiresAt int64
	Base      string // 实际落地的站点（故障转移后可能是备站）
	Chunked   bool   // 是否走了分块协议
}

// Client 文件服务客户端（并发安全：内部无可变共享状态，除 chunkSize 缓存外）
type Client struct {
	token  string
	baseCN string
	baseHK string
	region string // 强制区域（cn/hk），空则按 defaultRegion
	defReg string
	ttlMin int
	hc     *http.Client
}

// New 按环境变量构造客户端。defaultRegion 是本模块建议的站点（"cn" / "hk"）：
// 境内平台（QQ/微信/企微/飞书）与境内视觉模型建议 "cn"；Telegram/邮件等建议 "hk"。
// WS_FILES_REGION 显式设置时优先级更高。
func New(defaultRegion string) *Client {
	c := &Client{
		token:  strings.TrimSpace(os.Getenv("WS_FILES_TOKEN")),
		baseCN: envStr("WS_FILES_BASE_CN", DefaultBaseCN),
		baseHK: envStr("WS_FILES_BASE_HK", DefaultBaseHK),
		region: strings.ToLower(configValue("WS_FILES_REGION")),
		defReg: strings.ToLower(strings.TrimSpace(defaultRegion)),
		ttlMin: envInt("WS_FILES_TTL_MIN", 1440),
		hc: &http.Client{
			Timeout: time.Duration(envInt("WS_FILES_TIMEOUT", 60)) * time.Second,
		},
	}
	if c.region == "auto" {
		c.region = ""
	}
	return c
}

// Enabled 是否可用（有令牌）
func (c *Client) Enabled() bool { return c != nil && c.token != "" }

// TTLMin 返回生效的保留分钟数
func (c *Client) TTLMin() int {
	if c.ttlMin <= 0 {
		return 1440
	}
	return c.ttlMin
}

// bases 返回按优先级排序的站点列表：偏好站点在前，另一站兜底。
func (c *Client) bases() []string {
	pref := c.region
	if pref != "cn" && pref != "hk" {
		pref = c.defReg
	}
	cn, hk := strings.TrimRight(c.baseCN, "/"), strings.TrimRight(c.baseHK, "/")
	if pref == "hk" {
		return []string{hk, cn}
	}
	return []string{cn, hk}
}

// PrimaryBase 返回首选站点（日志用）
func (c *Client) PrimaryBase() string {
	if b := c.bases(); len(b) > 0 {
		return b[0]
	}
	return ""
}

// Upload 投递本地文件，返回公网 URL。小文件走单发，超过 786048 B 自动走分块。
// 首选站点失败（网络/5xx）时自动改用另一站点；两站皆失败才返回错误。
func (c *Client) Upload(ctx context.Context, localPath, displayName string) (*Result, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或不可读: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("不支持投递目录: %s", filepath.Base(localPath))
	}
	if fi.Size() == 0 {
		return nil, fmt.Errorf("文件内容为空: %s", filepath.Base(localPath))
	}
	if fi.Size() > MaxFileSize {
		return nil, fmt.Errorf("文件过大（%d 字节），上限 %d 字节", fi.Size(), int64(MaxFileSize))
	}
	name := sanitizeName(displayName)
	if name == "" {
		name = sanitizeName(filepath.Base(localPath))
	}
	if name == "" {
		name = "file"
	}

	var errs []string
	for _, base := range c.bases() {
		var res *Result
		var err error
		if fi.Size() <= SingleShotRawMax {
			res, err = c.uploadSingle(ctx, base, localPath, name)
		} else {
			res, err = c.uploadChunked(ctx, base, localPath, name, fi.Size())
		}
		if err == nil {
			res.Base = base
			return res, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", base, err))
	}
	return nil, fmt.Errorf("文件投递失败（已尝试 %d 个站点）: %s", len(errs), strings.Join(errs, "; "))
}

// Delete 删除已投递文件（尽力而为的回收；失败只返回错误，不影响主流程）。
// 不知道文件落在哪一站时用它——依次问两个站点，只有真的删掉才算成功。
// 已知站点（Result.Base）时优先用 DeleteAt，少一次无谓请求。
func (c *Client) Delete(ctx context.Context, id string) error {
	if !c.Enabled() || id == "" {
		return ErrDisabled
	}
	var lastErr error
	for _, base := range c.bases() {
		ok, err := c.deleteAt(ctx, base, id)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return nil
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("未找到文件 %s（可能已过期回收）", id)
}

// DeleteAt 在**指定站点**删除（用投递时返回的 Result.Base，避免在另一站空跑）
func (c *Client) DeleteAt(ctx context.Context, base, id string) error {
	if !c.Enabled() || id == "" {
		return ErrDisabled
	}
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return c.Delete(ctx, id)
	}
	ok, err := c.deleteAt(ctx, base, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("站点 %s 未找到文件 %s（可能已过期回收）", base, id)
	}
	return nil
}

// deleteAt 在单站删除，返回「是否确实删除了一个文件」。
// 注意：服务端对不存在的 id 也回 200（deleted=false），所以必须看 deleted 字段，
// 不能只看状态码——否则「文件其实在另一站」时会被误判为回收成功。
func (c *Client) deleteAt(ctx context.Context, base, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/api/file/"+id, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set(tokenHeader, c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var r apiResp
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode/100 != 2 || !r.OK {
		return false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstNonEmpty(r.Error, strings.TrimSpace(string(raw))))
	}
	return r.Deleted, nil
}

// ---------- 单发 ----------

func (c *Client) uploadSingle(ctx context.Context, base, path, name string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, err
	}
	_ = mw.WriteField("ttl_min", strconv.Itoa(c.TTLMin()))
	if err := mw.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/upload", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(tokenHeader, c.token)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var r apiResp
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode/100 != 2 || !r.OK {
		return nil, fmt.Errorf("单发上传失败 HTTP %d: %s", resp.StatusCode, firstNonEmpty(r.Error, strings.TrimSpace(string(raw))))
	}
	return r.toResult(false)
}

// ---------- 分块 ----------

func (c *Client) uploadChunked(ctx context.Context, base, path, name string, size int64) (*Result, error) {
	chunk := c.chunkSize(ctx, base)
	chunks := int((size + chunk - 1) / chunk)
	sha, err := sha256File(path)
	if err != nil {
		return nil, err
	}

	var initR apiResp
	err = c.postJSON(ctx, base+"/api/upload/init", map[string]any{
		"filename": name,
		"size":     size,
		"sha256":   sha,
		"chunks":   chunks,
		"ttl_min":  c.TTLMin(),
	}, &initR)
	if err != nil {
		return nil, fmt.Errorf("init 失败: %w", err)
	}
	if initR.UploadID == "" {
		return nil, errors.New("init 未返回 upload_id")
	}
	if initR.ChunkSize > 0 {
		chunk = initR.ChunkSize
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, chunk)
	for i := 0; i < chunks; i++ {
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			c.abort(ctx, base, initR.UploadID)
			return nil, fmt.Errorf("读取第 %d 块失败: %w", i, rerr)
		}
		payload := map[string]any{
			"upload_id": initR.UploadID,
			"idx":       i,
			"data":      base64.StdEncoding.EncodeToString(buf[:n]),
		}
		var cr apiResp
		if err := c.postJSON(ctx, base+"/api/upload/chunk", payload, &cr); err != nil {
			c.abort(ctx, base, initR.UploadID)
			return nil, fmt.Errorf("第 %d/%d 块上传失败: %w", i+1, chunks, err)
		}
	}

	var finR apiResp
	if err := c.postJSON(ctx, base+"/api/upload/complete", map[string]any{"upload_id": initR.UploadID}, &finR); err != nil {
		c.abort(ctx, base, initR.UploadID)
		return nil, fmt.Errorf("complete 失败: %w", err)
	}
	res, err := finR.toResult(true)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) chunkSize(ctx context.Context, base string) int64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/info", nil)
	if err != nil {
		return defaultChunkSize
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return defaultChunkSize
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var info struct {
		ChunkSize int64 `json:"chunk_size"`
	}
	if json.Unmarshal(raw, &info) != nil || info.ChunkSize <= 0 {
		return defaultChunkSize
	}
	return info.ChunkSize
}

func (c *Client) abort(ctx context.Context, base, uploadID string) {
	var out apiResp
	_ = c.postJSON(ctx, base+"/api/upload/abort", map[string]any{"upload_id": uploadID}, &out)
}

// ---------- HTTP 基础 ----------

type apiResp struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	URL       string `json:"url"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ExpiresAt int64  `json:"expires_at"`
	UploadID  string `json:"upload_id"`
	ChunkSize int64  `json:"chunk_size"`
	Chunks    int    `json:"chunks"`
	Deleted   bool   `json:"deleted"`
}

func (r apiResp) toResult(chunked bool) (*Result, error) {
	if !r.OK || r.URL == "" {
		return nil, fmt.Errorf("服务端未返回 URL: %s", firstNonEmpty(r.Error, "空响应"))
	}
	return &Result{
		URL: r.URL, ID: r.ID, Name: r.Name, Size: r.Size,
		SHA256: r.SHA256, ExpiresAt: r.ExpiresAt, Chunked: chunked,
	}, nil
}

// postJSON 发送 JSON 并解析回执；429/5xx/网络错误按指数退避重试（接口幂等）。
func (c *Client) postJSON(ctx context.Context, url string, payload any, out *apiResp) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	const maxTries = 6
	var lastErr error
	for try := 0; try < maxTries; try++ {
		if try > 0 {
			wait := time.Duration(1<<uint(try-1)) * time.Second // 1,2,4,8,16s
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(tokenHeader, c.token)

		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue // 网络错误：换一次机会重试
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()

		var r apiResp
		_ = json.Unmarshal(raw, &r)
		if resp.StatusCode/100 == 2 && r.OK {
			*out = r
			return nil
		}
		lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstNonEmpty(r.Error, strings.TrimSpace(string(raw))))
		// 429（限流）与 5xx 可重试；4xx 其余（鉴权/参数/超限）重试无意义
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode/100 != 5 {
			break
		}
	}
	return lastErr
}

// ---------- 工具 ----------

// sha256File 计算整份文件摘要（init 时上报，complete 时服务端校验）
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitizeName 清洗文件名：去目录、去控制字符、限长（服务端还会再清洗一次）
func sanitizeName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == "/" || base == ".." || base == string(filepath.Separator) {
		return ""
	}
	var b strings.Builder
	for _, r := range base {
		if r == unicode.ReplacementChar || r < 0x20 || r == 0x7f {
			continue
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	rs := []rune(strings.Trim(b.String(), " .-_"))
	if len(rs) > 100 {
		rs = rs[:100]
	}
	return string(rs)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func envStr(key, def string) string {
	if v := configValue(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := configValue(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// configValue 读取**非密**配置项：进程环境 > /etc/environment > $WS_PATH/.env。
// （与网关既有的 readSystemEnv 口径一致：supervisor 启动时 WS_PATH 常只写在 /etc/environment。）
//
// ★ 令牌不在此列：**WS_FILES_TOKEN 只从进程环境读取**（SECURITY/P0：密钥不落盘）。
func configValue(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	if v := readEnvFile("/etc/environment", key); v != "" {
		return v
	}
	wsPath := strings.TrimSpace(os.Getenv("WS_PATH"))
	if wsPath == "" {
		wsPath = readEnvFile("/etc/environment", "WS_PATH")
	}
	if wsPath == "" {
		return ""
	}
	return readEnvFile(filepath.Join(wsPath, ".env"), key)
}

// readEnvFile 从 KEY=VALUE 形式的文件里取一个键（忽略注释/空行，去掉引号）
func readEnvFile(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, key+"=") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, key+"="))
		v = strings.Trim(v, `"'`)
		return strings.TrimSpace(v)
	}
	return ""
}
