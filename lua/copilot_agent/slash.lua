-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.
--
-- slash.lua – provider-aware slash command router.
--
-- Dispatching strategy:
--   1. If the active provider is "claude", claude_slash.lua gets first
--      attempt at the command.  If it doesn't handle it, fall through to
--      copilot_slash.lua for shared commands (session, search, export …).
--   2. For all other providers ("copilot", default) commands go directly
--      to copilot_slash.lua.
--
-- All existing callers of `require('copilot_agent.slash')` continue to work
-- unchanged because this module exports the same public surface:
--   M.execute(text, opts)   – main entry point
--   M.set_input_mode(mode)  – forwarded to the active provider's module
--   M._extract_side_session_answer  – forwarded (used by commit.lua)

local session = require('copilot_agent.session')

local M = {}

-- ── Provider module resolution ─────────────────────────────────────────────

local function copilot_slash()
  return require('copilot_agent.copilot_slash')
end

local function claude_slash()
  return require('copilot_agent.claude_slash')
end

local function active_provider()
  if type(session.get_provider) == 'function' then
    return session.get_provider()
  end
  return 'copilot'
end

-- ── Command text parser (identical to the one in copilot_slash.lua) ────────

local function parse(text)
  text = vim.trim(text or '')
  if text:sub(1, 1) ~= '/' then
    return nil, nil
  end
  text = text:sub(2)
  local command, args = text:match('^([%w%-_]+)%s*(.*)')
  if not command then
    return nil, nil
  end
  return command:lower(), vim.trim(args or '')
end

-- ── Public API ─────────────────────────────────────────────────────────────

--- Execute a slash command, routing to the correct provider implementation.
function M.execute(text, opts)
  local command, args = parse(text)
  if not command then
    return false
  end

  local provider = active_provider()

  if provider == 'claude' then
    -- Let claude_slash have first attempt.
    local handled = claude_slash().execute(command, args, opts)
    if handled then
      return true
    end
  end

  -- Fall through to copilot_slash for all shared / copilot-specific commands.
  return copilot_slash().execute(text, opts)
end

--- Forward set_input_mode to the underlying provider module.
function M.set_input_mode(mode)
  return copilot_slash().set_input_mode(mode)
end

--- Forwarded for commit.lua compatibility.
M._extract_side_session_answer = nil  -- populated lazily below

local _meta = {}
_meta.__index = function(t, k)
  if k == '_extract_side_session_answer' then
    return copilot_slash()._extract_side_session_answer
  end
  return rawget(t, k)
end
setmetatable(M, _meta)

return M
