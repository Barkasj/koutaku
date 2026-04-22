import { getErrorMessage, useAppSelector, useUpdatePluginMutation } from "@/lib/store";
import { KoutakuConfigSchema, KoutakuFormSchema } from "@/lib/types/schemas";
import { useMemo } from "react";
import { toast } from "sonner";
import { KoutakuFormFragment } from "../../fragments/koutakuFormFragment";

interface KoutakuViewProps {
	onDelete?: () => void;
	isDeleting?: boolean;
}

export default function KoutakuView({ onDelete, isDeleting }: KoutakuViewProps) {
	const selectedPlugin = useAppSelector((state) => state.plugin.selectedPlugin);
	const [updatePlugin] = useUpdatePluginMutation();
	const currentConfig = useMemo(
		() => ({ ...((selectedPlugin?.config as KoutakuConfigSchema) ?? {}), enabled: selectedPlugin?.enabled }),
		[selectedPlugin],
	);

	const handleKoutakuConfigSave = (config: KoutakuFormSchema): Promise<void> => {
		return new Promise((resolve, reject) => {
			updatePlugin({
				name: "koutaku",
				data: {
					enabled: config.enabled,
					config: config.koutaku_config,
				},
			})
				.unwrap()
				.then(() => {
					toast.success("Koutaku configuration updated successfully");
					resolve();
				})
				.catch((err) => {
					toast.error("Failed to update Koutaku configuration", {
						description: getErrorMessage(err),
					});
					reject(err);
				});
		});
	};

	return (
		<div className="flex w-full flex-col gap-4">
			<div className="flex w-full flex-col gap-2">
				<div className="text-muted-foreground text-xs font-medium">Configuration</div>
				<div className="text-muted-foreground mb-2 text-xs font-normal">
					You can send in header <code>x-bf-log-repo-id</code> with a repository ID to log to a specific repository.
				</div>
				<KoutakuFormFragment onSave={handleKoutakuConfigSave} initialConfig={currentConfig} onDelete={onDelete} isDeleting={isDeleting} />
			</div>
		</div>
	);
}