## What's Changed

### Bug Fixes

- Read context/output limits and modalities from the nested `limit` and `modalities` objects in the dynamic OpenCode Go `/models` catalog, so registered models carry them again. Flat fields remain a fallback; malformed or non-positive values are ignored with a diagnostic instead of failing discovery. Routing is unchanged.

- Preserve DeepSeek `reasoning_content` across Responses tool turns on Chat Completions routes: streamed and non-streamed responses return it as a reasoning output item, which is restored to the matching assistant turn on replay. Assistant turns with no recoverable reasoning send an empty `reasoning_content` to DeepSeek models. Foreign reasoning summaries and encrypted reasoning are not replayed as full reasoning.

- Preserve Claude `output_config.effort` when routing Messages requests to Chat Completions and Responses models. The explicit effort now takes precedence over the level derived from a thinking budget; `auto` is still omitted on the Responses wire.

Special thanks to @samaluk in #19, #20, and #21.

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.2.2...v0.2.3
