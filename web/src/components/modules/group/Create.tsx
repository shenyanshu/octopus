import {
  MorphingDialogDescription,
  useMorphingDialog,
} from "@/components/ui/morphing-dialog";
import { useCreateGroup } from "@/api/group";
import { useTranslations } from "use-intl";
import { GroupEditor } from "./Editor";
import { toast } from "sonner";

export function CreateDialogContent() {
  const { setIsOpen } = useMorphingDialog();
  const createGroup = useCreateGroup();
  const t = useTranslations("group");

  return (
    <div className="w-screen max-w-full md:max-w-4xl h-[calc(100vh-2rem)] min-h-0 flex flex-col">
      <MorphingDialogDescription className="flex-1 min-h-0 overflow-hidden">
        <GroupEditor
          submitText={t("create.submit")}
          submittingText={t("create.submitting")}
          isSubmitting={createGroup.isPending}
          onCancel={() => setIsOpen(false)}
          onSubmit={({
            name,
            mode,
            relay_config,
            auto_add_pattern,
            members,
          }) => {
            createGroup.mutate(
              {
                name,
                mode,
                relay_config,
                auto_add_pattern,
                items: members.map((member) => ({
                  channel_grant_id: member.channel_grant_id,
                })),
              },
              {
                onSuccess: () => setIsOpen(false),
                onError: (error) =>
                  toast.error(t("toast.createFailed"), {
                    description: error.message,
                  }),
              },
            );
          }}
        />
      </MorphingDialogDescription>
    </div>
  );
}
