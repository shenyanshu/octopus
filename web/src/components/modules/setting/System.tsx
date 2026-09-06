import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslations } from "use-intl";
import {
  Monitor,
  Globe,
  Clock,
  RefreshCw,
  Shield,
  HelpCircle,
  X,
} from "lucide-react";
import { Input } from "@/components/ui/input";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { useSettingList, useSetSetting, SettingKey } from "@/api/setting";
import {
  MODEL_SYNC_INTERVAL_MAX_HOURS,
  MODEL_SYNC_INTERVAL_MIN_HOURS,
  parseSyncIntervalHours,
} from "@/api/channel-sync";
import { toast } from "sonner";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

export function SettingSystem() {
  const t = useTranslations("setting");
  const { data: settings } = useSettingList();
  const setSetting = useSetSetting();

  const [proxyUrl, setProxyUrl] = useState("");
  const [statsSaveInterval, setStatsSaveInterval] = useState("");
  const [modelSyncInterval, setModelSyncInterval] = useState("");
  const [corsAllowOrigins, setCorsAllowOrigins] = useState("");
  const [corsInputValue, setCorsInputValue] = useState("");

  const initialProxyUrl = useRef("");
  const initialStatsSaveInterval = useRef("");
  const initialModelSyncInterval = useRef("");
  const initialCorsAllowOrigins = useRef("");

  useEffect(() => {
    if (settings) {
      const proxy = settings.find((s) => s.key === SettingKey.ProxyURL);
      const interval = settings.find(
        (s) => s.key === SettingKey.StatsSaveInterval,
      );
      const syncInterval = settings.find(
        (s) => s.key === SettingKey.ModelSyncInterval,
      );
      const cors = settings.find((s) => s.key === SettingKey.CORSAllowOrigins);
      if (proxy) {
        queueMicrotask(() => setProxyUrl(proxy.value));
        initialProxyUrl.current = proxy.value;
      }
      if (interval) {
        queueMicrotask(() => setStatsSaveInterval(interval.value));
        initialStatsSaveInterval.current = interval.value;
      }
      if (syncInterval) {
        queueMicrotask(() => setModelSyncInterval(syncInterval.value));
        initialModelSyncInterval.current = syncInterval.value;
      }
      if (cors) {
        queueMicrotask(() => setCorsAllowOrigins(cors.value));
        initialCorsAllowOrigins.current = cors.value;
      }
    }
  }, [settings]);

  const handleSave = (key: string, value: string, initialValue: string) => {
    if (value === initialValue) return;

    setSetting.mutate(
      { key, value },
      {
        onSuccess: () => {
          toast.success(t("saved"));
          if (key === SettingKey.ProxyURL) {
            initialProxyUrl.current = value;
          } else if (key === SettingKey.StatsSaveInterval) {
            initialStatsSaveInterval.current = value;
          } else if (key === SettingKey.ModelSyncInterval) {
            initialModelSyncInterval.current = value;
          } else if (key === SettingKey.CORSAllowOrigins) {
            initialCorsAllowOrigins.current = value;
          }
        },
      },
    );
  };

  // 同步周期本地校验: 仅接受 1..168 的整数小时(没有 0 关闭档); 非法输入不落库、不提示已保存,
  // 还原为当前生效值并给出原因。保存失败沿用 handleSave 的静默, 输入保留待用户修正。
  const handleSyncIntervalBlur = () => {
    const hours = parseSyncIntervalHours(modelSyncInterval);
    if (hours === null) {
      if (modelSyncInterval !== initialModelSyncInterval.current) {
        toast.error(t("modelSyncInterval.invalid"));
      }
      setModelSyncInterval(initialModelSyncInterval.current);
      return;
    }
    // 归一化(如 '06' -> '6')后再比较与提交, 避免无意义的重复保存。
    const normalized = String(hours);
    setModelSyncInterval(normalized);
    handleSave(
      SettingKey.ModelSyncInterval,
      normalized,
      initialModelSyncInterval.current,
    );
  };

  const corsAllowOriginsList = useMemo(() => {
    const value = corsAllowOrigins.trim();
    if (!value) return [];
    if (value === "*") return ["*"];
    return Array.from(
      new Set(
        value
          .split(/[,\n，]/)
          .map((item) => item.trim())
          .filter(Boolean),
      ),
    );
  }, [corsAllowOrigins]);

  const corsAllowOriginsDisplay = useMemo(
    () =>
      corsAllowOriginsList.length > 0
        ? corsAllowOriginsList.join(", ")
        : t("corsAllowOrigins.hint"),
    [corsAllowOriginsList, t],
  );

  const saveCorsAllowOrigins = (origins: string[]) => {
    const normalizedOrigins = Array.from(
      new Set(origins.map((origin) => origin.trim()).filter(Boolean)),
    );
    const normalizedValue = normalizedOrigins.includes("*")
      ? "*"
      : normalizedOrigins.join(",");
    setCorsAllowOrigins(normalizedValue);
    handleSave(
      SettingKey.CORSAllowOrigins,
      normalizedValue,
      initialCorsAllowOrigins.current,
    );
  };

  const handleAddCorsOrigin = () => {
    const newOrigins = Array.from(
      new Set(
        corsInputValue
          .split(/[,\n，]/)
          .map((item) => item.trim())
          .filter(Boolean),
      ),
    );
    if (newOrigins.length === 0) return;

    if (newOrigins.includes("*")) {
      saveCorsAllowOrigins(["*"]);
      setCorsInputValue("");
      return;
    }

    const base = corsAllowOriginsList.includes("*") ? [] : corsAllowOriginsList;
    const merged = Array.from(new Set([...base, ...newOrigins]));
    saveCorsAllowOrigins(merged);
    setCorsInputValue("");
  };

  const handleRemoveCorsOrigin = (originToRemove: string) => {
    const nextOrigins = corsAllowOriginsList.filter(
      (origin) => origin !== originToRemove,
    );
    saveCorsAllowOrigins(nextOrigins);
  };

  return (
    <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
      <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
        <Monitor className="h-5 w-5" />
        {t("system")}
      </h2>

      {/* 代理地址 */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <Globe className="h-5 w-5 text-muted-foreground" />
          <span className="text-sm font-medium">{t("proxyUrl.label")}</span>
        </div>
        <Input
          value={proxyUrl}
          onChange={(e) => setProxyUrl(e.target.value)}
          onBlur={() =>
            handleSave("proxy_url", proxyUrl, initialProxyUrl.current)
          }
          placeholder={t("proxyUrl.placeholder")}
          className="w-48 rounded-xl"
        />
      </div>

      {/* 统计保存周期 */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <Clock className="h-5 w-5 text-muted-foreground" />
          <span className="text-sm font-medium">
            {t("statsSaveInterval.label")}
          </span>
        </div>
        <Input
          type="number"
          value={statsSaveInterval}
          onChange={(e) => setStatsSaveInterval(e.target.value)}
          onBlur={() =>
            handleSave(
              "stats_save_interval",
              statsSaveInterval,
              initialStatsSaveInterval.current,
            )
          }
          placeholder={t("statsSaveInterval.placeholder")}
          className="w-48 rounded-xl"
        />
      </div>

      {/* 模型同步周期 */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <RefreshCw className="h-5 w-5 text-muted-foreground" />
          <span className="text-sm font-medium">
            {t("modelSyncInterval.label")}
          </span>
          <Tooltip>
            <TooltipTrigger asChild>
              <HelpCircle className="size-4 text-muted-foreground cursor-help" />
            </TooltipTrigger>
            <TooltipContent side="top" sideOffset={10} align="center">
              {t("modelSyncInterval.hint")}
            </TooltipContent>
          </Tooltip>
        </div>
        <Input
          type="number"
          min={MODEL_SYNC_INTERVAL_MIN_HOURS}
          max={MODEL_SYNC_INTERVAL_MAX_HOURS}
          step={1}
          value={modelSyncInterval}
          onChange={(e) => setModelSyncInterval(e.target.value)}
          onBlur={handleSyncIntervalBlur}
          placeholder={t("modelSyncInterval.placeholder")}
          className="w-48 rounded-xl"
        />
      </div>

      {/* CORS 跨域白名单 */}
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <Shield className="h-5 w-5 text-muted-foreground" />
          <span className="text-sm font-medium">
            {t("corsAllowOrigins.label")}
          </span>
          <Tooltip>
            <TooltipTrigger asChild>
              <HelpCircle className="size-4 text-muted-foreground cursor-help" />
            </TooltipTrigger>
            <TooltipContent side="top" sideOffset={10} align="center">
              {t("corsAllowOrigins.hint")}
              <br />
              {t("corsAllowOrigins.example")}
            </TooltipContent>
          </Tooltip>
        </div>
        <Popover>
          <PopoverTrigger asChild>
            <button
              type="button"
              className="border-input focus-visible:border-ring focus-visible:ring-ring/50 w-48 min-h-9 rounded-xl border bg-transparent px-3 py-2 text-left text-sm shadow-xs transition-[color,box-shadow] outline-none focus-visible:ring-[3px]"
            >
              <span
                className={`block overflow-hidden text-ellipsis whitespace-nowrap ${corsAllowOriginsList.length === 0 ? "text-muted-foreground" : ""}`}
              >
                {corsAllowOriginsDisplay}
              </span>
            </button>
          </PopoverTrigger>
          <PopoverContent className="w-72 space-y-2 rounded-3xl p-3 bg-card">
            <Input
              value={corsInputValue}
              onChange={(e) => setCorsInputValue(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  handleAddCorsOrigin();
                }
              }}
              placeholder={t("corsAllowOrigins.example")}
              className="h-9 rounded-xl"
              autoFocus
            />
            <div className="max-h-48 space-y-1 overflow-y-auto">
              {corsAllowOriginsList.length > 0 &&
                corsAllowOriginsList.map((origin) => (
                  <div
                    key={origin}
                    className="flex items-center justify-between gap-2 rounded-xl border border-border/60 px-2 py-1"
                  >
                    <span className="break-all text-xs leading-5">
                      {origin}
                    </span>
                    <button
                      type="button"
                      onClick={() => handleRemoveCorsOrigin(origin)}
                      className="text-muted-foreground transition-colors hover:text-destructive"
                      aria-label={`remove ${origin}`}
                    >
                      <X className="size-4" />
                    </button>
                  </div>
                ))}
            </div>
          </PopoverContent>
        </Popover>
      </div>
    </div>
  );
}
