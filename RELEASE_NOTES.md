## What's Changed

### Bug Fixes

- Preserve Claude `output_config.effort` when routing Messages requests to Chat Completions and Responses models. The explicit effort now takes precedence over the level derived from a thinking budget; `auto` is still omitted on the Responses wire.

- Preserve Claude image tool results when routing to Chat Completions: retain images in an associated user message after tool replies, preserve text, tool references and error markers, and include images in session derivation. The selected upstream model must support image input.

Special thanks to @c0mm4nd in #16.

- Fix `tool_result` translation when receiving `tool_reference` content blocks from Claude clients (such as ToolSearch, web search, or web fetch in Claude Code), preventing 500 errors and conversation lockouts on Chat Completions and Responses models like DeepSeek.

Special thanks to @SpiGAndromeda in #14.

## Upgrade Notes

- Replace the old plugin binary with the new release binary.
- Restart CLIProxyAPI after replacing the plugin.

**Full Changelog**: https://github.com/massiveits/opencode-go-cliproxyapi/compare/v0.2.1...v0.2.2
