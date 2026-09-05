package model

import (
	"errors"
	"strings"
	"testing"
)

// CompileGroupPattern 是规则与预览共用的单一判定入口, 本文件覆盖其语义边界。
// 不涉及数据库与缓存: 纯函数行为在此一次性定稿, 上层测试聚焦事务与补齐时无需重复。

func TestCompileGroupPatternEmptyReturnsNil(t *testing.T) {
	re, err := CompileGroupPattern("")
	if err != nil {
		t.Fatalf("空串不应报错: %v", err)
	}
	if re != nil {
		t.Fatalf("空串应返回 nil 正则表示关闭规则")
	}
}

func TestCompileGroupPatternValid(t *testing.T) {
	re, err := CompileGroupPattern("gpt-4o")
	if err != nil {
		t.Fatalf("合法正则不应报错: %v", err)
	}
	if !re.MatchString("gpt-4o-mini") {
		t.Fatalf("标准 regexp 子串匹配失效")
	}
}

func TestCompileGroupPatternCaseSensitiveDefault(t *testing.T) {
	re, err := CompileGroupPattern("GPT")
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if re.MatchString("gpt") {
		t.Fatalf("默认应大小写敏感, 大写不应匹配小写")
	}
}

func TestCompileGroupPatternCaseInsensitiveFlag(t *testing.T) {
	re, err := CompileGroupPattern("(?i)GPT")
	if err != nil {
		t.Fatalf("(?i) 编译失败: %v", err)
	}
	if !re.MatchString("gpt") {
		t.Fatalf("(?i) 应忽略大小写")
	}
}

func TestCompileGroupPatternLookaroundRejected(t *testing.T) {
	// RE2 不支持 lookaround, 编译失败即非法。
	if _, err := CompileGroupPattern("(?<=gpt)-4o"); err == nil {
		t.Fatalf("lookaround 应被拒绝")
	}
}

func TestCompileGroupPatternInvalidRegex(t *testing.T) {
	if _, err := CompileGroupPattern("[gpt"); err == nil {
		t.Fatalf("未闭合括号应被拒绝")
	}
}

func TestCompileGroupPatternOverlongRejected(t *testing.T) {
	long := strings.Repeat("a", MaxPatternBytes+1)
	re, err := CompileGroupPattern(long)
	if err == nil {
		t.Fatalf("超过上限应报错")
	}
	if re != nil {
		t.Fatalf("超长不应返回正则")
	}
	if !errors.Is(err, ErrPatternTooLong) {
		t.Fatalf("超长错误应为 ErrPatternTooLong: %v", err)
	}
}

func TestCompileGroupPatternMaxLengthBoundary(t *testing.T) {
	// 恰好 MaxPatternBytes 字节: 合法正则不因长度被拒。
	re, err := CompileGroupPattern(strings.Repeat("a", MaxPatternBytes))
	if err != nil {
		t.Fatalf("恰好上限不应报错: %v", err)
	}
	if re == nil {
		t.Fatalf("非空合法 pattern 应返回正则")
	}
}

func TestCompileGroupPatternInvalidIsSentinel(t *testing.T) {
	// 错误契约稳定: 非法正则返回 ErrPatternInvalid, 文案 "invalid regex pattern"。
	if _, err := CompileGroupPattern("[gpt"); !errors.Is(err, ErrPatternInvalid) {
		t.Fatalf("非法正则错误应为 ErrPatternInvalid: %v", err)
	}
	if err := ErrPatternInvalid; err.Error() != "invalid regex pattern" {
		t.Fatalf("非法正则文案变化: %q", err.Error())
	}
	if err := ErrPatternTooLong; err.Error() != "pattern exceeds maximum length" {
		t.Fatalf("超长文案变化: %q", err.Error())
	}
}
