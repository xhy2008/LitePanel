package service

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestLogBufKeepsLastN(t *testing.T) {
	b := NewLogBuf(500)
	for i := 1; i <= 600; i++ {
		b.Write([]byte(fmt.Sprintf("line %d\n", i)))
	}
	lines := b.Tail(1000)
	if len(lines) != 500 {
		t.Fatalf("应只留最后 500 行, got %d", len(lines))
	}
	// 顺序必须是时间序，且丢掉的是最老的 100 行。
	if lines[0] != "line 101" || lines[499] != "line 600" {
		t.Fatalf("边界不对: first=%q last=%q", lines[0], lines[499])
	}
}

func TestLogBufSplitsOnLines(t *testing.T) {
	b := NewLogBuf(10)
	// 三次写入拼出四行：切分必须按行界，不能按写入次数或字节数。
	b.Write([]byte("a1\nb1"))
	b.Write([]byte("c\n"))
	b.Write([]byte("d\n"))
	got := b.Tail(10)
	want := []string{"a1", "b1c", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("按行切分不对: %v", got)
	}
}

// 单行超长要截断，否则一个二进制输出就能把预算吃光。
func TestLogBufTruncatesLongLine(t *testing.T) {
	b := NewLogBuf(10)
	b.Write([]byte(strings.Repeat("x", maxLogLineBytes+1000) + "\n短行\n"))
	lines := b.Tail(10)
	if len(lines) != 2 {
		t.Fatalf("期望 2 行, got %d", len(lines))
	}
	if len(lines[0]) != maxLogLineBytes {
		t.Fatalf("超长行未截断, len=%d want %d", len(lines[0]), maxLogLineBytes)
	}
	if lines[1] != "短行" {
		t.Fatalf("截断把后续行也带坏了: %q", lines[1])
	}
}

// 没有换行符的尾巴不能丢 —— 服务刚 printf 没换行时也得看得见。
func TestLogBufExposesPartialTail(t *testing.T) {
	b := NewLogBuf(10)
	b.Write([]byte("正在启动…"))
	if got := b.Tail(10); len(got) != 1 || got[0] != "正在启动…" {
		t.Fatalf("未换行的尾巴应可见, got %v", got)
	}
	b.Write([]byte("完成\n"))
	if got := b.Tail(10); len(got) != 1 || got[0] != "正在启动…完成" {
		t.Fatalf("补上换行后应合成一行, got %v", got)
	}
}

func TestLogBufClear(t *testing.T) {
	b := NewLogBuf(10)
	b.Write([]byte("x\ny\n"))
	b.Clear()
	if got := b.Tail(10); len(got) != 0 {
		t.Fatalf("清空后仍有内容: %v", got)
	}
	if b.Len() != 0 {
		t.Fatalf("Len 未归零: %d", b.Len())
	}
}

func TestLogBufLimitChangeable(t *testing.T) {
	b := NewLogBuf(500)
	for i := 0; i < 100; i++ {
		b.Write([]byte("l\n"))
	}
	b.Resize(10)
	if got := b.Tail(1000); len(got) != 10 {
		t.Fatalf("改上限后应立刻生效, got %d", len(got))
	}
}

// 写入方是 io.Copy（服务输出），读取方是 API/WS，两边并发。
func TestLogBufConcurrent(t *testing.T) {
	b := NewLogBuf(200)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				b.Write([]byte(fmt.Sprintf("w%d-%d\n", w, i)))
			}
		}(w)
	}
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				_ = b.Tail(50)
			}
		}()
	}
	wg.Wait()
	if n := b.Len(); n > 200 {
		t.Fatalf("并发下超出上限: %d", n)
	}
}

// OnAppend 供 WS svclog 频道实时转发。
func TestLogBufOnAppendReceivesLines(t *testing.T) {
	b := NewLogBuf(10)
	var got []string
	b.OnAppend(func(lines []string) { got = append(got, lines...) })
	b.Write([]byte("一\n二\n"))
	if strings.Join(got, "|") != "一|二" {
		t.Fatalf("回调内容不对: %v", got)
	}
	// 清空后不该再往外吐旧行。
	b.Clear()
	if len(b.Tail(10)) != 0 {
		t.Fatal("Clear 没生效")
	}
}
