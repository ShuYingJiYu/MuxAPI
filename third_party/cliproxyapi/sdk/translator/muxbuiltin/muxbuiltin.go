// Package muxbuiltin exposes the built-in translator registrations used by MuxAPI.
//
// The upstream SDK also ships translators for provider-specific protocols that
// are not part of MuxAPI's public routing matrix. Keeping this list explicit
// avoids registering unrelated protocol pairs while retaining the current
// OpenAI, Responses, Claude, Gemini, and Codex paths.
package muxbuiltin

import (
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/gemini"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/chat-completions"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/codex/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/codex/gemini"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/codex/openai/chat-completions"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/codex/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/gemini"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/chat-completions"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/gemini/openai/responses"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/claude"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/gemini"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/openai/openai/responses"
)

// Registry returns the shared registry populated with MuxAPI's protocol pairs.
func Registry() *sdktranslator.Registry {
	return sdktranslator.Default()
}
