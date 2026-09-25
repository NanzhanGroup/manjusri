package wsfiles

// mode.go —— 文件交付模式（网关共用）
//
// 网关把文件交给用户的路径有两条：
//
//	native : 平台原生附件上传（微信 iLink CDN / TG sendDocument / 飞书 upload_all …）；QQ 特殊，
//	         平台只收 URL，由「平台回源抓取」后仍是真附件。
//	link   : 一律投递到 ws-files，把公网 URL 作为消息发给用户（永不依赖平台上传能力）。
//
// WS_FILE_DELIVERY 选择策略：
//
//	auto（默认）: 先试原生；**失败的文件**自动回退为 ws-files 链接（既修失败又不丢体验）
//	link        : 一律走 ws-files 链接（完全绕开平台上传，最稳但用户看到的是链接）
//	native      : 只用原生（旧行为，失败即报错）
//
// 之所以默认 auto：它能把「发送文件失败」修掉（失败一定有链接兜底），同时不改变
// 原生本就能用的平台的体验。要强制全部走链接，设 WS_FILE_DELIVERY=link 即可，无需改代码。

import (
	"fmt"
	"strings"
)

// EnvDeliveryMode 交付模式环境变量名
const EnvDeliveryMode = "WS_FILE_DELIVERY"

// Mode 文件交付模式
type Mode string

const (
	// ModeAuto 原生优先，失败回退链接（默认）
	ModeAuto Mode = "auto"
	// ModeLink 一律投递 ws-files，链接交付
	ModeLink Mode = "link"
	// ModeNative 只用平台原生上传
	ModeNative Mode = "native"
)

// DeliveryMode 读取 WS_FILE_DELIVERY（缺省/非法一律 auto，非法值由调用方记日志更好，
// 这里保守取最安全且不改变体验的 auto）。
func DeliveryMode() Mode {
	switch Mode(strings.ToLower(configValue(EnvDeliveryMode))) {
	case ModeLink:
		return ModeLink
	case ModeNative:
		return ModeNative
	default:
		return ModeAuto
	}
}

// UsesLink 该模式下是否需要 ws-files 链接能力（auto/link 都要）
func (m Mode) UsesLink() bool { return m == ModeAuto || m == ModeLink }

// NativeFirst 该模式下是否先尝试平台原生上传
func (m Mode) NativeFirst() bool { return m == ModeAuto || m == ModeNative }

func (m Mode) String() string { return string(m) }

// Link 一个待交付的链接（Err 非空表示投递失败，只能把失败原因告诉用户）
type Link struct {
	Name      string
	URL       string
	Size      int64
	ExpiresAt int64
	Err       error
}

// FormatLinks 生成发给用户的链接文本。title 为空时用默认首行。
func FormatLinks(title string, links []Link) string {
	if len(links) == 0 {
		return ""
	}
	var ok, bad []Link
	for _, l := range links {
		if l.URL != "" {
			ok = append(ok, l)
		} else {
			bad = append(bad, l)
		}
	}
	var sb strings.Builder
	if title == "" {
		title = "📎 文件下载链接（24 小时内有效）"
	}
	if len(ok) > 0 {
		sb.WriteString(title)
		for _, l := range ok {
			sb.WriteString("\n· ")
			sb.WriteString(l.Name)
			if l.Size > 0 {
				sb.WriteString("（")
				sb.WriteString(HumanSize(l.Size))
				sb.WriteString("）")
			}
			sb.WriteString("\n  ")
			sb.WriteString(l.URL)
		}
	}
	for _, l := range bad {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		reason := "投递失败"
		if l.Err != nil {
			reason = l.Err.Error()
		}
		sb.WriteString(fmt.Sprintf("⚠️ %s 发送失败：%s", l.Name, reason))
	}
	return sb.String()
}

// HumanSize 人类可读体积
func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
