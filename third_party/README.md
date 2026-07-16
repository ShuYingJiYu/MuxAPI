# Third-party source

`cliproxyapi/` contains CLIProxyAPI v7.2.80, sourced from
`github.com/router-for-me/CLIProxyAPI/v7` at commit `09da52ad`.

MuxAPI uses its public translator SDK. The upstream MIT license is retained in
`cliproxyapi/LICENSE`.

`cliproxyapi/sdk/translator/muxbuiltin/` limits registration to the OpenAI,
Responses, Claude, and Codex protocol pairs used by MuxAPI. Hard-coded
Antigravity OAuth credentials were also removed from the snapshot; the upstream
Antigravity provider now reads `CLIPROXY_ANTIGRAVITY_CLIENT_ID` and
`CLIPROXY_ANTIGRAVITY_CLIENT_SECRET`.
