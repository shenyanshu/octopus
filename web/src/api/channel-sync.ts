import type {
  ChannelModelSyncStatus,
  ChannelSyncStartResult,
} from "@/api/channel";

// 同步状态进入以下集合即代表一次同步结束; idle(未同步)与 running 都不是结束。
const TERMINAL_SYNC_STATUSES: ReadonlySet<ChannelModelSyncStatus["status"]> =
  new Set(["success", "partial", "failed", "skipped"]);

// hasRunningSync 判断是否存在正在进行的同步, 用于提高轮询频率与禁用触发按钮避免连点。
export function hasRunningSync(
  statuses: ChannelModelSyncStatus[] | undefined,
): boolean {
  return statuses?.some((s) => s.status === "running") ?? false;
}

// SYNC_STATUS_RUNNING_POLL_MS / SYNC_STATUS_IDLE_POLL_MS 是状态查询的两档轮询间隔:
// 存在运行中的同步时高频, 全部空闲时低频; 只由 useChannelSyncStatus 消费, 集中在此维护。
export const SYNC_STATUS_RUNNING_POLL_MS = 2000;
export const SYNC_STATUS_IDLE_POLL_MS = 30000;

// detectCompletedSyncs 找出自上一轮以来新完成一次同步的渠道, 这些渠道的模型与授权可能已增加,
// 相关查询需要失效刷新。同步由后台任务推进且没有完成事件推送, 只能靠相邻两轮状态识别。
// previous 为 undefined 时是首轮基线, 一律不报: 缓存里已有的终态不算新完成。
// 识别规则:
// 1. running -> 终态直接算完成, 同毫秒结束导致时间相同也算;
// 2. 其余路径只信终态记录携带的 last_sync_at, 只做 opaque 字符串比较, 不解析日期:
//    上一轮无记录(含 previous=[])或 idle 时, 新出现的带有效时间的终态即新完成;
//    上一轮已是终态时, 时间值变了才算又完成一次(覆盖短任务整个发生在两轮之间,
//    以及受理后立即 invalidate 时状态已落终态的场景)。
export function detectCompletedSyncs(
  previous: ChannelModelSyncStatus[] | undefined,
  next: ChannelModelSyncStatus[] | undefined,
): number[] {
  if (previous === undefined || !next) return [];
  const previousByChannel = new Map(previous.map((s) => [s.channel_id, s]));
  return next
    .filter((s) => {
      if (!TERMINAL_SYNC_STATUSES.has(s.status)) return false;
      const prev = previousByChannel.get(s.channel_id);
      if (prev?.status === "running") return true;
      // 非 running 落终态的路径必须有有效时间, 否则无法判定"新完成"。
      if (typeof s.last_sync_at !== "string" || s.last_sync_at === "")
        return false;
      if (!prev || prev.status === "idle") return true;
      return s.last_sync_at !== prev.last_sync_at;
    })
    .map((s) => s.channel_id);
}

// classifySingleSyncResult 判定单个渠道同步请求的受理结果。
// 回执的三个集合互斥; 渠道不在任一集合属于意外, 按 skipped 处理, 不谎报已启动。
export function classifySingleSyncResult(
  result: ChannelSyncStartResult,
  channelId: number,
): "started" | "busy" | "skipped" {
  if (result.started_ids.includes(channelId)) return "started";
  if (result.busy_ids.includes(channelId)) return "busy";
  return "skipped";
}

// classifyBatchSyncResult 归纳批量同步的受理结果以决定提示:
// 有启动报启动数; 零启动但有人在跑说明正在进行; 其余(全部被跳过或没有符合条件的渠道)按无可同步处理。
export function classifyBatchSyncResult(
  result: ChannelSyncStartResult,
):
  | { kind: "started"; count: number }
  | { kind: "busy"; count: number }
  | { kind: "none" } {
  if (result.started_ids.length > 0)
    return { kind: "started", count: result.started_ids.length };
  if (result.busy_ids.length > 0)
    return { kind: "busy", count: result.busy_ids.length };
  return { kind: "none" };
}

// 同步周期的合法范围, 单位小时, 与后端 model_sync_interval 设置一致; 范围不含 0, 没有全局关闭档。
export const MODEL_SYNC_INTERVAL_MIN_HOURS = 1;
export const MODEL_SYNC_INTERVAL_MAX_HOURS = 168;

// parseSyncIntervalHours 解析同步周期输入: 仅接受 1..168 的整数小时, 其他一律返回 null 表示非法。
export function parseSyncIntervalHours(raw: string): number | null {
  const trimmed = raw.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const hours = Number(trimmed);
  if (
    hours < MODEL_SYNC_INTERVAL_MIN_HOURS ||
    hours > MODEL_SYNC_INTERVAL_MAX_HOURS
  )
    return null;
  return hours;
}

// classifyEnableAllResult 区分批量开启自动同步的回执: 有实际翻转报数量, 全部为 0 说明没有需要开启的渠道。
export function classifyEnableAllResult(
  updatedCount: number,
): { kind: "updated"; count: number } | { kind: "none" } {
  return updatedCount > 0
    ? { kind: "updated", count: updatedCount }
    : { kind: "none" };
}

// formatSyncChanges 生成一次同步完成的增删摘要分段; 四项都为 0 时返回空数组, 由界面决定不显示计数行,
// 避免 no-op 也渲染出 "新增 0 移除 0" 的噪音。分隔符由调用方按语言决定。
export function formatSyncChanges(
  s: Pick<
    ChannelModelSyncStatus,
    "added_models" | "added_grants" | "removed_models" | "removed_grants"
  >,
  added: (models: number, grants: number) => string,
  removed: (models: number, grants: number) => string,
): string[] {
  const parts: string[] = [];
  if (s.added_models > 0 || s.added_grants > 0)
    parts.push(added(s.added_models, s.added_grants));
  if (s.removed_models > 0 || s.removed_grants > 0)
    parts.push(removed(s.removed_models, s.removed_grants));
  return parts;
}

// formatSyncTime 把 RFC3339 UTC 时间渲染为本地短格式; 缺失或非法时返回 null, 由界面决定不显示时间。
export function formatSyncTime(
  iso: string | null,
  locale: string,
): string | null {
  if (!iso) return null;
  const time = new Date(iso).getTime();
  if (Number.isNaN(time)) return null;
  return new Intl.DateTimeFormat(locale, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(time);
}
