package service

import (
	"testing"
	"time"
)

// 停止宽限期必须是可读写的默认值（设置页热改的就是它）。
//
// 这条测试钉的是"接线"而不是字段：handler 现在传 grace=0 表示"用面板
// 当前设置"，如果 Supervisor 里没有这个默认值，0 会被 terminate 当成
// "你自己决定"回落到常量，设置页上的数字就成了只存不读的摆设。
func TestSupervisorGraceDefault(t *testing.T) {
	s := NewSupervisor(nil)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("默认应是 %v，得 %v", DefaultGrace, got)
	}
	s.SetGrace(30 * time.Second)
	if got := s.Grace(); got != 30*time.Second {
		t.Errorf("改后应是 30s，得 %v", got)
	}
	// 非正值按"没设置"处理：宽限 0 秒会让 SIGTERM 立刻升级成 SIGKILL，
	// 把还在写盘的服务打断 —— 一个坏数字不该有这么大的破坏力。
	s.SetGrace(0)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("0 应回落到默认，得 %v", got)
	}
	s.SetGrace(-time.Second)
	if got := s.Grace(); got != DefaultGrace {
		t.Errorf("负值应回落到默认，得 %v", got)
	}
}
