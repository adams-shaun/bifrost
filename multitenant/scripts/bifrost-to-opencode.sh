#!/usr/bin/env bash
#
# bifrost-to-opencode.sh — translate the Bifrost configuration attached
# to a Virtual Key into an opencode.json file.
#
# What it does
# ------------
# - Hits the Bifrost admin API live to read the VK (matched by value),
#   its provider_configs (provider + allowed_models per provider), and
#   the per-provider model catalog when allowed_models is wildcard.
# - Emits an opencode config with one provider entry per upstream
#   provider the VK is permitted to reach. Each entry points at the
#   Bifrost gateway (/v1) and carries the VK as the Bearer key plus the
#   x-f5xc-tenant header so the request path resolves correctly.
#
# Why an OpenAI-compatible block per upstream provider
# ----------------------------------------------------
# opencode looks up a model under <providerKey>/<modelID>. Putting every
# upstream behind a single fake "bifrost" provider would force a flat
# model namespace and lose the per-provider model split that the UI
# (and the user's mental model) expect. One block per upstream keeps
# claude-3-5-sonnet under "bifrost-anthropic" and gpt-4o-mini under
# "bifrost-openai".
#
# Usage
# -----
#   bifrost-to-opencode.sh --vk sk-bf-... --tenant acme \
#     --bifrost-url https://admin.codeburro2.net \
#     [--inference-url https://api.codeburro2.net] \
#     [--admin-user admin] [--admin-pass HUNTER2] \
#     [--default-model bifrost-anthropic/claude-3-5-sonnet-20241022] \
#     [--output ~/.config/opencode/opencode.json]
#
# Env-var fallbacks (so the flags can stay out of shell history):
#   BIFROST_URL, BIFROST_INFERENCE_URL, BIFROST_TENANT, BIFROST_VK,
#   BIFROST_ADMIN_USER, BIFROST_ADMIN_PASS
#
# Exit codes
# ----------
#   0 success, 1 bad input, 2 API error, 3 VK not found, 4 missing tool

set -euo pipefail

# ---- dependency check ------------------------------------------------

for tool in curl jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "error: $tool is required but not on PATH" >&2
		exit 4
	fi
done

# ---- flag / env parsing ----------------------------------------------

bifrost_url="${BIFROST_URL:-}"
inference_url="${BIFROST_INFERENCE_URL:-}"
tenant="${BIFROST_TENANT:-}"
vk="${BIFROST_VK:-}"
admin_user="${BIFROST_ADMIN_USER:-}"
admin_pass="${BIFROST_ADMIN_PASS:-}"
default_model=""
output=""

usage() {
	sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--bifrost-url)     bifrost_url="$2"; shift 2;;
		--inference-url)   inference_url="$2"; shift 2;;
		--tenant)          tenant="$2"; shift 2;;
		--vk)              vk="$2"; shift 2;;
		--admin-user)      admin_user="$2"; shift 2;;
		--admin-pass)      admin_pass="$2"; shift 2;;
		--default-model)   default_model="$2"; shift 2;;
		--output)          output="$2"; shift 2;;
		-h|--help)         usage 0;;
		*)                 echo "error: unknown flag $1" >&2; usage 1;;
	esac
done

# Inference base defaults to the admin URL because in single-cluster
# deploys the gateway and the admin UI sit behind the same hostname.
# Override when /v1 has its own ingress (public inference, private admin).
inference_url="${inference_url:-$bifrost_url}"

# ---- validate --------------------------------------------------------

missing=()
[[ -z "$bifrost_url" ]] && missing+=("--bifrost-url / BIFROST_URL")
[[ -z "$tenant"      ]] && missing+=("--tenant / BIFROST_TENANT")
[[ -z "$vk"          ]] && missing+=("--vk / BIFROST_VK")
[[ -z "$admin_user"  ]] && missing+=("--admin-user / BIFROST_ADMIN_USER")
[[ -z "$admin_pass"  ]] && missing+=("--admin-pass / BIFROST_ADMIN_PASS")
if [[ ${#missing[@]} -gt 0 ]]; then
	printf 'error: missing required input:\n' >&2
	printf '  - %s\n' "${missing[@]}" >&2
	exit 1
fi

bifrost_url="${bifrost_url%/}"     # strip trailing slash so concatenation is clean
inference_url="${inference_url%/}"

# ---- API helpers -----------------------------------------------------

# api_get PATH — returns body on stdout, exits non-zero on HTTP error.
# Captures status separately so we can give a useful diagnostic instead
# of letting jq choke on the error envelope.
api_get() {
	local path="$1" body status
	body="$(mktemp)"
	status=$(curl -sS -o "$body" -w '%{http_code}' \
		-u "$admin_user:$admin_pass" \
		-H "x-f5xc-tenant: $tenant" \
		"$bifrost_url$path") || {
			rm -f "$body"
			echo "error: curl failed talking to $bifrost_url$path" >&2
			exit 2
		}
	if [[ "$status" != "200" ]]; then
		echo "error: GET $path -> HTTP $status" >&2
		cat "$body" >&2
		rm -f "$body"
		exit 2
	fi
	cat "$body"
	rm -f "$body"
}

# ---- step 1: locate the VK -------------------------------------------
#
# /api/governance/virtual-keys is paginated but the page shape is
# {virtual_keys: [...], count, total_count, ...}. We list, then filter
# by value client-side; the alternative (GET by id) would force the
# caller to know the internal UUID instead of the user-facing value
# they actually have in their config.

vk_list_json="$(api_get '/api/governance/virtual-keys')"

vk_json="$(echo "$vk_list_json" \
	| jq --arg v "$vk" '
		(.virtual_keys // [])
		| map(select(.value == $v))
		| .[0] // empty
	')"

if [[ -z "$vk_json" ]]; then
	echo "error: no virtual key with that value visible to tenant '$tenant'" >&2
	echo "       (the VK exists in another tenant, or the value is wrong)" >&2
	exit 3
fi

vk_id="$(echo "$vk_json" | jq -r '.id')"
vk_name="$(echo "$vk_json" | jq -r '.name')"
echo "info: matched VK id=$vk_id name=$vk_name" >&2

# ---- step 2: provider list + per-provider model catalog --------------
#
# For each entry in provider_configs we need the model list. allowed_models
# is the authoritative whitelist; "*" (wildcard) means "use whatever the
# provider currently exposes", so we resolve it by hitting /api/models.

providers_json="$(echo "$vk_json" | jq '
	(.provider_configs // [])
	| map({
		provider: .provider,
		allowed_models: (.allowed_models // [])
	})
')"

if [[ "$(echo "$providers_json" | jq 'length')" == "0" ]]; then
	echo "error: VK '$vk_name' has no provider_configs — opencode would have no models to call" >&2
	echo "       attach at least one provider to this VK before generating an opencode config" >&2
	exit 3
fi

resolve_models() {
	# resolve_models PROVIDER ALLOWED_JSON
	#   ALLOWED_JSON is a JSON array; "*" element means wildcard.
	local provider="$1" allowed="$2"
	if echo "$allowed" | jq -e 'index("*")' >/dev/null 2>&1; then
		# Wildcard — query catalog. /api/models returns
		# {models: ["model-id-1", "model-id-2", ...]} or similar; we
		# extract names defensively because the shape has drifted in
		# both directions across releases.
		local catalog
		catalog="$(api_get "/api/models?provider=$(jq -nr --arg p "$provider" '$p | @uri')&unfiltered=true&limit=1000")"
		echo "$catalog" | jq '
			# Accept either {models: ["id", ...]} or
			# {models: [{name: "id"}, ...]} or {models: [{id: "id"}, ...]}.
			(.models // [])
			| map(if type == "object" then (.id // .name // .model // empty) else . end)
			| map(select(. != null and . != ""))
			| unique
		'
	else
		echo "$allowed"
	fi
}

# ---- step 3: assemble opencode.json ----------------------------------

# We build the providers object provider-by-provider so a single bad
# upstream surfaces immediately rather than poisoning a giant jq pipeline.
providers_object='{}'
provider_names=()

while IFS= read -r row; do
	prov="$(echo "$row" | jq -r '.provider')"
	allowed="$(echo "$row" | jq -c '.allowed_models')"
	models="$(resolve_models "$prov" "$allowed")"
	if [[ "$(echo "$models" | jq 'length')" == "0" ]]; then
		echo "warn: provider '$prov' resolved to zero models — opencode entry will be empty" >&2
	fi
	# opencode keys providers under a short string; namespace with
	# "bifrost-" so the user can tell at a glance which provider blocks
	# came from this script vs. their own additions.
	key="bifrost-$prov"
	provider_names+=("$key")
	block="$(jq -n \
		--arg name "Bifrost / $prov" \
		--arg baseurl "$inference_url/v1" \
		--arg apikey "$vk" \
		--arg tenant "$tenant" \
		--argjson models "$models" \
		'{
			npm: "@ai-sdk/openai-compatible",
			name: $name,
			options: {
				baseURL: $baseurl,
				apiKey: $apikey,
				headers: {
					"x-f5xc-tenant": $tenant
				}
			},
			models: ($models | map({ (.): { name: . } }) | add // {})
		}')"
	providers_object="$(jq --arg k "$key" --argjson v "$block" '. + {($k): $v}' <<<"$providers_object")"
done < <(echo "$providers_json" | jq -c '.[]')

# Decide the top-level default model. Order of preference:
#   1. --default-model if the user asked for one (we trust them);
#   2. first model of the first provider, alphabetically by provider key
#      (deterministic across reruns).
if [[ -z "$default_model" ]]; then
	first_key="$(printf '%s\n' "${provider_names[@]}" | sort | head -n 1)"
	first_model="$(jq -r --arg k "$first_key" '.[$k].models | keys | sort | .[0] // empty' <<<"$providers_object")"
	if [[ -n "$first_key" && -n "$first_model" ]]; then
		default_model="$first_key/$first_model"
	fi
fi

result="$(jq -n \
	--argjson providers "$providers_object" \
	--arg model "$default_model" \
	'{
		"$schema": "https://opencode.ai/config.json",
		provider: $providers
	}
	| (if $model != "" then . + {model: $model} else . end)')"

if [[ -n "$output" ]]; then
	mkdir -p "$(dirname "$output")"
	echo "$result" > "$output"
	echo "info: wrote $output" >&2
else
	echo "$result"
fi
