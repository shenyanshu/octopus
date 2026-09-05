import assert from "node:assert/strict";
import test from "node:test";
import {
  PATTERN_MAX_BYTES,
  patternByteLength,
  patternTooLong,
  previewQueryEnabled,
  resolvePreview,
  type PreviewQuerySnapshot,
} from "./utils.ts";

// 运行: node --experimental-strip-types src/components/modules/group/utils.test.ts (web/ 目录下)
// 覆盖 Editor 预览判定的风险行为: 旧 400 污染新输入、网络错误阻塞保存、旧列表泄漏, 以及规则字节边界。

const snapshot = (
  overrides: Partial<PreviewQuerySnapshot>,
): PreviewQuerySnapshot => ({
  pending: false,
  success: false,
  error: null,
  ...overrides,
});

test("patternByteLength 按 UTF-8 字节而非字符数计长", () => {
  assert.equal(patternByteLength("gpt-4"), 5);
  assert.equal(patternByteLength("模型"), 6);
});

test("patternTooLong 以后端字节上限为界, 多字节字符按字节判定", () => {
  assert.equal(patternTooLong("a".repeat(PATTERN_MAX_BYTES)), false);
  assert.equal(patternTooLong("a".repeat(PATTERN_MAX_BYTES + 1)), true);
  assert.equal(patternTooLong("模".repeat(PATTERN_MAX_BYTES / 3)), false);
  assert.equal(patternTooLong("模".repeat(PATTERN_MAX_BYTES / 3 + 1)), true);
});

test("previewQueryEnabled 仅在防抖后规则非空且未超长时发起预览", () => {
  assert.equal(previewQueryEnabled("gpt", ""), false);
  assert.equal(previewQueryEnabled("gpt", "gpt"), true);
  const tooLong = "a".repeat(PATTERN_MAX_BYTES + 1);
  assert.equal(previewQueryEnabled(tooLong, tooLong), false);
});

test("防抖窗口内旧 400 不得禁用新规则", () => {
  // 旧 pattern "(bad" 被判 400, 用户已输入新 pattern 但防抖未到期、新请求未发出。
  const decision = resolvePreview(
    "gpt-.*",
    "(bad",
    snapshot({ error: "rejected" }),
  );
  assert.equal(decision.settling, true);
  assert.equal(decision.invalid, false);
  assert.equal(decision.status, "matching");
  assert.equal(decision.showCandidates, false);
});

test("防抖窗口内旧成功结果不得展示给新输入", () => {
  const decision = resolvePreview("new", "old", snapshot({ success: true }));
  assert.equal(decision.settling, true);
  assert.equal(decision.showCandidates, false);
});

test("当前 pattern 的 400 判定为规则无效并拦截保存", () => {
  const decision = resolvePreview(
    "(bad",
    "(bad",
    snapshot({ error: "rejected" }),
  );
  assert.equal(decision.settling, false);
  assert.equal(decision.invalid, true);
  assert.equal(decision.status, "invalid");
  assert.equal(decision.showCandidates, false);
});

test("新请求未返回时不展示旧列表", () => {
  const decision = resolvePreview("gpt", "gpt", snapshot({ pending: true }));
  assert.equal(decision.settling, true);
  assert.equal(decision.status, "matching");
  assert.equal(decision.showCandidates, false);
});

test("网络错误允许保存但不显示任何列表", () => {
  const decision = resolvePreview("gpt", "gpt", snapshot({ error: "failed" }));
  assert.equal(decision.invalid, false);
  assert.equal(decision.failed, true);
  assert.equal(decision.status, "failed");
  assert.equal(decision.showCandidates, false);
});

test("预览成功时展示当前候选", () => {
  const decision = resolvePreview("gpt", "gpt", snapshot({ success: true }));
  assert.equal(decision.invalid, false);
  assert.equal(decision.status, "ready");
  assert.equal(decision.showCandidates, true);
});

test("超长规则直接判无效, 不依赖查询状态", () => {
  const tooLong = "a".repeat(PATTERN_MAX_BYTES + 1);
  const decision = resolvePreview(
    tooLong,
    tooLong,
    snapshot({ success: true }),
  );
  assert.equal(decision.tooLong, true);
  assert.equal(decision.invalid, true);
  assert.equal(decision.showCandidates, false);
});

test("查询禁用(空规则)时 pending 不得误判为匹配中", () => {
  // React Query v5 中 disabled 查询的 isPending 恒为 true, 只能靠 enabled 短路。
  const decision = resolvePreview("", "", snapshot({ pending: true }));
  assert.equal(decision.settling, false);
  assert.equal(decision.invalid, false);
});
