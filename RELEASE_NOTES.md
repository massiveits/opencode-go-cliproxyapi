## What's Changed

- Support CLIProxyAPI v8 and its generic plugin quota API.
- Return rolling, weekly, and monthly quotas for the credential selected by CLIProxyAPI.
- Advertise the quota provider as **OpenCode Go**.
- Remove the separate OpenCode Go quota page and custom management routes.
- Accept an optional `name` for each API key.
- Use readable account labels and filenames instead of key digests.
- Preserve existing auth IDs, custom labels, filenames, and host metadata.
- Prevent duplicate auth records at cold start when readable filenames exist.
- Reject invalid quota values and redact upstream request errors.

## Upgrade

1. Upgrade CLIProxyAPI to v8.0.0 or later.
2. Replace the plugin binary with the new build.
3. Restart CLIProxyAPI.

The generic endpoints are `GET /v0/management/quota/providers` and
`POST /v0/management/quota/fetch`. Both require the management key.
Quota reset is unsupported. A management dashboard must support the generic
quota API to show plugin refresh controls. Backend support alone does not
add those controls to older dashboard builds.

Existing credential files keep their names and stable IDs. Set `name:` on an
API key to choose its display label. New files use readable names.
