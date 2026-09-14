package main

import (
	"strings"
	"testing"

	"litepanel/internal/config"
)

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"100.64.0.5\n":               "100.64.0.5",
		"  100.64.0.5  \n100.64.0.6": "100.64.0.5",
		"":                           "",
		"single":                     "single",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// 覆盖 -listen 优先于配置；配置为 tailscale 但探测不到时回落 127.0.0.1。
func TestResolveListen(t *testing.T) {
	if got := resolveListen(config.Config{Listen: "10.0.0.1", Port: 9530}, "127.0.0.1:1234"); got != "127.0.0.1:1234" {
		t.Errorf("override 应生效, got %q", got)
	}
	if got := resolveListen(config.Config{Listen: "100.64.0.7", Port: 9530}, ""); got != "100.64.0.7:9530" {
		t.Errorf("显式地址应直接使用, got %q", got)
	}

	old := detectIP
	detectIP = func() string { return "" }
	defer func() { detectIP = old }()

	got := resolveListen(config.Config{Listen: config.ListenAuto, Port: 9530}, "")
	if !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("探测不到 tailscale 时应回落 127.0.0.1, got %q", got)
	}

	detectIP = func() string { return "100.64.1.2" }
	if got := resolveListen(config.Config{Listen: config.ListenAuto, Port: 9530}, ""); got != "100.64.1.2:9530" {
		t.Errorf("应使用探测到的 tailscale 地址, got %q", got)
	}
}
