import {
  useCallback,
  useMemo,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import {
  Check,
  ChevronDownIcon,
  HelpCircle,
  Plus,
  Search,
  Trash2,
} from "lucide-react";
import { useTranslations } from "use-intl";
import { useDebounce } from "@uidotdev/usehooks";
import { toast } from "sonner";
import * as AccordionPrimitive from "@radix-ui/react-accordion";
import {
  Protocol,
  useChannelGrantList,
  useChannelGrantPreview,
  type ChannelGrantCandidate,
} from "@/api/channel";
import { ApiError } from "@/api/client";
import { Button } from "@/components/ui/button";
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Accordion,
  AccordionContent,
  AccordionItem,
} from "@/components/ui/accordion";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { getModelIcon } from "@/lib/model-icons";
import type { GroupMode, GroupRelayConfig } from "@/api/group";
import type { SelectedMember } from "./ItemList";
import { MemberList } from "./ItemList";
import {
  memberKey,
  normalizeKey,
  previewQueryEnabled,
  resolvePreview,
} from "./utils";

export type GroupEditorValues = {
  name: string;
  mode: GroupMode;
  relay_config: GroupRelayConfig;
  auto_add_pattern: string;
  members: SelectedMember[];
};

// defaultRelayConfig 提供创建分组时的前端初始配置。
const defaultRelayConfig: GroupRelayConfig = {
  member_max_attempts: 2,
  member_retry_interval_seconds: 1,
  member_non_stream_response_timeout_seconds: 120,
  member_stream_first_event_timeout_seconds: 30,
  member_cooldown_seconds: 60,
  member_affinity_seconds: 0,
};

// PREVIEW_DEBOUNCE_MS 是规则输入到预览请求的防抖间隔: 逐键请求会把输入态与响应态混在一起。
const PREVIEW_DEBOUNCE_MS = 300;

// PROTOCOL_TAGS 是凭据行上的协议标识。
// 此处写全称: 凭据行只有名称一列, 横向有余量; 渠道表单的授权矩阵是三列复选框, 列宽紧张才用缩写。
const PROTOCOL_TAGS = [
  { bit: Protocol.OpenAIChatCompletion, label: "Chat" },
  { bit: Protocol.OpenAIResponse, label: "Response" },
  { bit: Protocol.AnthropicMessage, label: "Message" },
];

// FieldHelp 渲染配置字段的简短帮助提示。
function FieldHelp({ text }: { text: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <HelpCircle className="size-4 cursor-help text-muted-foreground" />
      </TooltipTrigger>
      <TooltipContent side="top" sideOffset={10} align="center">
        {text}
      </TooltipContent>
    </Tooltip>
  );
}

function ModelPickerSection({
  grantMembers,
  selectedMembers,
  onAdd,
  patternBar,
}: {
  grantMembers: SelectedMember[];
  selectedMembers: SelectedMember[];
  onAdd: (channel: SelectedMember) => void;
  patternBar: ReactNode; // 自动匹配规则输入行, 由外层组装好查询状态后传入。
}) {
  const t = useTranslations("group");
  const [searchKeyword, setSearchKeyword] = useState("");

  const selectedKeys = useMemo(
    () => new Set(selectedMembers.map(memberKey)),
    [selectedMembers],
  );
  const normalizedSearch = searchKeyword.trim().toLowerCase();

  // 候选按渠道 -> 模型 -> 凭据三级组织: 一个模型可有多份凭据, 各自是独立授权, 需再展开一级才能分别选取。
  // 三级顺序沿用后端给出的候选顺序: Map 保留插入顺序, 后端已按渠道, 模型, 凭据排好, 此处无需再排。
  const channels = useMemo(() => {
    const byChannel = new Map<
      number,
      {
        id: number;
        name: string;
        models: Map<string, SelectedMember[]>;
      }
    >();
    grantMembers.forEach((mc) => {
      let channel = byChannel.get(mc.channel_id);
      if (!channel) {
        channel = {
          id: mc.channel_id,
          name: mc.channel_name,
          models: new Map(),
        };
        byChannel.set(mc.channel_id, channel);
      }
      const grants = channel.models.get(mc.name);
      if (grants) grants.push(mc);
      else channel.models.set(mc.name, [mc]);
    });

    return Array.from(byChannel.values()).map((channel) => ({
      id: channel.id,
      name: channel.name,
      models: Array.from(channel.models, ([name, grants]) => ({
        name,
        grants,
      })),
    }));
  }, [grantMembers]);

  const filteredChannels = useMemo(() => {
    if (!normalizedSearch) return channels;
    return channels.reduce<typeof channels>((acc, channel) => {
      if (channel.name.toLowerCase().includes(normalizedSearch)) {
        acc.push(channel);
        return acc;
      }

      const models = channel.models.filter((model) =>
        model.name.toLowerCase().includes(normalizedSearch),
      );
      if (models.length > 0) acc.push({ ...channel, models });
      return acc;
    }, []);
  }, [channels, normalizedSearch]);

  return (
    <div className="rounded-xl border border-border/50 bg-muted/30 flex flex-col min-h-0">
      <div className="flex items-center justify-between gap-2 px-3 py-2 border-b border-border/30 bg-muted/50">
        <span className="min-w-0 text-sm font-medium text-foreground">
          {t("form.addItem")}
        </span>

        <div className="relative w-30">
          <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={searchKeyword}
            onChange={(event) => setSearchKeyword(event.target.value)}
            className="h-6 rounded-lg border-border/60 bg-background/70 pl-7 pr-2 text-xs shadow-none focus-visible:border-border/60 focus-visible:ring-0"
            aria-label="search"
          />
        </div>
      </div>

      {patternBar}

      <div className="flex-1 min-h-0 overflow-y-auto p-2">
        <Accordion type="multiple" className="w-full space-y-2">
          {filteredChannels.map((channel) => {
            // 计数按授权算而非按模型: 展开后每份凭据都是一个可选项。
            const grants = channel.models.flatMap((model) => model.grants);
            const total = grants.length;
            const selectedCount = grants.reduce(
              (acc, m) => acc + (selectedKeys.has(memberKey(m)) ? 1 : 0),
              0,
            );
            const available = total - selectedCount;

            return (
              <AccordionItem key={channel.id} value={`channel-${channel.id}`}>
                <AccordionPrimitive.Header className="rounded-lg bg-muted sticky top-0 z-10 flex px-2 overflow-hidden">
                  <AccordionPrimitive.Trigger className="flex flex-1 min-w-0 items-center gap-4 py-4 text-left text-sm transition-all outline-none focus-visible:ring-[3px] disabled:pointer-events-none disabled:opacity-50 [&[data-state=open]>svg]:rotate-180">
                    <span className="truncate">{channel.name}</span>
                    <span className="text-xs text-muted-foreground shrink-0">
                      {available}/{total}
                    </span>
                    <ChevronDownIcon className="text-muted-foreground pointer-events-none size-4 shrink-0 transition-transform duration-200" />
                  </AccordionPrimitive.Trigger>
                </AccordionPrimitive.Header>
                <AccordionContent className="px-2 pt-2">
                  <div className="flex flex-col gap-1.5">
                    {channel.models.map((model) => {
                      const { Icon, className: iconClassName } = getModelIcon(
                        model.name,
                      );
                      const modelSelected = model.grants.reduce(
                        (acc, m) =>
                          acc + (selectedKeys.has(memberKey(m)) ? 1 : 0),
                        0,
                      );
                      return (
                        <div
                          key={model.name}
                          className="rounded-lg border border-border/50 bg-background"
                        >
                          {/* 模型行只作分组标题, 不可点选: 可选的是它下面的凭据, 一份凭据一条授权。 */}
                          <div className="flex items-center justify-between gap-2 px-2.5 py-2">
                            <span className="flex items-center gap-2 min-w-0">
                              <Icon
                                aria-hidden="true"
                                className={iconClassName}
                                width={16}
                                height={16}
                              />
                              <span className="text-sm font-medium truncate">
                                {model.name}
                              </span>
                            </span>
                            <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
                              {model.grants.length - modelSelected}/
                              {model.grants.length}
                            </span>
                          </div>

                          <div className="flex flex-col border-t border-border/50">
                            {model.grants.map((m) => {
                              const isSelected = selectedKeys.has(memberKey(m));
                              return (
                                <button
                                  key={memberKey(m)}
                                  type="button"
                                  onClick={() => !isSelected && onAdd(m)}
                                  disabled={isSelected}
                                  className={cn(
                                    "flex w-full items-center justify-between gap-2 px-2.5 py-1.5 pl-8 text-left transition-colors",
                                    isSelected
                                      ? "opacity-60 cursor-not-allowed"
                                      : "hover:bg-muted",
                                  )}
                                >
                                  <span className="flex min-w-0 items-center gap-2">
                                    <span className="truncate text-xs text-muted-foreground">
                                      {m.key_name}
                                    </span>
                                    {/* 标出该凭据讲得通的协议: 同一模型的不同凭据可能只支持其中一部分, 选之前就要能看出来。 */}
                                    {PROTOCOL_TAGS.map(
                                      ({ bit, label }) =>
                                        (m.protocols & bit) !== 0 && (
                                          <span
                                            key={bit}
                                            className="shrink-0 rounded border border-border/60 px-1 text-[10px] leading-4 text-muted-foreground"
                                          >
                                            {label}
                                          </span>
                                        ),
                                    )}
                                  </span>
                                  <span className="shrink-0 text-muted-foreground">
                                    {isSelected ? (
                                      <Check className="size-4 text-primary" />
                                    ) : (
                                      <Plus className="size-4" />
                                    )}
                                  </span>
                                </button>
                              );
                            })}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </AccordionContent>
              </AccordionItem>
            );
          })}
        </Accordion>
      </div>
    </div>
  );
}

function SortSection({
  members,
  onReorder,
  onRemove,
  removingIds,
  onClear,
  view,
  onViewChange,
  previewAvailable,
  preview,
}: {
  members: SelectedMember[];
  onReorder: (members: SelectedMember[]) => void;
  onRemove: (id: string) => void;
  removingIds: Set<string>;
  onClear: () => void;
  view: "selected" | "preview";
  onViewChange: (view: "selected" | "preview") => void;
  previewAvailable: boolean; // 仅规则非空时提供预览分段。
  preview: ReactNode;
}) {
  const t = useTranslations("group");

  const tabClass = (active: boolean) =>
    cn(
      "px-2 py-1 rounded-lg text-sm font-medium transition-colors",
      active
        ? "bg-background text-foreground"
        : "text-muted-foreground hover:text-foreground",
    );

  return (
    <div className="rounded-xl border border-border/50 bg-muted/30 flex flex-col min-h-0">
      <div className="flex items-center justify-between px-3 py-2 border-b border-border/30 bg-muted/50">
        <div className="flex items-center gap-1">
          <button
            type="button"
            onClick={() => onViewChange("selected")}
            className={tabClass(view === "selected")}
          >
            {t("form.items")}
            {members.length > 0 && (
              <span className="ml-1 text-xs text-muted-foreground font-normal">
                ({members.length})
              </span>
            )}
          </button>
          {previewAvailable && (
            <button
              type="button"
              onClick={() => onViewChange("preview")}
              className={tabClass(view === "preview")}
            >
              {t("form.previewTab")}
            </button>
          )}
        </div>
        {view === "selected" && (
          <button
            type="button"
            onClick={onClear}
            disabled={members.length === 0}
            className={cn(
              "flex items-center gap-1 px-2 py-1 rounded-lg text-xs font-medium transition-colors",
              members.length === 0
                ? "text-muted-foreground/50 cursor-not-allowed"
                : "hover:bg-muted text-muted-foreground hover:text-foreground",
            )}
          >
            <Trash2 className="size-3.5" />
            <span>{t("form.clear")}</span>
          </button>
        )}
      </div>

      <div className="flex-1 min-h-0">
        {view === "selected" ? (
          <MemberList
            members={members}
            onReorder={onReorder}
            onRemove={onRemove}
            removingIds={removingIds}
            showConfirmDelete={false}
          />
        ) : (
          preview
        )}
      </div>
    </div>
  );
}

// PreviewRow 是预览里的一条授权: 模型、渠道与凭据取自后端候选, 命中状态由所在分组标题表达。
function PreviewRow({
  candidate,
  dimmed,
}: {
  candidate: ChannelGrantCandidate;
  dimmed: boolean;
}) {
  const { Icon, className: iconClassName } = getModelIcon(candidate.model_name);
  return (
    <div
      className={cn(
        "flex items-center gap-2 rounded-lg border border-border/50 bg-background px-2.5 py-1.5",
        dimmed && "opacity-60",
      )}
    >
      <Icon
        aria-hidden="true"
        className={iconClassName}
        width={16}
        height={16}
      />
      <div className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-sm leading-tight">
          {candidate.model_name}
        </span>
        <span className="truncate text-[10px] leading-tight text-muted-foreground">
          {candidate.key_name
            ? `${candidate.channel_name} · ${candidate.key_name}`
            : candidate.channel_name}
        </span>
      </div>
      {PROTOCOL_TAGS.map(
        ({ bit, label }) =>
          (candidate.protocols & bit) !== 0 && (
            <span
              key={bit}
              className="shrink-0 rounded border border-border/60 px-1 text-[10px] leading-4 text-muted-foreground"
            >
              {label}
            </span>
          ),
      )}
    </div>
  );
}

// PatternPreview 展示规则当前命中的授权, 按"将加入 / 已在列表 / 不可用"分组。
// 只是保存前的对照视图: 真正的补齐在保存时由服务端按同一规则重算, 此处不构成数量承诺。
function PatternPreview({
  status,
  candidates,
  selectedGrantIds,
}: {
  status: "matching" | "invalid" | "failed" | "ready";
  candidates: ChannelGrantCandidate[];
  selectedGrantIds: Set<number>;
}) {
  const t = useTranslations("group");

  if (status !== "ready" || candidates.length === 0) {
    const text =
      status === "matching"
        ? t("form.matching")
        : status === "invalid"
          ? t("form.patternInvalid")
          : status === "failed"
            ? t("form.previewFailed")
            : t("form.previewEmpty");
    return (
      <div className="flex h-full items-center justify-center px-4 text-center text-sm text-muted-foreground">
        {text}
      </div>
    );
  }

  // 已排除的成员仍在 selectedGrantIds 中, 自然归入"已在列表", 不会被当成新增。
  const willAdd = candidates.filter(
    (c) => c.available && !selectedGrantIds.has(c.id),
  );
  const already = candidates.filter((c) => selectedGrantIds.has(c.id));
  const unavailable = candidates.filter(
    (c) => !c.available && !selectedGrantIds.has(c.id),
  );

  const groups = [
    { label: t("form.willAdd"), rows: willAdd, dimmed: false },
    { label: t("form.alreadyAdded"), rows: already, dimmed: true },
    { label: t("form.unavailableSkip"), rows: unavailable, dimmed: true },
  ];

  return (
    <div className="h-full overflow-y-auto p-2">
      {groups.map(
        (group) =>
          group.rows.length > 0 && (
            <div
              key={group.label}
              className="mb-2 flex flex-col gap-1.5 last:mb-0"
            >
              <span className="px-1 text-[11px] font-medium text-muted-foreground">
                {group.label} ({group.rows.length})
              </span>
              {group.rows.map((candidate) => (
                <PreviewRow
                  key={candidate.id}
                  candidate={candidate}
                  dimmed={group.dimmed}
                />
              ))}
            </div>
          ),
      )}
    </div>
  );
}

export function GroupEditor({
  initial,
  submitText,
  submittingText,
  isSubmitting,
  onSubmit,
  onCancel,
}: {
  initial?: {
    name?: string;
    mode?: GroupMode;
    relay_config?: Partial<GroupRelayConfig>;
    members?: SelectedMember[];
    auto_add_pattern?: string;
  };
  submitText: string;
  submittingText: string;
  isSubmitting: boolean;
  onSubmit: (values: GroupEditorValues) => void;
  onCancel?: () => void;
}) {
  const t = useTranslations("group");
  const { data: grantCandidates = [] } = useChannelGrantList();
  const grantMembers = useMemo<SelectedMember[]>(
    () =>
      grantCandidates.map((grant) => ({
        id: String(grant.id),
        channel_grant_id: grant.id,
        name: grant.model_name,
        enabled: grant.available,
        channel_id: grant.channel_id,
        channel_name: grant.channel_name,
        key_name: grant.key_name,
        protocols: grant.protocols,
      })),
    [grantCandidates],
  );

  const [groupName, setGroupName] = useState(initial?.name ?? "");
  const [mode, setMode] = useState<GroupMode>(initial?.mode ?? "manual");
  const [relayConfig, setRelayConfig] = useState<GroupRelayConfig>(() => ({
    ...defaultRelayConfig,
    ...initial?.relay_config,
  }));
  const [selectedMembers, setSelectedMembers] = useState<SelectedMember[]>(
    initial?.members ?? [],
  );
  const [removingIds, setRemovingIds] = useState<Set<string>>(new Set());

  const groupKey = normalizeKey(groupName);

  const [pattern, setPattern] = useState(initial?.auto_add_pattern ?? "");
  const debouncedPattern = useDebounce(pattern, PREVIEW_DEBOUNCE_MS);
  const [rightView, setRightView] = useState<"selected" | "preview">(
    "selected",
  );

  const selectedGrantIds = useMemo(
    () => new Set(selectedMembers.map((m) => m.channel_grant_id)),
    [selectedMembers],
  );

  // 防抖窗口内或请求未返回时不展示任何旧结果: 计数与预览要么对应当前输入, 要么显示"匹配中"。
  // 预览按查询键隔离, 旧 pattern 的响应不会落到新 pattern 上; 判定矩阵抽为纯函数见 utils.resolvePreview。
  const previewEnabled = previewQueryEnabled(pattern, debouncedPattern);
  const previewQuery = useChannelGrantPreview(debouncedPattern, previewEnabled);
  // 后端 400 仅用于非法/不支持的正则, 据此拦截保存; 其余错误(网络等)不阻塞, 保存时服务端会再校验。
  const previewError =
    previewQuery.error == null
      ? null
      : previewQuery.error instanceof ApiError &&
          previewQuery.error.status === 400
        ? "rejected"
        : "failed";
  const preview = resolvePreview(pattern, debouncedPattern, {
    pending: previewQuery.isPending,
    success: previewQuery.isSuccess,
    error: previewError,
  });
  const patternInvalid = preview.invalid;
  const previewStatus = preview.status;
  const previewCandidates = preview.showCandidates
    ? (previewQuery.data ?? [])
    : [];

  const handleAddMember = useCallback((channel: SelectedMember) => {
    const key = memberKey(channel);
    setSelectedMembers((prev) => {
      if (prev.some((m) => m.id === key)) return prev;
      return [...prev, { ...channel, id: key }];
    });
  }, []);

  // 规则命中的成员即便移除, 保存时也会被服务端补齐; 指出真正的排除入口, 避免"删了又回来"的困惑。
  // 前端不做本地正则判定(Go 语法与 JS 不兼容且用户正则可能灾难回溯): 规则非空即给条件文案提示。
  const notifyPatternRemoval = useCallback(() => {
    if (pattern.length > 0) {
      toast.info(t("form.removeMatchedHint"));
    }
  }, [pattern, t]);

  const handleRemoveMember = useCallback(
    (id: string) => {
      notifyPatternRemoval();
      setRemovingIds((prev) => new Set(prev).add(id));
      setTimeout(() => {
        setSelectedMembers((prev) => prev.filter((m) => m.id !== id));
        setRemovingIds((prev) => {
          const n = new Set(prev);
          n.delete(id);
          return n;
        });
      }, 200);
    },
    [notifyPatternRemoval],
  );

  const handleClearMembers = useCallback(() => {
    notifyPatternRemoval();
    setSelectedMembers([]);
    setRemovingIds(new Set());
  }, [notifyPatternRemoval]);

  // 允许纯规则创建: 名称必填, 成员与规则至少其一; 规则被后端判无效(或超长)时禁止提交。
  const isValid =
    groupKey.length > 0 &&
    (selectedMembers.length > 0 || pattern.length > 0) &&
    !patternInvalid;

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!isValid) return;
    onSubmit({
      name: groupName,
      mode,
      relay_config: relayConfig,
      auto_add_pattern: pattern,
      members: selectedMembers,
    });
  };

  const matchedAddCount = previewCandidates.filter(
    (c) => c.available && !selectedGrantIds.has(c.id),
  ).length;

  const patternBar = (
    <div className="flex flex-col gap-1 border-b border-border/30 bg-muted/50 px-3 py-2">
      <div className="flex items-center gap-2">
        <Input
          value={pattern}
          onChange={(event) => setPattern(event.target.value)}
          placeholder={t("form.autoAddPatternPlaceholder")}
          aria-label={t("form.autoAddPattern")}
          aria-invalid={patternInvalid || undefined}
          className={cn(
            "h-7 flex-1 rounded-lg border-border/60 bg-background/70 px-2 text-xs shadow-none focus-visible:ring-0",
            patternInvalid && "border-destructive",
          )}
        />
        <FieldHelp text={t("form.autoAddPatternHint")} />
        {pattern.length > 0 && previewStatus === "matching" && (
          <span className="shrink-0 text-xs text-muted-foreground">
            {t("form.matching")}
          </span>
        )}
        {pattern.length > 0 && previewStatus === "ready" && (
          <button
            type="button"
            onClick={() => setRightView("preview")}
            className="shrink-0 text-xs text-muted-foreground transition-colors hover:text-foreground"
          >
            {t("form.matchedSummary", {
              total: previewCandidates.length,
              add: matchedAddCount,
            })}
          </button>
        )}
      </div>
      {patternInvalid && (
        <p className="text-[11px] text-destructive">
          {preview.tooLong
            ? t("form.patternTooLong")
            : t("form.patternInvalid")}
        </p>
      )}
    </div>
  );

  return (
    <form onSubmit={handleSubmit} className="flex flex-col h-full min-h-0 ">
      <div className="flex-1 min-h-0 overflow-hidden px-1">
        <FieldGroup className="gap-4 flex flex-col min-h-0 h-full">
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            <Field>
              <FieldLabel htmlFor="group-name">{t("form.name")}</FieldLabel>
              <Input
                id="group-name"
                value={groupName}
                onChange={(e) => setGroupName(e.target.value)}
                className="rounded-xl"
              />
            </Field>
            <Field>
              <FieldLabel htmlFor="group-mode">
                {t("form.mode")}
                <FieldHelp text={t("form.modeHint")} />
              </FieldLabel>
              <Select
                value={mode}
                onValueChange={(value) => setMode(value as GroupMode)}
              >
                <SelectTrigger id="group-mode" className="w-full rounded-xl">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="manual">{t("form.manual")}</SelectItem>
                  <SelectItem value="failover">{t("form.failover")}</SelectItem>
                  <SelectItem value="scored">{t("form.scored")}</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          </div>

          <Tabs defaultValue="members" className="flex flex-1 min-h-0">
            <TabsList className="grid w-full shrink-0 grid-cols-2">
              <TabsTrigger value="members">{t("form.members")}</TabsTrigger>
              <TabsTrigger value="relay">{t("form.relay")}</TabsTrigger>
            </TabsList>

            <TabsContent value="members" className="min-h-0 overflow-hidden">
              <div className="grid h-full min-h-0 grid-cols-1 gap-4 md:grid-cols-2">
                <ModelPickerSection
                  grantMembers={grantMembers}
                  selectedMembers={selectedMembers}
                  onAdd={handleAddMember}
                  patternBar={patternBar}
                />
                <SortSection
                  members={selectedMembers}
                  onReorder={setSelectedMembers}
                  onRemove={handleRemoveMember}
                  removingIds={removingIds}
                  onClear={handleClearMembers}
                  view={pattern.length > 0 ? rightView : "selected"}
                  onViewChange={setRightView}
                  previewAvailable={pattern.length > 0}
                  preview={
                    <PatternPreview
                      status={previewStatus}
                      candidates={previewCandidates}
                      selectedGrantIds={selectedGrantIds}
                    />
                  }
                />
              </div>
            </TabsContent>

            <TabsContent value="relay" className="min-h-0 overflow-y-auto px-1">
              <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                {/* 评分路由不使用成员重试配置, 按最高分选路即可, 故只保留超时字段。 */}
                {mode !== "scored" && (
                  <>
                    <Field>
                      <FieldLabel htmlFor="group-retry-count">
                        {t("form.retryCount")}
                        <FieldHelp text={t("form.retryCountHint")} />
                      </FieldLabel>
                      <Input
                        id="group-retry-count"
                        type="number"
                        inputMode="numeric"
                        min={0}
                        step={1}
                        value={String(relayConfig.member_max_attempts)}
                        onChange={(event) => {
                          const value = Number.parseInt(event.target.value, 10);
                          setRelayConfig((prev) => ({
                            ...prev,
                            member_max_attempts:
                              Number.isFinite(value) && value >= 1 ? value : 1,
                          }));
                        }}
                        className="rounded-xl"
                      />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="group-retry-interval">
                        {t("form.retryInterval")}
                        <FieldHelp text={t("form.retryIntervalHint")} />
                      </FieldLabel>
                      <Input
                        id="group-retry-interval"
                        type="number"
                        inputMode="numeric"
                        min={1}
                        step={1}
                        value={String(
                          relayConfig.member_retry_interval_seconds,
                        )}
                        onChange={(event) => {
                          const value = Number.parseInt(event.target.value, 10);
                          setRelayConfig((prev) => ({
                            ...prev,
                            member_retry_interval_seconds:
                              Number.isFinite(value) && value >= 1 ? value : 1,
                          }));
                        }}
                        className="rounded-xl"
                      />
                    </Field>
                  </>
                )}
                <Field>
                  <FieldLabel htmlFor="group-non-stream-timeout">
                    {t("form.nonStreamTimeout")}
                    <FieldHelp text={t("form.nonStreamTimeoutHint")} />
                  </FieldLabel>
                  <Input
                    id="group-non-stream-timeout"
                    type="number"
                    inputMode="numeric"
                    min={1}
                    step={1}
                    value={String(
                      relayConfig.member_non_stream_response_timeout_seconds,
                    )}
                    onChange={(event) => {
                      const value = Number.parseInt(event.target.value, 10);
                      setRelayConfig((prev) => ({
                        ...prev,
                        member_non_stream_response_timeout_seconds:
                          Number.isFinite(value) && value >= 1 ? value : 1,
                      }));
                    }}
                    className="rounded-xl"
                  />
                </Field>
                <Field>
                  <FieldLabel htmlFor="group-stream-timeout">
                    {t("form.streamTimeout")}
                    <FieldHelp text={t("form.streamTimeoutHint")} />
                  </FieldLabel>
                  <Input
                    id="group-stream-timeout"
                    type="number"
                    inputMode="numeric"
                    min={1}
                    step={1}
                    value={String(
                      relayConfig.member_stream_first_event_timeout_seconds,
                    )}
                    onChange={(event) => {
                      const value = Number.parseInt(event.target.value, 10);
                      setRelayConfig((prev) => ({
                        ...prev,
                        member_stream_first_event_timeout_seconds:
                          Number.isFinite(value) && value >= 1 ? value : 1,
                      }));
                    }}
                    className="rounded-xl"
                  />
                </Field>
                {mode !== "scored" && (
                  <>
                    <Field>
                      <FieldLabel htmlFor="group-cooldown">
                        {t("form.cooldown")}
                        <FieldHelp text={t("form.cooldownHint")} />
                      </FieldLabel>
                      <Input
                        id="group-cooldown"
                        type="number"
                        inputMode="numeric"
                        min={1}
                        step={1}
                        value={String(relayConfig.member_cooldown_seconds)}
                        onChange={(event) => {
                          const value = Number.parseInt(event.target.value, 10);
                          setRelayConfig((prev) => ({
                            ...prev,
                            member_cooldown_seconds:
                              Number.isFinite(value) && value >= 1 ? value : 1,
                          }));
                        }}
                        className="rounded-xl"
                      />
                    </Field>
                    <Field>
                      <FieldLabel htmlFor="group-affinity">
                        {t("form.affinity")}
                        <FieldHelp text={t("form.affinityHint")} />
                      </FieldLabel>
                      <Input
                        id="group-affinity"
                        type="number"
                        inputMode="numeric"
                        min={0}
                        step={1}
                        value={String(relayConfig.member_affinity_seconds)}
                        onChange={(event) => {
                          const value = Number.parseInt(event.target.value, 10);
                          setRelayConfig((prev) => ({
                            ...prev,
                            member_affinity_seconds:
                              Number.isFinite(value) && value >= 0 ? value : 0,
                          }));
                        }}
                        className="rounded-xl"
                      />
                    </Field>
                  </>
                )}
              </div>
            </TabsContent>
          </Tabs>
        </FieldGroup>
      </div>

      <div className="pt-4 mt-auto shrink-0">
        <div className="flex gap-2">
          {onCancel && (
            <Button
              type="button"
              variant="secondary"
              className="flex-1 rounded-xl h-11"
              onClick={onCancel}
            >
              {t("detail.actions.cancel")}
            </Button>
          )}
          <Button
            type="submit"
            disabled={!isValid || isSubmitting}
            className="flex-1 rounded-xl h-11"
          >
            {isSubmitting ? submittingText : submitText}
          </Button>
        </div>
      </div>
    </form>
  );
}
