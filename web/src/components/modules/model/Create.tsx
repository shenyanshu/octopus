import { useState } from "react";
import { toast } from "sonner";
import { useCreateModel } from "@/api/model";
import { parseEditPrices } from "@/api/model-price";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { Field, FieldLabel, FieldGroup } from "@/components/ui/field";
import {
  MorphingDialogDescription,
  useMorphingDialog,
} from "@/components/ui/morphing-dialog";
import { useTranslations } from "use-intl";

export function CreateDialogContent() {
  const { setIsOpen } = useMorphingDialog();
  const t = useTranslations("model.create");
  const createModel = useCreateModel();

  const [formData, setFormData] = useState({
    name: "",
    input: "",
    output: "",
    cache_read: "",
    cache_write: "",
  });

  const handleSubmit = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = formData.name.trim();
    if (!name || createModel.isPending) return;
    // 四价必填且为有限非负数; 不满足则不提交, 绝不让空值静默按 0 免费落库。
    const prices = parseEditPrices({
      input: formData.input,
      output: formData.output,
      cache_read: formData.cache_read,
      cache_write: formData.cache_write,
    });
    if (!prices) {
      toast.error(t("invalidPrice"));
      return;
    }

    createModel.mutate(
      { name, ...prices },
      {
        onSuccess: () => {
          setFormData({
            name: "",
            input: "",
            output: "",
            cache_read: "",
            cache_write: "",
          });
          setIsOpen(false);
        },
      },
    );
  };

  return (
    <div className="w-screen max-w-full md:max-w-xl">
      <MorphingDialogDescription>
        <form onSubmit={handleSubmit}>
          <FieldGroup className="gap-4">
            <Field>
              <FieldLabel htmlFor="model-name">{t("name")}</FieldLabel>
              <Input
                id="model-name"
                value={formData.name}
                onChange={(e) =>
                  setFormData({ ...formData, name: e.target.value })
                }
                className="rounded-xl"
              />
            </Field>
            <div className="grid grid-cols-2 gap-4">
              <Field>
                <FieldLabel htmlFor="model-input">{t("input")}</FieldLabel>
                <Input
                  id="model-input"
                  type="number"
                  step="any"
                  value={formData.input}
                  onChange={(e) =>
                    setFormData({ ...formData, input: e.target.value })
                  }
                  className="rounded-xl"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="model-output">{t("output")}</FieldLabel>
                <Input
                  id="model-output"
                  type="number"
                  step="any"
                  value={formData.output}
                  onChange={(e) =>
                    setFormData({ ...formData, output: e.target.value })
                  }
                  className="rounded-xl"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="model-cache-read">
                  {t("cacheRead")}
                </FieldLabel>
                <Input
                  id="model-cache-read"
                  type="number"
                  step="any"
                  value={formData.cache_read}
                  onChange={(e) =>
                    setFormData({ ...formData, cache_read: e.target.value })
                  }
                  className="rounded-xl"
                />
              </Field>
              <Field>
                <FieldLabel htmlFor="model-cache-write">
                  {t("cacheWrite")}
                </FieldLabel>
                <Input
                  id="model-cache-write"
                  type="number"
                  step="any"
                  value={formData.cache_write}
                  onChange={(e) =>
                    setFormData({ ...formData, cache_write: e.target.value })
                  }
                  className="rounded-xl"
                />
              </Field>
            </div>
            <div className="flex gap-2">
              <Button
                type="button"
                variant="secondary"
                onClick={() => setIsOpen(false)}
                className="flex-1 rounded-xl h-11"
              >
                {t("cancel")}
              </Button>
              <Button
                type="submit"
                disabled={createModel.isPending || !formData.name.trim()}
                className="flex-1 rounded-xl h-11"
              >
                {createModel.isPending ? t("submitting") : t("submit")}
              </Button>
            </div>
          </FieldGroup>
        </form>
      </MorphingDialogDescription>
    </div>
  );
}
