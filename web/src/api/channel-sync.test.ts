import assert from "node:assert/strict";
import test from "node:test";
import type { ChannelDetail } from "./channel.ts";
import {
  emptyFormState,
  fromChannel,
  toChannelConfig,
  toChannelDetail,
} from "../components/modules/channel/state.ts";
import {
  classifyBatchSyncResult,
  classifySingleSyncResult,
  detectCompletedSyncs,
  formatSyncTime,
  hasRunningSync,
  MODEL_SYNC_INTERVAL_MAX_HOURS,
  MODEL_SYNC_INTERVAL_MIN_HOURS,
  parseSyncIntervalHours,
} from "./channel-sync.ts";

// 运行: node --experimental-strip-types src/api/channel-sync.test.ts (web/ 目录下)
// 覆盖模型同步的风险判定: 启动回执不误报完成、running 去重、完成识别(时间变化)、周期输入边界与时间缺失。

const status = (
  channel_id: number,
  s: "idle" | "running" | "success" | "partial" | "failed" | "skipped",
  last_sync_at: string | null = null,
) => ({
  channel_id,
  status: s,
  last_sync_at,
  added_models: 0,
  added_grants: 0,
  error: "",
});

const T1 = "2026-09-05T08:00:00Z";
const T2 = "2026-09-05T09:00:00Z";

test("hasRunningSync 仅在有 running 状态时为真", () => {
  assert.equal(hasRunningSync(undefined), false);
  assert.equal(hasRunningSync([]), false);
  assert.equal(hasRunningSync([status(1, "success")]), false);
  assert.equal(
    hasRunningSync([status(1, "success"), status(2, "running")]),
    true,
  );
});

test("detectCompletedSyncs: running 落终态算完成, 同毫秒时间相同也算", () => {
  const before = [
    status(1, "running"),
    status(2, "running", T1),
    status(3, "idle"),
  ];
  const after = [
    status(1, "success", T1),
    status(2, "running", T1),
    status(3, "failed", T1),
  ];
  // 1 号 running->success 完成; 2 号仍在跑不算; 3 号 idle->failed 时间从无到有, 也算完成。
  assert.deepEqual(detectCompletedSyncs(before, after), [1, 3]);
});

test("detectCompletedSyncs: 首轮 undefined 只建基线, 缓存已有终态不误报", () => {
  assert.deepEqual(
    detectCompletedSyncs(undefined, [status(1, "success", T1)]),
    [],
  );
  assert.deepEqual(
    detectCompletedSyncs([status(1, "success", T1)], undefined),
    [],
  );
});

test("detectCompletedSyncs: previous=[] 基线后新出现的带时间终态是新完成", () => {
  assert.deepEqual(detectCompletedSyncs([], [status(1, "success", T1)]), [1]);
  // 新出现的终态但没有有效时间, 不能判定新完成。
  assert.deepEqual(detectCompletedSyncs([], [status(1, "success")]), []);
});

test("detectCompletedSyncs: idle 落终态(时间从无到有)算完成", () => {
  const before = [status(1, "idle")];
  assert.deepEqual(
    detectCompletedSyncs(before, [status(1, "success", T1)]),
    [1],
  );
});

test("detectCompletedSyncs: success 时间变化算又完成一次, 覆盖两轮之间的短任务", () => {
  const before = [status(1, "success", T1)];
  assert.deepEqual(
    detectCompletedSyncs(before, [status(1, "success", T2)]),
    [1],
  );
});

test("detectCompletedSyncs: partial 等其余终态的时间变化同样算完成", () => {
  const before = [status(1, "partial", T1)];
  assert.deepEqual(
    detectCompletedSyncs(before, [status(1, "partial", T2)]),
    [1],
  );
  // 终态类型变了但时间没变, 不算新完成(同一轮结果的状态迁移)。
  assert.deepEqual(detectCompletedSyncs(before, [status(1, "failed", T1)]), []);
});

test("detectCompletedSyncs: 状态与时间都不变时不误报", () => {
  const snapshot = [status(1, "success", T1), status(2, "idle")];
  assert.deepEqual(detectCompletedSyncs(snapshot, [...snapshot]), []);
  assert.deepEqual(
    detectCompletedSyncs(snapshot, [
      status(1, "success", T1),
      status(2, "running"),
    ]),
    [],
  );
});

test("detectCompletedSyncs: 手动短任务在第一次状态 refetch 前完成也能识别", () => {
  // 基线是手动触发前的 idle, 受理后立刻 refetch 时任务已结束: idle -> success 带时间。
  const baseline = [status(1, "idle")];
  const afterFastRefetch = [status(1, "success", T1)];
  assert.deepEqual(detectCompletedSyncs(baseline, afterFastRefetch), [1]);
  // 基线 undefined(页面刚打开)时不报, 但下一轮以该快照为基线后时间再变就会报。
  assert.deepEqual(detectCompletedSyncs(undefined, afterFastRefetch), []);
  assert.deepEqual(
    detectCompletedSyncs(afterFastRefetch, [status(1, "success", T2)]),
    [1],
  );
});

test("classifySingleSyncResult 按回执集合归类, 不在任一集合按 skipped 处理", () => {
  const result = { started_ids: [1], busy_ids: [2], skipped_ids: [3] };
  assert.equal(classifySingleSyncResult(result, 1), "started");
  assert.equal(classifySingleSyncResult(result, 2), "busy");
  assert.equal(classifySingleSyncResult(result, 3), "skipped");
  assert.equal(classifySingleSyncResult(result, 99), "skipped");
});

test("classifyBatchSyncResult 有启动优先报启动, 零启动区分进行中与无可同步", () => {
  assert.deepEqual(
    classifyBatchSyncResult({
      started_ids: [1, 2],
      busy_ids: [3],
      skipped_ids: [4],
    }),
    { kind: "started", count: 2 },
  );
  assert.deepEqual(
    classifyBatchSyncResult({
      started_ids: [],
      busy_ids: [3],
      skipped_ids: [],
    }),
    { kind: "busy", count: 1 },
  );
  assert.deepEqual(
    classifyBatchSyncResult({
      started_ids: [],
      busy_ids: [],
      skipped_ids: [4],
    }),
    { kind: "none" },
  );
  assert.deepEqual(
    classifyBatchSyncResult({ started_ids: [], busy_ids: [], skipped_ids: [] }),
    { kind: "none" },
  );
});

test("parseSyncIntervalHours 仅接受 1..168 的整数小时", () => {
  assert.equal(
    parseSyncIntervalHours(String(MODEL_SYNC_INTERVAL_MIN_HOURS)),
    MODEL_SYNC_INTERVAL_MIN_HOURS,
  );
  assert.equal(
    parseSyncIntervalHours(String(MODEL_SYNC_INTERVAL_MAX_HOURS)),
    MODEL_SYNC_INTERVAL_MAX_HOURS,
  );
  assert.equal(parseSyncIntervalHours(" 6 "), 6);
  assert.equal(parseSyncIntervalHours("0"), null); // 0 不是关闭档, 非法。
  assert.equal(parseSyncIntervalHours("169"), null);
  assert.equal(parseSyncIntervalHours("-3"), null);
  assert.equal(parseSyncIntervalHours("2.5"), null);
  assert.equal(parseSyncIntervalHours(""), null);
  assert.equal(parseSyncIntervalHours("abc"), null);
});

test("auto_sync_models 在初始/编辑还原/提交载荷全链路保留", () => {
  // 新建默认关闭。
  assert.equal(emptyFormState.auto_sync_models, false);

  // 编辑时从渠道配置还原, 开与关都不能丢。
  const channel: ChannelDetail = {
    id: 7,
    revision: "rev-a",
    name: "c",
    dialect: "generic",
    enabled: true,
    auto_sync_models: true,
    base_url: "https://example.com",
    openai_chat_completion_path: "/v1/chat/completions",
    openai_response_path: "/v1/responses",
    anthropic_message_path: "/v1/messages",
    keys: [],
    models: [],
    grants: [],
    proxy: false,
    custom_header: [],
    param_override: "",
    channel_proxy: "",
    match_regex: "",
  };
  const editing = fromChannel(channel);
  assert.equal(editing.auto_sync_models, true);

  // 提交载荷(创建与更新共用)携带开关, 编辑态的开启不会在保存时丢失。
  const payload = toChannelDetail(editing, 7, channel.revision);
  assert.equal(payload.auto_sync_models, true);
  const created = toChannelDetail(emptyFormState, 0, "");
  assert.equal(created.auto_sync_models, false);
});

test("revision 与表单内容分离: 提交用捕获令牌, 探测载荷不含 revision", () => {
  const state = {
    ...emptyFormState,
    name: "c",
    base_url: "https://example.com",
  };
  // 提交载荷携带打开草稿时捕获的令牌。
  const payload = toChannelDetail(state, 7, "rev-a");
  assert.equal(payload.revision, "rev-a");
  // 后续 prop 轮换不影响已捕获的令牌(表单持有自己的副本)。
  assert.equal(toChannelDetail(state, 7, "rev-a").revision, "rev-a");
  // 创建传空串, 由服务端生成。
  assert.equal(toChannelDetail(state, 0, "").revision, "");
  // 探测载荷(与提交共用 toChannelConfig)不得携带 revision。
  assert.equal("revision" in toChannelConfig(state), false);
});

test("formatSyncTime 缺失或非法时间返回 null, 合法时间给出本地化文本", () => {
  assert.equal(formatSyncTime(null, "zh-CN"), null);
  assert.equal(formatSyncTime("", "zh-CN"), null);
  assert.equal(formatSyncTime("not-a-date", "zh-CN"), null);
  const formatted = formatSyncTime("2026-09-05T08:00:00Z", "zh-CN");
  assert.ok(formatted !== null && formatted.length > 0);
});
