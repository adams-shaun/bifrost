import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "path";

export default defineConfig({
	// Companion to the vitest 4.1.0 security bump (CVE-2026-47429, security/0002):
	// vitest 4.1.x honors tsconfig `jsx: "preserve"` and leaves JSX untransformed,
	// so vite's import-analysis fails on .tsx test imports (e.g. columns.test.ts).
	// The react plugin restores JSX transform for the test pipeline.
	plugins: [react()],
	resolve: {
		alias: {
			"@": path.resolve(__dirname, "."),
		},
	},
	test: {
		globals: true,
	},
});