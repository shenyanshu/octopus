import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { apiRequest } from "./client";
import { modelListQueryOptions } from "./queries";
import type { LLMPrice, PriceField } from "./model-price";

export type { LLMPrice, PriceField };

/**
 * 价格来源。manual 为用户显式配置(含手动 0 = 免费), auto 为参考价;
 * price 可能为 null(未知), 绝不能把"未知"显示成 0/免费。
 */
export type LLMPriceSource = "manual" | "auto";

/**
 * model/list 冻结契约: 嵌套 price|null + 来源 + 是否已知, 完全替代旧 flat 响应。
 */
export interface LLMInfo {
  name: string;
  source: LLMPriceSource;
  price_known: boolean;
  price: LLMPrice | null;
}

/**
 * create/update 仍是 flat 四价负载, 不含 source/known。
 */
export type LLMPricePayload = Record<PriceField, number> & { name: string };

export function useModelList() {
  return useQuery({
    ...modelListQueryOptions,
    refetchInterval: 30000,
    refetchOnMount: "always",
  });
}

/**
 * 更新模型价格。保存成功后端强制 source=manual, 失效列表立即反映。
 */
export function useUpdateModel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: LLMPricePayload) =>
      apiRequest<LLMPricePayload>("/api/v1/model/update", {
        method: "POST",
        body: data,
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      }),
  });
}

/**
 * 创建模型价格记录。后端强制 source=manual。
 */
export function useCreateModel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (data: LLMPricePayload) =>
      apiRequest<LLMPricePayload>("/api/v1/model/create", {
        method: "POST",
        body: data,
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      }),
  });
}

/**
 * 删除未被渠道引用的模型价格。
 */
export function useDeleteModel() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (name: string) =>
      apiRequest<null>("/api/v1/model/delete", {
        method: "POST",
        body: { name },
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      }),
  });
}

/**
 * 拉取最新参考价目录, auto 模型价格随后端参考表更新; manual 不动。
 */
export function useUpdateModelPrice() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () =>
      apiRequest<null>("/api/v1/model/update-price", {
        method: "POST",
        body: {},
      }),
    onSuccess: () => {
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      });
      queryClient.invalidateQueries({
        queryKey: ["models", "last-update-time"],
      });
    },
  });
}

/**
 * 补齐缺失的价格记录并重新解析参考价, 不覆盖 manual, 历史费用不重算。
 */
export function useRebuildModelPrice() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () =>
      apiRequest<{ count: number }>("/api/v1/model/rebuild-price", {
        method: "POST",
        body: {},
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      }),
  });
}

/**
 * 恢复自动定价: 放弃手动价回到参考价(或 unknown)。
 * 成功后端返回解析后的列表项; 失效列表刷新 source/price。
 */
export function useRestoreAutoModelPrice() {
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (name: string) =>
      apiRequest<LLMInfo>("/api/v1/model/restore-auto", {
        method: "POST",
        body: { name },
      }),
    onSuccess: () =>
      queryClient.invalidateQueries({
        queryKey: modelListQueryOptions.queryKey,
      }),
  });
}

/**
 * 获取 LLM 模型价格最后更新时间。
 */
export function useLastUpdateTime() {
  return useQuery({
    queryKey: ["models", "last-update-time"],
    queryFn: () => apiRequest<string>("/api/v1/model/last-update-time"),
    refetchInterval: 30000,
  });
}
