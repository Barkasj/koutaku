import { Siren } from "lucide-react";
import ContactUsView from "../views/contactUsView";

export default function AlertChannelsView() {
	return (
		<div className="h-full w-full">
			<ContactUsView
				className="mx-auto min-h-[80vh]"
				icon={<Siren className="h-[5.5rem] w-[5.5rem]" strokeWidth={1} />}
				title="Unlock alert channels for better observability"
				description="This feature is a part of the Koutaku enterprise license. We would love to know more about your use case and how we can help you."
				readmeLink="https://docs.getkoutaku.ai/enterprise/alert-channels"
			/>
		</div>
	);
}