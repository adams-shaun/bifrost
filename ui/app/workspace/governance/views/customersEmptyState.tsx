import { Button } from "@/components/ui/button";
import { WalletCards } from "lucide-react";
import { ArrowUpRight } from "lucide-react";

const CUSTOMERS_DOCS_URL = "https://docs.getbifrost.ai/features/governance/virtual-keys#customers";

interface CustomersEmptyStateProps {
	onAddClick: () => void;
	canCreate?: boolean;
}

export function CustomersEmptyState({ onAddClick, canCreate = true }: CustomersEmptyStateProps) {
	return (
		<div className="flex min-h-[80vh] w-full flex-col items-center justify-center gap-4 py-16 text-center">
			<div className="text-muted-foreground">
				<WalletCards className="h-[5.5rem] w-[5.5rem]" strokeWidth={1} />
			</div>
			<div className="flex flex-col gap-1">
				<h1 className="text-muted-foreground text-xl font-medium">Namespaces have their own teams, budgets, and access controls</h1>
				<div className="text-muted-foreground mx-auto mt-2 max-w-[600px] text-sm font-normal">
					Create namespaces to group teams, assign spending limits, and set rate limits per namespace.
				</div>
				<div className="mx-auto mt-6 flex flex-row flex-wrap items-center justify-center gap-2">
					<Button
						variant="outline"
						aria-label="Read more about namespaces (opens in new tab)"
						data-testid="customer-button-read-more"
						onClick={() => {
							window.open(`${CUSTOMERS_DOCS_URL}?utm_source=bfd`, "_blank", "noopener,noreferrer");
						}}
					>
						Read more <ArrowUpRight className="text-muted-foreground h-3 w-3" />
					</Button>
					<Button aria-label="Add your first namespace" onClick={onAddClick} disabled={!canCreate} data-testid="customer-button-create">
						Add Namespace
					</Button>
				</div>
			</div>
		</div>
	);
}