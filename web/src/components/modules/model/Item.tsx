import {
  memo,
  useCallback,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";
import { Pencil, Trash2, ArrowDownToLine, ArrowUpFromLine } from "lucide-react";
import { motion, AnimatePresence } from "motion/react";
import { useTranslations } from "use-intl";
import {
  useUpdateModel,
  useDeleteModel,
  useRestoreAutoModelPrice,
  type LLMInfo,
} from "@/api/model";
import {
  initialEditValues,
  parseEditPrices,
  canInteract,
  type PriceField,
} from "@/api/model-price";
import { getModelIcon } from "@/lib/model-icons";
import { toast } from "sonner";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { ModelDeleteOverlay, ModelEditOverlay } from "./ItemOverlays";
import { cn } from "@/lib/utils";
import { createPortal } from "react-dom";

interface ModelItemProps {
  model: LLMInfo;
  layout?: "grid" | "list";
}

// editValuesFromModel 生成编辑浮层初始值: 价格未知时留空, 已知回显当前值。
function editValuesFromModel(model: LLMInfo): Record<PriceField, string> {
  return initialEditValues(model.price_known, model.price);
}

// 缓存编辑弹层实测高度，首次打开即可正确判断是否需要向上弹出
let cachedEditOverlayHeight = 0;

export const ModelItem = memo(function ModelItem({
  model,
  layout = "grid",
}: ModelItemProps) {
  const t = useTranslations("model");
  const isListLayout = layout === "list";
  const [isEditOpen, setIsEditOpen] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [overlayRect, setOverlayRect] = useState<{
    top: number;
    left: number;
    width: number;
  } | null>(null);
  const instanceId = useId();
  const editLayoutId = `edit-btn-${model.name}-${instanceId}`;
  const deleteLayoutId = `delete-btn-${model.name}-${instanceId}`;
  const cardRef = useRef<HTMLElement | null>(null);
  const [editValues, setEditValues] = useState(() =>
    editValuesFromModel(model),
  );

  const updateModel = useUpdateModel();
  const deleteModel = useDeleteModel();
  const restoreAuto = useRestoreAutoModelPrice();
  const [confirmRestore, setConfirmRestore] = useState(false);

  const {
    Icon,
    className: iconClassName,
    color: brandColor,
  } = useMemo(() => getModelIcon(model.name), [model.name]);

  const updateOverlayRect = useCallback(() => {
    const card = cardRef.current;
    if (!card) return;
    const rect = card.getBoundingClientRect();
    const height = cachedEditOverlayHeight;
    // 下方空间不足时改为向上弹出，避免最后一行弹层被视口底部截断
    const flipUp = height > 0 && rect.top + height > window.innerHeight;
    const top = flipUp
      ? Math.min(
          Math.max(rect.bottom - height, 0),
          Math.max(window.innerHeight - height, 0),
        )
      : rect.top;
    setOverlayRect((prev) => {
      if (
        prev &&
        prev.top === top &&
        prev.left === rect.left &&
        prev.width === rect.width
      ) {
        return prev;
      }
      return { top, left: rect.left, width: rect.width };
    });
  }, []);

  const closeEdit = useCallback(() => {
    setIsEditOpen(false);
  }, []);

  const handleOverlayHeightChange = useCallback(
    (height: number) => {
      if (height === cachedEditOverlayHeight) return;
      cachedEditOverlayHeight = height;
      updateOverlayRect();
    },
    [updateOverlayRect],
  );

  const handleEditClick = () => {
    setConfirmDelete(false);
    setEditValues(editValuesFromModel(model));
    // Ensure first open already has anchor geometry so layout animation can run.
    updateOverlayRect();
    setIsEditOpen(true);
  };

  const handleCancelEdit = () => {
    closeEdit();
  };

  // 编辑互斥: 恢复在途则保存拒, 反之亦然; 不依赖按钮 disabled(键盘/重复点击同样被挡)。
  const busy = !canInteract(updateModel.isPending, restoreAuto.isPending);

  const handleSaveEdit = () => {
    if (busy) return;
    const prices = parseEditPrices(editValues);
    if (!prices) {
      // 四价必填且为有限非负数; 不满足则不提交, 绝不让空值静默按 0 免费落库。
      toast.error(t("overlay.invalidPrice"));
      return;
    }
    updateModel.mutate(
      { name: model.name, ...prices },
      {
        onSuccess: () => {
          closeEdit();
          toast.success(t("toast.updated"));
        },
        onError: (error) => {
          toast.error(t("toast.updateFailed"), { description: error.message });
        },
      },
    );
  };

  const handleCancelRestore = () => {
    if (busy) return;
    setConfirmRestore(false);
  };

  const handleConfirmRestore = () => {
    if (busy) return;
    restoreAuto.mutate(model.name, {
      onSuccess: () => {
        // 恢复成功后回到参考价/未知, 编辑浮层关闭交给列表缓存刷新。
        setConfirmRestore(false);
        closeEdit();
        toast.success(t("overlay.restored"));
      },
      onError: (error) => {
        // 失败保留编辑输入与确认状态, 用户可重试或取消。
        setConfirmRestore(false);
        toast.error(t("overlay.restoreFailed"), { description: error.message });
      },
    });
  };

  const handleStartRestore = () => {
    if (busy) return;
    setConfirmRestore(true);
  };

  const handleDeleteClick = () => {
    closeEdit();
    setConfirmRestore(false);
    setConfirmDelete(true);
  };
  const handleCancelDelete = () => setConfirmDelete(false);
  const handleConfirmDelete = () => {
    deleteModel.mutate(model.name, {
      onSuccess: () => {
        setConfirmDelete(false);
        toast.success(t("toast.deleted"));
      },
      onError: (error) => {
        setConfirmDelete(false);
        toast.error(t("toast.deleteFailed"), { description: error.message });
      },
    });
  };

  useEffect(() => {
    if (!isEditOpen) return;

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") closeEdit();
    };

    updateOverlayRect();
    window.addEventListener("resize", updateOverlayRect);
    window.addEventListener("scroll", updateOverlayRect, true);
    document.addEventListener("keydown", handleKeyDown);

    return () => {
      window.removeEventListener("resize", updateOverlayRect);
      window.removeEventListener("scroll", updateOverlayRect, true);
      document.removeEventListener("keydown", handleKeyDown);
    };
  }, [isEditOpen, updateOverlayRect, closeEdit]);

  const shouldRenderEditPortal = isEditOpen || overlayRect !== null;

  return (
    <article
      ref={cardRef}
      className={cn(
        "group relative rounded-3xl border border-border bg-card flex items-center gap-3 p-4",
        (isEditOpen || confirmDelete) && "z-50",
      )}
    >
      <Icon
        aria-hidden="true"
        className={iconClassName}
        width={52}
        height={52}
      />

      <div className="flex-1 min-w-0 flex flex-col justify-center gap-2">
        <Tooltip>
          <TooltipTrigger asChild>
            <span className="w-fit max-w-full text-base font-semibold text-card-foreground leading-tight truncate">
              {model.name}
            </span>
          </TooltipTrigger>
          <TooltipContent
            key={model.name}
            side="top"
            sideOffset={10}
            align="center"
          >
            {model.name}
          </TooltipContent>
        </Tooltip>

        {isListLayout ? (
          <p className="flex items-center gap-2 overflow-hidden text-sm text-muted-foreground whitespace-nowrap">
            {!model.price_known || !model.price ? (
              <span className="text-muted-foreground italic">
                {t("card.unknownPrice")}
              </span>
            ) : (
              <>
                <span className="inline-flex items-center gap-1">
                  <ArrowDownToLine
                    className="size-3.5 shrink-0"
                    style={{ color: brandColor }}
                  />
                  {t("card.inputCache")}
                  <span className="tabular-nums">
                    {model.price.input.toFixed(2)}/
                    {model.price.cache_read.toFixed(2)}$
                  </span>
                </span>
                <span className="text-muted-foreground/60">|</span>
                <span className="inline-flex items-center gap-1 overflow-hidden">
                  <ArrowUpFromLine
                    className="size-3.5 shrink-0"
                    style={{ color: brandColor }}
                  />
                  {t("card.outputCache")}
                  <span className="tabular-nums truncate">
                    {model.price.output.toFixed(2)}/
                    {model.price.cache_write.toFixed(2)}$
                  </span>
                </span>
                <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground">
                  {model.source === "manual"
                    ? t("card.manual")
                    : t("card.auto")}
                </span>
              </>
            )}
          </p>
        ) : (
          <>
            {!model.price_known || !model.price ? (
              <p className="text-sm text-muted-foreground italic">
                {t("card.unknownPrice")}
              </p>
            ) : (
              <>
                <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                  <ArrowDownToLine
                    className="size-3.5"
                    style={{ color: brandColor }}
                  />
                  {t("card.inputCache")}
                  <span className="tabular-nums">
                    {model.price.input.toFixed(2)}/
                    {model.price.cache_read.toFixed(2)}$
                  </span>
                </p>
                <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                  <ArrowUpFromLine
                    className="size-3.5"
                    style={{ color: brandColor }}
                  />
                  {t("card.outputCache")}
                  <span className="tabular-nums">
                    {model.price.output.toFixed(2)}/
                    {model.price.cache_write.toFixed(2)}$
                  </span>
                  <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-xs">
                    {model.source === "manual"
                      ? t("card.manual")
                      : t("card.auto")}
                  </span>
                </p>
              </>
            )}
          </>
        )}
      </div>

      <div
        className={cn(
          isListLayout
            ? "shrink-0 flex items-center gap-2 self-center"
            : "shrink-0 flex flex-col justify-between self-stretch",
          (isEditOpen || confirmDelete) && "invisible pointer-events-none",
        )}
      >
        <motion.button
          layoutId={editLayoutId}
          type="button"
          onClick={handleEditClick}
          disabled={isEditOpen || confirmDelete}
          className="h-9 w-9 flex items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted disabled:opacity-50"
        >
          <Pencil className="size-4" />
        </motion.button>

        <motion.button
          layoutId={deleteLayoutId}
          type="button"
          onClick={handleDeleteClick}
          disabled={isEditOpen || confirmDelete}
          className="h-9 w-9 flex items-center justify-center rounded-lg bg-destructive/10 text-destructive transition-colors hover:bg-destructive hover:text-destructive-foreground disabled:opacity-50"
        >
          <Trash2 className="size-4" />
        </motion.button>
      </div>

      <AnimatePresence>
        {confirmDelete && (
          <ModelDeleteOverlay
            layoutId={deleteLayoutId}
            isPending={deleteModel.isPending}
            onCancel={handleCancelDelete}
            onConfirm={handleConfirmDelete}
          />
        )}
      </AnimatePresence>

      {shouldRenderEditPortal && typeof document !== "undefined"
        ? createPortal(
            <AnimatePresence onExitComplete={() => setOverlayRect(null)}>
              {isEditOpen && overlayRect && (
                <div
                  className="fixed z-[90]"
                  style={{
                    top: `${overlayRect.top}px`,
                    left: `${overlayRect.left}px`,
                    width: `${overlayRect.width}px`,
                  }}
                >
                  <ModelEditOverlay
                    layoutId={editLayoutId}
                    onHeightChange={handleOverlayHeightChange}
                    modelName={model.name}
                    brandColor={brandColor}
                    editValues={editValues}
                    isPending={updateModel.isPending || restoreAuto.isPending}
                    priceKnown={model.price_known && model.price !== null}
                    showRestoreAuto={model.source === "manual"}
                    isConfirmingRestore={confirmRestore}
                    onChange={setEditValues}
                    onCancel={handleCancelEdit}
                    onSave={handleSaveEdit}
                    onStartRestore={handleStartRestore}
                    onCancelRestore={handleCancelRestore}
                    onConfirmRestore={handleConfirmRestore}
                  />
                </div>
              )}
            </AnimatePresence>,
            document.body,
          )
        : null}
    </article>
  );
});
