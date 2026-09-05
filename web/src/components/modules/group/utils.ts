export function normalizeKey(value: string) {
  return value.trim().toLowerCase();
}

export function memberKey(member: { channel_grant_id: number }) {
  return String(member.channel_grant_id);
}

// PATTERN_MAX_BYTES 是自动匹配规则的最大 UTF-8 字节数, 与后端预览接口的限制一致。
export const PATTERN_MAX_BYTES = 1024;

// patternByteLength 按 UTF-8 字节计长: 后端按字节限制, 不能用字符数代替。
export function patternByteLength(pattern: string) {
  return new TextEncoder().encode(pattern).length;
}

// patternTooLong 判断规则是否超出后端字节上限: 超长不发送预览请求, 直接按无效处理。
export function patternTooLong(pattern: string) {
  return patternByteLength(pattern) > PATTERN_MAX_BYTES;
}

// previewQueryEnabled 决定是否为防抖后的规则发起预览: 规则非空且未超字节上限才请求。
export function previewQueryEnabled(pattern: string, debouncedPattern: string) {
  return debouncedPattern.length > 0 && !patternTooLong(pattern);
}

// PreviewQuerySnapshot 是预览查询中与判定相关的最小状态快照, 与 React Query 解耦以便纯函数测试。
// error 只分两类: rejected = 服务端 400(规则非法, 拦截保存); failed = 网络等其他错误(不阻塞保存, 保存时服务端再校验)。
export type PreviewQuerySnapshot = {
  pending: boolean;
  success: boolean;
  error: "rejected" | "failed" | null;
};

export type PreviewDecision = {
  tooLong: boolean;
  settling: boolean;
  invalid: boolean;
  failed: boolean;
  status: "matching" | "invalid" | "failed" | "ready";
  showCandidates: boolean;
};

// resolvePreview 汇总规则输入与预览查询状态, 给出编辑器展示与提交拦截的判定。
// 关键不变量: 旧规则的结论(含 400)绝不落到新输入上 —— 防抖窗口内与新请求未返回时都算 settling, 不展示任何旧结果。
export function resolvePreview(
  pattern: string,
  debouncedPattern: string,
  query: PreviewQuerySnapshot,
): PreviewDecision {
  const tooLong = patternTooLong(pattern);
  const enabled = previewQueryEnabled(pattern, debouncedPattern);
  const settling = pattern !== debouncedPattern || (enabled && query.pending);
  const invalid = tooLong || (!settling && query.error === "rejected");
  const failed = !settling && !invalid && query.error === "failed";
  const showCandidates = !settling && !invalid && query.success;
  const status: PreviewDecision["status"] = settling
    ? "matching"
    : invalid
      ? "invalid"
      : failed
        ? "failed"
        : "ready";
  return { tooLong, settling, invalid, failed, status, showCandidates };
}
