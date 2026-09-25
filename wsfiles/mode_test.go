package wsfiles

import (
	"errors"
	"strings"
	"testing"
)

func TestDeliveryMode(t *testing.T) {
	cases := map[string]Mode{
		"":        ModeAuto,
		"auto":    ModeAuto,
		"AUTO":    ModeAuto,
		"link":    ModeLink,
		" Link ":  ModeLink,
		"native":  ModeNative,
		"nativex": ModeAuto, // 非法值保守取 auto
		"garbage": ModeAuto,
	}
	for in, want := range cases {
		t.Setenv(EnvDeliveryMode, in)
		if got := DeliveryMode(); got != want {
			t.Errorf("DeliveryMode(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestModeCapabilities(t *testing.T) {
	if !ModeAuto.UsesLink() || !ModeAuto.NativeFirst() {
		t.Error("auto 应既用原生也保留链接兜底")
	}
	if ModeLink.NativeFirst() {
		t.Error("link 不应先走原生")
	}
	if ModeNative.UsesLink() {
		t.Error("native 不应使用链接")
	}
	if !ModeLink.UsesLink() || !ModeNative.NativeFirst() {
		t.Error("link/native 能力判定错误")
	}
}

func TestFormatLinks(t *testing.T) {
	out := FormatLinks("", []Link{
		{Name: "报告.pdf", URL: "https://cn.dl.xiusoft.cn/f/20260926/abc/报告.pdf", Size: 2048},
		{Name: "坏.bin", Err: errors.New("投递失败: 连接超时")},
	})
	if !strings.Contains(out, "📎 文件下载链接") {
		t.Errorf("缺默认标题: %s", out)
	}
	if !strings.Contains(out, "报告.pdf（2.0 KB）") {
		t.Errorf("缺文件名/体积: %s", out)
	}
	if !strings.Contains(out, "https://cn.dl.xiusoft.cn/f/20260926/abc/报告.pdf") {
		t.Errorf("缺链接: %s", out)
	}
	if !strings.Contains(out, "⚠️ 坏.bin 发送失败：投递失败: 连接超时") {
		t.Errorf("缺失败原因: %s", out)
	}

	if got := FormatLinks("x", nil); got != "" {
		t.Errorf("空列表应返回空串，实际 %q", got)
	}
	only := FormatLinks("自定义标题", []Link{{Name: "a.txt", URL: "https://x/f/1/a.txt"}})
	if !strings.HasPrefix(only, "自定义标题") {
		t.Errorf("应使用自定义标题: %s", only)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		999:     "999 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		1048576: "1.0 MB",
		3 << 30: "3.0 GB",
	}
	for in, want := range cases {
		if got := HumanSize(in); got != want {
			t.Errorf("HumanSize(%d) = %q，期望 %q", in, got, want)
		}
	}
}
