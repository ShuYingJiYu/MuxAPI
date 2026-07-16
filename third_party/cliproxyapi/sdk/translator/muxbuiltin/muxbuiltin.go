// Package muxbuiltin exposes only the built-in translators used by MuxAPI.
package muxbuiltin

import (
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/chat-completions"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/chat-completions"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
)

// Registry returns the shared registry populated with MuxAPI's protocol pairs.
func Registry() *sdktranslator.Registry {
	return sdktranslator.Default()
}
