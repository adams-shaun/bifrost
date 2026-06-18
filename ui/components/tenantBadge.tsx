import { useState } from "react";
import { Button } from "@/components/ui/button";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuLabel,
	DropdownMenuSeparator,
	DropdownMenuTrigger,
} from "@/components/ui/dropdownMenu";
import { Input } from "@/components/ui/input";
import { useCreateTenantMutation, useGetTenantsQuery } from "@/lib/store/apis";
import { useCurrentTenant } from "@/lib/store/hooks/useCurrentTenant";
import { Buildings, CaretDown, Check, Plus } from "@phosphor-icons/react";

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
//   - The "+ new tenant" entry expands inline (no modal) into a small
//     id+name form that POSTs to /api/platform/tenants and auto-
//     switches into the newly minted tenant on success.
export default function TenantBadge() {
	const { currentTenantID, setCurrentTenant } = useCurrentTenant();
	const { data: tenants, isLoading } = useGetTenantsQuery();
	const [createTenant, { isLoading: creating }] = useCreateTenantMutation();

	const [isCreating, setIsCreating] = useState(false);
	const [newID, setNewID] = useState("");
	const [newName, setNewName] = useState("");
	const [error, setError] = useState<string | null>(null);

	const current = tenants?.find((t) => t.id === currentTenantID) ?? null;
	const label = isLoading
		? "Loading tenants…"
		: current
		? current.name
		: currentTenantID
		? `Unknown tenant (${currentTenantID})`
		: "Select tenant";

	const resetCreate = () => {
		setIsCreating(false);
		setNewID("");
		setNewName("");
		setError(null);
	};

	const submitCreate = async () => {
		const id = newID.trim();
		const name = newName.trim();
		if (!id || !name) {
			setError("id and name are required");
			return;
		}
		try {
			const res = await createTenant({ id, name }).unwrap();
			setCurrentTenant(res.tenant?.id ?? id);
			resetCreate();
		} catch (err: unknown) {
			// RTK Query surfaces { data: { error: { message } } } for
			// our SendError envelope; fall back to a generic string
			// when the shape is different (network failure, etc.).
			const msg =
				(err as { data?: { error?: { message?: string } } })?.data?.error?.message ??
				(err as { error?: string })?.error ??
				"failed to create tenant";
			setError(String(msg));
		}
	};

	return (
		<DropdownMenu
			onOpenChange={(open) => {
				if (!open) resetCreate();
			}}
		>
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
								if (isCurrent) return;
								setCurrentTenant(t.id);
								// Force a full reload on tenant switch so RTK Query
								// caches, in-memory selectors, and any component-local
								// state tied to the prior tenant are dropped cleanly.
								// Cheaper than auditing every slice/api for tenant-keyed
								// invalidation, and matches operator intuition: a tenant
								// switch is a workspace switch.
								window.location.reload();
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

				<DropdownMenuSeparator />

				{isCreating ? (
					// Inline create form — kept in the dropdown so the operator
					// stays in the switching flow. `e.preventDefault()` on every
					// key/click stops Radix from auto-closing the menu.
					<div
						className="flex flex-col gap-2 px-2 py-1.5"
						onClick={(e) => e.stopPropagation()}
						onKeyDown={(e) => {
							if (e.key === "Enter") {
								e.preventDefault();
								void submitCreate();
							} else if (e.key === "Escape") {
								e.preventDefault();
								resetCreate();
							}
						}}
					>
						<Input
							autoFocus
							value={newID}
							onChange={(e) => setNewID(e.target.value)}
							placeholder="tenant id (slug)"
							className="h-7 text-xs"
							data-testid="tenant-create-id"
						/>
						<Input
							value={newName}
							onChange={(e) => setNewName(e.target.value)}
							placeholder="display name"
							className="h-7 text-xs"
							data-testid="tenant-create-name"
						/>
						{error ? <p className="text-destructive text-[10px]">{error}</p> : null}
						<div className="flex gap-1.5">
							<Button
								size="sm"
								className="h-7 flex-1 text-xs"
								onClick={(e) => {
									e.preventDefault();
									void submitCreate();
								}}
								disabled={creating}
								data-testid="tenant-create-submit"
							>
								{creating ? "Creating…" : "Create"}
							</Button>
							<Button
								size="sm"
								variant="ghost"
								className="h-7 text-xs"
								onClick={(e) => {
									e.preventDefault();
									resetCreate();
								}}
								disabled={creating}
							>
								Cancel
							</Button>
						</div>
					</div>
				) : (
					<DropdownMenuItem
						onSelect={(e) => {
							// Keep the dropdown open so the form is visible.
							e.preventDefault();
							setIsCreating(true);
						}}
						className="flex items-center gap-2 text-sm"
						data-testid="tenant-create-trigger"
					>
						<Plus className="h-3.5 w-3.5" weight="bold" />
						<span>New tenant</span>
					</DropdownMenuItem>
				)}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
