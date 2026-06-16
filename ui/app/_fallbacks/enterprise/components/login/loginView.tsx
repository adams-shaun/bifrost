import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectSeparator, SelectTrigger, SelectValue } from "@/components/ui/select";
import { getErrorMessage, useCreateTenantMutation, useGetTenantsPublicQuery, useIsAuthEnabledQuery, useLoginMutation } from "@/lib/store/apis";
import { useCurrentTenant } from "@/lib/store/hooks/useCurrentTenant";
import { BooksIcon, DiscordLogoIcon, GithubLogoIcon } from "@phosphor-icons/react";
import { useNavigate } from "@tanstack/react-router";
import { Eye, EyeOff } from "lucide-react";
import { useTheme } from "next-themes";
import { useEffect, useState } from "react";

const externalLinks = [
	{
		title: "Discord Server",
		url: "https://discord.gg/exN5KAydbU",
		icon: DiscordLogoIcon,
	},
	{
		title: "GitHub Repository",
		url: "https://github.com/maximhq/bifrost",
		icon: GithubLogoIcon,
	},
	{
		title: "Full Documentation",
		url: "https://docs.getbifrost.ai",
		icon: BooksIcon,
		strokeWidth: 1,
	},
];

export default function LoginView() {
	const { resolvedTheme } = useTheme();
	const [mounted, setMounted] = useState(false);
	const [username, setUsername] = useState("");
	const [password, setPassword] = useState("");
	const [showPassword, setShowPassword] = useState(false);
	const [errorMessage, setErrorMessage] = useState("");
	const [isCheckingAuth, setIsCheckingAuth] = useState(true);
	const navigate = useNavigate();
	const [isLoading, setIsLoading] = useState(false);
	const { data: isAuthEnabledData, isLoading: isLoadingIsAuthEnabled, error: isAuthEnabledError } = useIsAuthEnabledQuery();
	const isAuthEnabled = isAuthEnabledData?.is_auth_enabled || false;
	const hasValidToken = isAuthEnabledData?.has_valid_token || false;
	const [login, { isLoading: isLoggingIn }] = useLoginMutation();

	// Tenant picker. Backed by the public /api/session/tenants endpoint
	// (whitelisted from auth) so the dropdown can populate before the
	// user has credentials. The endpoint returns only {id, name, status}.
	// Phase 4's OIDC plugin will want this list narrowed per claims;
	// when that lands, this query moves to a post-login step.
	const { currentTenantID, setCurrentTenant } = useCurrentTenant();
	const { data: tenants, isLoading: isLoadingTenants, error: tenantsError } = useGetTenantsPublicQuery();
	const [selectedTenantID, setSelectedTenantID] = useState<string>("");

	// "+ New tenant" mode: when the user picks the sentinel option below
	// the existing tenants, the picker switches to two text inputs (id
	// + name) and on submit we (a) log in with the typed credentials
	// then (b) POST /api/platform/tenants with the new id+name then
	// (c) set it as the current tenant and navigate. The POST is
	// authenticated by the cookie the login leaves behind, so a stub
	// AdminAuthz today and an OIDC-backed authz check in Phase 4 both
	// gate it without code changes here.
	const CREATE_NEW_SENTINEL = "__create-new-tenant__";
	const [newTenantID, setNewTenantID] = useState("");
	const [newTenantName, setNewTenantName] = useState("");
	const [createTenant, { isLoading: isCreatingTenant }] = useCreateTenantMutation();
	const isCreatingNew = selectedTenantID === CREATE_NEW_SENTINEL;

	// Seed selectedTenantID from (a) the previously-persisted tenant if it
	// still exists in the list, otherwise (b) the first tenant returned —
	// usually "default", which the initial migration always seeds.
	useEffect(() => {
		if (!tenants || tenants.length === 0) return;
		if (selectedTenantID) return;
		const persisted = currentTenantID && tenants.find((t) => t.id === currentTenantID) ? currentTenantID : null;
		setSelectedTenantID(persisted ?? tenants[0].id);
	}, [tenants, currentTenantID, selectedTenantID]);

	useEffect(() => {
		setMounted(true);
	}, []);

	// Check auth status on component mount
	useEffect(() => {
		if (isLoadingIsAuthEnabled) {
			return;
		}
		if (isAuthEnabledError) {
			setErrorMessage("Unable to verify authentication status. Please retry.");
			return;
		}
		if (!isAuthEnabled || hasValidToken) {
			navigate({ to: "/workspace" });
			return;
		}
		// Auth is enabled but user is not logged in, show login form
		setIsCheckingAuth(false);
	}, [isLoadingIsAuthEnabled]);

	const handleSubmit = async (e: React.FormEvent<HTMLFormElement>) => {
		setIsLoading(true);
		e.preventDefault();
		setErrorMessage("");
		try {
			// Step 1: authenticate. The server sets the session cookie on
			// success; that cookie also authorizes the optional tenant
			// create call below, so login must come first.
			await login({ username, password }).unwrap();

			let tenantToActivate = selectedTenantID;
			if (isCreatingNew) {
				const id = newTenantID.trim();
				const name = newTenantName.trim() || id;
				if (!id) {
					setErrorMessage("Tenant id is required when creating a new tenant.");
					return;
				}
				try {
					await createTenant({ id, name, status: "active" }).unwrap();
				} catch (err: any) {
					// 409 = already exists; treat as "activate this one" so
					// repeated clicks on Sign in don't strand the user. Any
					// other error surfaces.
					if (err?.status !== 409) {
						setErrorMessage(getErrorMessage(err));
						return;
					}
				}
				tenantToActivate = id;
			}

			// Persist the chosen tenant BEFORE navigation so the workspace
			// shell picks it up on the very first render. setCurrentTenant
			// writes to localStorage + Redux atomically.
			if (tenantToActivate) {
				setCurrentTenant(tenantToActivate);
			}
			navigate({ to: "/workspace" });
		} catch (error) {
			const message = getErrorMessage(error);
			setErrorMessage(message);
		} finally {
			setIsLoading(false);
		}
	};

	// Use light logo for SSR to avoid hydration mismatch
	const logoSrc = mounted && resolvedTheme === "dark" ? "/bifrost-logo-dark.webp" : "/bifrost-logo.webp";

	// Show loading state while checking auth
	if (isCheckingAuth || isLoadingIsAuthEnabled) {
		return (
			<div className="flex min-h-screen items-center justify-center p-4">
				<div className="w-full max-w-md">
					<div className="border-border bg-card w-full space-y-6 rounded-sm border p-8">
						<div className="flex items-center justify-center">
							<img src={logoSrc} alt="Bifrost" width={160} height={26} className="" />
						</div>
						<div className="flex items-center justify-center py-8">
							<div className="text-muted-foreground text-sm">Checking authentication...</div>
						</div>
					</div>
				</div>
			</div>
		);
	}

	return (
		<div className="flex min-h-screen items-center justify-center p-4">
			<div className="w-full max-w-md">
				<div className="border-border bg-card w-full space-y-6 rounded-sm border p-8">
					{/* Logo */}
					<div className="flex items-center justify-center">
						<img src={logoSrc} alt="Bifrost" width={160} height={26} className="" />
					</div>

					<div className="space-y-2 text-center">
						<h1 className="text-foreground text-lg font-semibold">Welcome back</h1>
						<p className="text-muted-foreground text-sm">Sign in to your account to continue</p>
					</div>

					<form onSubmit={handleSubmit} className="space-y-5">
						{errorMessage && <div className="bg-destructive/10 text-destructive rounded-sm p-3 text-sm">{errorMessage}</div>}

						<div className="space-y-2">
							<Label htmlFor="username" className="text-sm font-medium">
								Username
							</Label>
							<Input
								id="username"
								type="text"
								placeholder="Enter your username"
								value={username}
								onChange={(e) => setUsername(e.target.value)}
								required
								className="text-sm"
								autoComplete="username"
							/>
						</div>

						<div className="space-y-2">
							<Label htmlFor="password" className="text-sm font-medium">
								Password
							</Label>
							<div className="relative">
								<Input
									id="password"
									type={showPassword ? "text" : "password"}
									placeholder="Enter your password"
									value={password}
									onChange={(e) => setPassword(e.target.value)}
									required
									className="pr-10 text-sm"
									autoComplete="current-password"
								/>
								<button
									type="button"
									onClick={() => setShowPassword(!showPassword)}
									className="text-muted-foreground hover:text-foreground absolute top-1/2 right-3 -translate-y-1/2 transition-colors"
									aria-label={showPassword ? "Hide password" : "Show password"}
								>
									{showPassword ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
								</button>
							</div>
						</div>

						<div className="space-y-2">
							<Label htmlFor="tenant" className="text-sm font-medium">
								Tenant
							</Label>
							{/* Tenant picker. Shows a Select when there are 2+ tenants
							    OR the user wants to create one (always). The "+ New
							    tenant…" sentinel flips the picker into a two-input
							    create-on-login mode (id + name), authenticated by the
							    login that runs first in handleSubmit. */}
							{tenantsError ? (
								<div className="text-muted-foreground text-xs">
									Unable to list tenants. You can still sign in; the workspace will use the previously-selected tenant.
								</div>
							) : isLoadingTenants ? (
								<div className="text-muted-foreground text-xs">Loading tenants…</div>
							) : (
								<>
									<Select value={selectedTenantID} onValueChange={setSelectedTenantID}>
										<SelectTrigger id="tenant" className="text-sm">
											<SelectValue placeholder="Select a tenant" />
										</SelectTrigger>
										<SelectContent>
											{(tenants ?? []).map((t) => (
												<SelectItem key={t.id} value={t.id}>
													{t.name} {t.status !== "active" ? `(${t.status})` : ""}
												</SelectItem>
											))}
											{tenants && tenants.length > 0 ? <SelectSeparator /> : null}
											<SelectItem value={CREATE_NEW_SENTINEL} data-testid="tenant-option-new">
												+ New tenant…
											</SelectItem>
										</SelectContent>
									</Select>
									{isCreatingNew ? (
										<div className="space-y-2 pt-2">
											<Input
												id="new-tenant-id"
												data-testid="new-tenant-id"
												type="text"
												placeholder="tenant-id (slug)"
												value={newTenantID}
												onChange={(e) => setNewTenantID(e.target.value)}
												className="text-sm"
												autoComplete="off"
												required
											/>
											<Input
												id="new-tenant-name"
												data-testid="new-tenant-name"
												type="text"
												placeholder="Display name (optional)"
												value={newTenantName}
												onChange={(e) => setNewTenantName(e.target.value)}
												className="text-sm"
												autoComplete="off"
											/>
											<p className="text-muted-foreground text-xs">
												Creating a tenant requires platform-admin permissions. The signed-in account is used for authorization.
											</p>
										</div>
									) : null}
								</>
							)}
						</div>

						<Button type="submit" className="h-9 w-full text-sm" isLoading={isLoading} disabled={isLoading}>
							{isCreatingTenant
								? "Creating tenant…"
								: isLoading || isLoggingIn
								? "Signing in..."
								: isCreatingNew
								? "Sign in & create tenant"
								: "Sign in"}
						</Button>
					</form>

					{/* Social Links */}
					<div className="flex items-center justify-center gap-4 pt-4">
						{externalLinks.map((item, index) => (
							<a
								key={index}
								href={item.url}
								target="_blank"
								rel="noopener noreferrer"
								className="text-muted-foreground hover:text-primary transition-colors"
								title={item.title}
							>
								<item.icon className="h-5 w-5" size={20} weight="regular" strokeWidth={item.strokeWidth} />
							</a>
						))}
					</div>
				</div>
			</div>
		</div>
	);
}