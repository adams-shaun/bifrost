import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdownMenu";
import { useGetTenantsQuery } from "@/lib/store/apis";
import { useCurrentTenant } from "@/lib/store/hooks/useCurrentTenant";
import { Buildings, CaretDown, Check } from "@phosphor-icons/react";

// TenantBadge — the in-workspace tenant indicator + switcher. Shows the
// current tenant's name with a dropdown listing every tenant the caller
// can see (today: all of them, since AdminAuthz is a stub; Phase 4 will
// scope this to the caller's grants).
//
// Designed for the workspace shell's top-right corner. Stays mounted
// across route changes so the active tenant is always visible.
//
// Behavior:
//   - If no tenant is currently selected (e.g. first paint before
//     the login flow seeded one), renders "Select tenant" with the
//     dropdown open-by-default affordance.
//   - If the persisted tenant id no longer exists in the list (e.g.
//     was deleted between sessions), falls back to "Unknown tenant"
//     and offers to switch.
//   - Switching is purely client-side state — the next API call to a
//     tenant-scoped endpoint will pick up the new id.
export default function TenantBadge() {
	const { currentTenantID, setCurrentTenant } = useCurrentTenant();
	const { data: tenants, isLoading } = useGetTenantsQuery();

	const current = tenants?.find((t) => t.id === currentTenantID) ?? null;
	const label = isLoading
		? "Loading tenants…"
		: current
		? current.name
		: currentTenantID
		? `Unknown tenant (${currentTenantID})`
		: "Select tenant";

	return (
		<DropdownMenu>
			<DropdownMenuTrigger asChild>
				<Button
					variant="outline"
					size="sm"
					className="h-8 gap-1.5 text-xs font-normal"
					data-testid="tenant-badge"
					aria-label="Active tenant"
				>
					<Buildings className="h-3.5 w-3.5 opacity-70" weight="regular" />
					<span className="max-w-[14rem] truncate">{label}</span>
					<CaretDown className="h-3 w-3 opacity-60" weight="bold" />
				</Button>
			</DropdownMenuTrigger>
			<DropdownMenuContent align="end" className="min-w-56">
				<DropdownMenuLabel className="text-muted-foreground text-xs font-normal">
					Switch tenant
				</DropdownMenuLabel>
				<DropdownMenuSeparator />
				{(tenants ?? []).map((t) => {
					const isCurrent = t.id === currentTenantID;
					return (
						<DropdownMenuItem
							key={t.id}
							onSelect={() => {
								if (!isCurrent) setCurrentTenant(t.id);
							}}
							className="flex items-center justify-between gap-2"
						>
							<span className="flex flex-col">
								<span className="text-sm">{t.name}</span>
								<span className="text-muted-foreground text-[10px]">{t.id}</span>
							</span>
							{isCurrent ? <Check className="h-3.5 w-3.5" weight="bold" /> : null}
						</DropdownMenuItem>
					);
				})}
				{!isLoading && (tenants?.length ?? 0) === 0 ? (
					<DropdownMenuItem disabled className="text-muted-foreground text-xs">
						No tenants configured
					</DropdownMenuItem>
				) : null}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
