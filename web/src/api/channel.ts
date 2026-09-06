import {
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import { apiRequest } from "./client";
import {
  detectCompletedSyncs,
  hasRunningSync,
  SYNC_STATUS_IDLE_POLL_MS,
  SYNC_STATUS_RUNNING_POLL_MS,
} from "./channel-sync";
import {
  channelStatsQueryOptions,
  groupListQueryOptions,
  modelListQueryOptions,
} from "./queries";
import {
  formatStatsMetrics,
  type StatsMetrics,
  type StatsMetricsFormatted,
} from "./stats";

// Protocol 是渠道支持的上游线协议位掩码，位值与后端 model.Protocol 一致，不可变更。
// 一条授权可同时支持多个协议，按位或组合；1 << 0 由后端保留待用。
export const Protocol = {
  OpenAIChatCompletion: 1 << 1,
  OpenAIResponse: 1 << 2,
  AnthropicMessage: 1 << 3,
} as const;

// Dialect 是上游在标准协议之上的方言，决定出站转换器的厂商特化配置。
// 地址与路径不属于方言范畴，由前端按服务商预填到渠道字段上。
export type Dialect = "generic";

type CustomHeader = {
  header_key: string;
  header_value: string;
};

// ChannelKey 是渠道下的一份上游凭据；名称在渠道内唯一，读写都按它引用。
export type ChannelKey = {
  name: string;
  key: string;
  enabled: boolean;
};

// ChannelGrant 是渠道内的一条上游授权：指定模型使用指定凭据时支持的协议集合，也是转发的最小单位。
// 两侧按名称引用，读写同构：名称在渠道内唯一，新增的模型与凭据在后端同一事务内才分配主键，
// 故提交侧只能按名称引用，读侧也随之给名称，页面无需在主键与名称之间翻译。
// 授权本身没有启用状态: 不再授权即删掉该组合, 临时收回则停用凭据或摘掉协议位。
export type ChannelGrant = {
  model_name: string;
  key_name: string;
  protocols: number; // Protocol 位掩码。
};

// ChannelGrantCandidate 是分组页可选取的一条授权，字段与 GroupItem 的展示字段一一对应。
// 分组页由此不必拉整份渠道列表：那里带着统计、路径、代理与凭据明文，与选取成员无关。
// available 与分组成员同一口径，均由后端定稿，前后端不会各判一套。
export type ChannelGrantCandidate = {
  id: number; // 授权主键，分组成员按它引用。
  channel_id: number;
  channel_name: string;
  model_name: string;
  key_name: string;
  protocols: number; // Protocol 位掩码。
  available: boolean;
};

/**
 * 渠道完整配置（与后端 model.ChannelDetail 对齐）
 * 读写同构：编辑表单读到什么形状就提交什么形状，故创建与更新共用此类型，无需另建提交类型。
 * 提交即全量，未列出的凭据与模型会被删除并级联删除其授权；创建时 id 取 0，由后端分配。
 * keys、models、grants 和 custom_header 恒为数组，后端读取侧承诺不为 null。
 */
export type ChannelDetail = {
  id: number;
  // revision 是后端为整份配置生成的不透明版本令牌: detail/create/update 响应都会返回当前值,
  // 更新请求必须携带打开草稿时拿到的 revision; 缺失 400, 已轮换(后台同步/其他编辑/导入) 409。
  // 它只做并发保护, 不参与探测与展示。
  revision: string;
  name: string;
  dialect: Dialect;
  enabled: boolean;
  // auto_sync_models 开启后由后台按系统周期逐凭据补充模型与授权, 只增不删; 存量渠道默认关闭。
  auto_sync_models: boolean;
  base_url: string; // 上游地址，各协议共用。
  openai_chat_completion_path: string;
  openai_response_path: string;
  anthropic_message_path: string;
  keys: ChannelKey[];
  models: string[]; // 上游模型名称；模型除名称外没有界面用得上的字段。
  grants: ChannelGrant[];
  proxy: boolean;
  custom_header: CustomHeader[];
  param_override: string;
  channel_proxy: string;
  match_regex: string;
};

// ChannelModelStats 是单个渠道模型的累计统计，自带名称。
export type ChannelModelStats = StatsMetrics & {
  model_id: number;
  model_name: string;
};

// ChannelStats 是单个渠道及其模型的累计统计，自带名称与启停状态。
// 这一份同时充当渠道列表项：列表页要展示的名称、启停与模型个数（即 models.length）都在此，
// 故没有单独的渠道概览接口，整份配置在点开编辑时由 useChannelDetail 单独取。
export type ChannelStats = StatsMetrics & {
  channel_id: number;
  channel_name: string;
  enabled: boolean;
  models: ChannelModelStats[];
};

// ChannelModelStatsFormatted 是单个渠道模型的展示用统计。
export type ChannelModelStatsFormatted = {
  model_id: number;
  model_name: string;
  formatted: StatsMetricsFormatted;
};

// ChannelStatsFormatted 是单个渠道及其模型的展示用统计，同时充当渠道列表项。
// 名称与启停随统计一并给出，列表页由此只消费这一条查询：模型个数即 models.length，
// 整份配置在点开编辑时由 useChannelDetail 单独取。
export type ChannelStatsFormatted = {
  channel_id: number;
  channel_name: string;
  enabled: boolean;
  models: ChannelModelStatsFormatted[];
  formatted: StatsMetricsFormatted;
};

// FetchModelRequest 按指定凭据试拉上游模型列表。
// 渠道尚未保存时也可试拉，故随请求携带整份渠道配置：探测用的地址、协议路径、代理、Header 与过滤表达式
// 必须和保存后生效的完全一致，直接给编辑态即可，探测用不上的字段后端忽略。
// revision 只做并发保护, 与探测无关, 不属于探测载荷。
// 名称可为空：探测常发生在渠道尚未命名时，后端只要求地址非空。
// 不带协议：后端一次同时探 OpenAI 与 Anthropic 两侧，协议支持由各侧响应决定。
type FetchModelRequest = {
  channel: Omit<
    ChannelDetail,
    "id" | "keys" | "models" | "grants" | "revision"
  >;
  key: string;
};

// FetchModel 是探测到的单个上游模型及其支持的协议集合。
// OpenAI 侧记为 Responses 而非 Chat：Chat Completions 已被官方标记弃用，需要 Chat 的渠道由用户手动勾选。
export type FetchModel = {
  name: string;
  protocols: number; // Protocol 位掩码。
};

// channelGrantListQueryOptions 供分组页查询可选授权。
// 与渠道列表分开: 选取成员只需名称与可用性, 拉整份渠道会连带路径, 代理与凭据明文。
export const channelGrantListQueryOptions = queryOptions({
  queryKey: ["channels", "grants"],
  queryFn: () => apiRequest<ChannelGrantCandidate[]>("/api/v1/channel/grants"),
});

// useChannelGrantList 获取分组页可选的全部渠道授权。
export function useChannelGrantList(enabled = true) {
  return useQuery({
    ...channelGrantListQueryOptions,
    enabled,
    refetchOnMount: "always",
  });
}

// useChannelGrantPreview 按 Go 正则实时预览规则命中的授权候选, 供分组编辑器展示。
// 仅作保存前的对照: 提交仍由服务端按规则重算, 前端不提交预览结果。
// 不可用候选一并返回, 由界面标出"不会加入"。
export function useChannelGrantPreview(pattern: string, enabled: boolean) {
  return useQuery({
    queryKey: ["channels", "grants", "preview", pattern],
    queryFn: ({ signal }) =>
      apiRequest<ChannelGrantCandidate[]>("/api/v1/channel/grants/preview", {
        method: "POST",
        body: { pattern },
        signal,
      }),
    enabled,
    // 输入驱动的预览: 非法正则 400 重试无意义, 网络错误也以即时反馈为先。
    retry: false,
  });
}

// channelStatsFormattedQueryOptions 统一渠道统计查询, 格式化和刷新策略。
// 与配置分开刷新: 统计每次转发都在变, 配置只在人工改动后由 mutation 失效。
const channelStatsFormattedQueryOptions = queryOptions({
  ...channelStatsQueryOptions,
  select: (data) =>
    data.map((item): ChannelStatsFormatted => ({
      channel_id: item.channel_id,
      channel_name: item.channel_name,
      enabled: item.enabled,
      models: item.models.map((channelModel) => ({
        model_id: channelModel.model_id,
        model_name: channelModel.model_name,
        formatted: formatStatsMetrics(channelModel),
      })),
      formatted: formatStatsMetrics(item),
    })),
  refetchInterval: 30000,
  refetchOnMount: "always",
});

// useChannelStats 获取全部渠道及其模型的展示用统计, 也是渠道列表页的数据来源。
export function useChannelStats(enabled = true) {
  return useQuery({ ...channelStatsFormattedQueryOptions, enabled });
}

// channelDetailQueryOptions 统一渠道完整配置的查询定义, 供编辑表单打开时订阅与
// 冲突后强制重取(queryClient.fetchQuery)共用; 重取必须走同一 queryKey 才能拿到真实新数据。
export const channelDetailQueryOptions = (id: number) =>
  queryOptions({
    queryKey: ["channels", "detail", id],
    queryFn: () => apiRequest<ChannelDetail>(`/api/v1/channel/detail/${id}`),
    refetchOnMount: "always",
  });

/**
 * 获取单个渠道完整配置 Hook, 供编辑表单打开时读取; id 为空时不发请求。
 * 不随统计一并取回: 整份配置带着路径、代理与凭据明文, 只有正在编辑的那一个渠道用得上。
 *
 * @example
 * const { data: detail } = useChannelDetail(channelId);
 */
export function useChannelDetail(id?: number) {
  return useQuery({
    ...channelDetailQueryOptions(id ?? 0),
    enabled: id !== undefined,
  });
}

/**
 * 创建渠道 Hook；提交整份配置，id 取 0 由后端分配。
 * 授权与凭据、模型在同一请求提交：授权按名称引用两侧，后端在同一事务内解析为主键。
 *
 * @example
 * const createChannel = useCreateChannel();
 *
 * createChannel.mutate({
 *   id: 0,
 *   name: 'OpenAI',
 *   base_url: 'https://api.openai.com',
 *   keys: [{ name: 'default', key: 'sk-xxx', enabled: true }],
 *   models: ['gpt-4o'],
 *   grants: [{ model_name: 'gpt-4o', key_name: 'default', protocols: Protocol.OpenAIResponse }],
 *   // ...其余配置字段
 * });
 */
export function useCreateChannel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: ChannelDetail) =>
      apiRequest<ChannelDetail>("/api/v1/channel/create", {
        method: "POST",
        body: data,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["channels"] });
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      });
    },
  });
}

/**
 * 更新渠道 Hook；提交整份配置，整体替换。
 * keys、models 和 grants 提交即覆盖：未列出的凭据和模型会被删除并级联删除其授权。
 * grants 的 protocols 不能为 0 或含未定义位，名称也必须属于同一渠道，否则后端整单拒绝。
 *
 * @example
 * const updateChannel = useUpdateChannel();
 *
 * updateChannel.mutate({ ...detail, enabled: false });
 */
export function useUpdateChannel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: ChannelDetail) =>
      apiRequest<ChannelDetail>("/api/v1/channel/update", {
        method: "POST",
        body: data,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["channels"] });
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      });
      queryClient.invalidateQueries({
        queryKey: groupListQueryOptions.queryKey,
      });
    },
  });
}

/**
 * 删除渠道 Hook
 *
 * @example
 * const deleteChannel = useDeleteChannel();
 *
 * deleteChannel.mutate(1); // 删除 ID 为 1 的渠道
 */
export function useDeleteChannel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (id: number) =>
      apiRequest<null>(`/api/v1/channel/delete/${id}`, { method: "DELETE" }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["channels"] });
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      });
      queryClient.invalidateQueries({
        queryKey: groupListQueryOptions.queryKey,
      });
    },
  });
}

/**
 * 启用/禁用渠道 Hook
 *
 * @example
 * const enableChannel = useEnableChannel();
 *
 * enableChannel.mutate({ id: 1, enabled: true }); // 启用 ID 为 1 的渠道
 * enableChannel.mutate({ id: 1, enabled: false }); // 禁用 ID 为 1 的渠道
 */
export function useEnableChannel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: { id: number; enabled: boolean }) =>
      apiRequest<null>("/api/v1/channel/enable", {
        method: "POST",
        body: data,
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["channels"] });
      // 渠道启停会改变其授权在分组内的可用性，成员列表要跟着刷新。
      queryClient.invalidateQueries({
        queryKey: groupListQueryOptions.queryKey,
      });
    },
  });
}

/**
 * 获取渠道模型列表 Hook
 *
 * @example
 * const fetchModel = useFetchModel();
 *
 * fetchModel.mutate({
 *   channel: { base_url: 'https://api.openai.com', openai_response_path: '/v1/responses', ... },
 *   key: 'sk-xxx',
 * });
 *
 * // 在 onSuccess 中获取模型列表
 * fetchModel.data // [{ name: 'gpt-4o', protocols: 4 }, ...]
 */
export function useFetchModel() {
  return useMutation({
    mutationFn: (data: FetchModelRequest) =>
      apiRequest<FetchModel[]>("/api/v1/channel/fetch-model", {
        method: "POST",
        body: data,
      }),
  });
}

// ChannelModelSyncStatus 是单个渠道最近一次模型同步的结果快照, 由后端内存维护:
// 重启后无记录, 列表中不存在的渠道一律视为 idle。last_sync_at 为 RFC3339 UTC 时间, 未同步过为 null。
// error 已由后端保证可安全展示, 前端不做二次加工。
export type ChannelModelSyncStatus = {
  channel_id: number;
  status: "idle" | "running" | "success" | "partial" | "failed" | "skipped";
  last_sync_at: string | null;
  added_models: number;
  added_grants: number;
  // 仅完整成功且上游非空时移除本凭据已消失的自动管理项; 部分成功/失败/空结果不删, 手动与历史项始终保留。
  removed_models: number;
  removed_grants: number;
  error: string;
};

// ChannelSyncStartResult 是同步启动接口的即时回执: 200 仅表示请求被受理, 不代表同步完成,
// 完成结果由 sync-status 查询随后给出。三个集合互斥。
export type ChannelSyncStartResult = {
  started_ids: number[];
  busy_ids: number[];
  skipped_ids: number[];
};

// channelSyncStatusQueryOptions 统一模型同步状态查询定义, 供状态订阅与失效共用。
export const channelSyncStatusQueryOptions = queryOptions({
  queryKey: ["channels", "sync-status"],
  queryFn: () =>
    apiRequest<ChannelModelSyncStatus[]>("/api/v1/channel/sync-status"),
});

// useChannelSyncStatus 订阅全部渠道最近一次同步的状态。
// 仅在订阅方(渠道页)展示期间轮询: 空闲每 30s 低频刷新, 存在运行中的同步时降到 2s, 结束后自动回到低频。
export function useChannelSyncStatus(enabled = true) {
  return useQuery({
    ...channelSyncStatusQueryOptions,
    enabled,
    refetchOnMount: "always",
    refetchInterval: (query) =>
      hasRunningSync(query.state.data)
        ? SYNC_STATUS_RUNNING_POLL_MS
        : SYNC_STATUS_IDLE_POLL_MS,
  });
}

// useChannelSyncCompletionInvalidation 在同步由 running 落入终态时失效相关查询,
// 让新增的模型与授权立刻出现在各页面。
// detail 只标记过期不主动重取(refetchType: "none"): 编辑表单可能正开着, 草稿在挂载时已定稿,
// 重取既无意义也容易造成困惑; 下次打开时 refetchOnMount 会取回最新配置。
export function useChannelSyncCompletionInvalidation(
  statuses: ChannelModelSyncStatus[] | undefined,
) {
  const queryClient = useQueryClient();
  const previousStatuses = useRef<ChannelModelSyncStatus[] | undefined>(
    undefined,
  );

  useEffect(() => {
    const completedChannelIds = detectCompletedSyncs(
      previousStatuses.current,
      statuses,
    );
    previousStatuses.current = statuses;
    if (completedChannelIds.length === 0) return;
    for (const id of completedChannelIds) {
      queryClient.invalidateQueries({
        queryKey: ["channels", "detail", id],
        refetchType: "none",
      });
    }
    queryClient.invalidateQueries({
      queryKey: channelStatsQueryOptions.queryKey,
    });
    queryClient.invalidateQueries({
      queryKey: channelGrantListQueryOptions.queryKey,
    });
    queryClient.invalidateQueries({ queryKey: groupListQueryOptions.queryKey });
    // 同步只增不删地补充了模型, 全局模型列表(模型页与表单可选集)也要跟着出现新项。
    queryClient.invalidateQueries({ queryKey: modelListQueryOptions.queryKey });
  }, [statuses, queryClient]);
}

/**
 * 请求后台同步单个渠道的模型 Hook；200 只代表受理，完成结果看 sync-status。
 * 后端要求渠道已启用且存在启用的凭据，否则跳过；渠道不存在返回 404。
 *
 * @example
 * const syncModels = useSyncChannelModels();
 * syncModels.mutate(1);
 */
export function useSyncChannelModels() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (channelId: number) =>
      apiRequest<ChannelSyncStartResult>(
        `/api/v1/channel/sync-models/${channelId}`,
        {
          method: "POST",
          body: {},
        },
      ),
    // 受理后立即刷新状态查询, 让 running 尽快出现在状态条上。
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: channelSyncStatusQueryOptions.queryKey,
      }),
  });
}

/**
 * 请求后台同步所有"已启用且开启自动同步"的渠道 Hook；200 只代表受理，完成结果看 sync-status。
 * 正在同步的渠道由后端去重(busy_ids)，不符合条件的渠道跳过(skipped_ids)。
 *
 * @example
 * const syncAll = useSyncAllChannelModels();
 * syncAll.mutate();
 */
export function useSyncAllChannelModels() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () =>
      apiRequest<ChannelSyncStartResult>("/api/v1/channel/sync-models-all", {
        method: "POST",
        body: {},
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: channelSyncStatusQueryOptions.queryKey,
      }),
  });
}

// ChannelEnableAllAutoSyncResult 是批量开启自动同步的回执; 后端只在 true->false 实际翻转时计数,
// 已全部开启(含空库)时 count 为 0。
export type ChannelEnableAllAutoSyncResult = {
  updated_count: number;
};

/**
 * 为所有渠道开启模型自动同步 Hook；只翻转 auto_sync_models, 绝不改启用状态也不触发同步。
 * 已开启的渠道不受影响, 返回值计数为实际变更数(0 即全部已开启)。
 *
 * 后端为此操作轮换受影响渠道的 revision, 打开中的编辑草稿会因令牌失配在保存时收到 409,
 * 由草稿内既有冲突流程处理; 这里不重挂载表单, 只失效查询让下一次打开取到新值。
 *
 * @example
 * const enableAll = useEnableAllChannelAutoSync();
 * enableAll.mutate(undefined, { onSuccess: ({ updated_count }) => ... });
 */
export function useEnableAllChannelAutoSync() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () =>
      apiRequest<ChannelEnableAllAutoSyncResult>(
        "/api/v1/channel/auto-sync/enable-all",
        { method: "POST", body: {} },
      ),
    // ["channels"] 前缀失效覆盖 detail/stats/sync-status/grants, 跟随项目既有惯例;
    // 不重挂载打开的编辑表单, 草稿与捕获的 revision 原样保留。
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["channels"] }),
  });
}
