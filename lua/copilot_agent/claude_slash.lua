-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.
--
-- claude_slash.lua – slash commands tailored for the Claude / Anthropic provider.
--
-- Commands implemented here mirror the Claude CLI's built-in slash commands:
--   /init          generate CLAUDE.md project guide
--   /memory        open / edit CLAUDE.md in the current project
--   /mcp           manage MCP servers registered for Claude (~/.claude/mcp-config.json)
--   /permissions   show or set Claude tool-permission rules
--   /effort        adjust reasoning effort (low / medium / high)
--   /plan          enter plan mode before a large change
--   /compact       summarise conversation history to reclaim context window
--   /context       show context-window usage breakdown
--   /diff          show a diff of files changed in the current session
--   /simplify      ask Claude to review & simplify recent changes
--   /review        read-only code review pass
--   /security-review  security-focused review pass
--   /clear         start fresh while keeping project memory (CLAUDE.md)
--   /rewind        roll code and conversation back to a checkpoint
--   /background    detach session to run as a background agent
--   /agents        open subagent manager
--   /batch         decompose a large change into parallel worktree tasks
--   /doctor        diagnose install / runtime issues
--   /debug         toggle debug logging (COPILOT_DEBUG)
--   /feedback      report a bug with session context
--   /btw           quick aside that won't bloat conversation history

local cfg = require('copilot_agent.config')
local http = require('copilot_agent.http')
local init_project = require('copilot_agent.project_init')
local render = require('copilot_agent.render')
local service = require('copilot_agent.service')
local session = require('copilot_agent.session')
local window = require('copilot_agent.window')

local state = cfg.state
local notify = cfg.notify
local append_entry = render.append_entry
local working_directory = service.working_directory

local M = {}

-- ── Helpers ────────────────────────────────────────────────────────────────

local function trim(s)
  return vim.trim(s or '')
end

local function dispatch_prompt(prompt, opts)
  -- Delegate to the session send path via the input module to avoid a
  -- circular dependency on slash.lua.
  local input = require('copilot_agent.input')
  if type(input.send_prompt) == 'function' then
    input.send_prompt(prompt, opts)
  else
    -- Fallback: append as a system note so it still surfaces.
    append_entry('system', prompt)
  end
end

local function show_markdown_result(title, lines)
  window.show_float_markdown(title, lines)
end

-- ── /init ──────────────────────────────────────────────────────────────────

local function init_command(args)
  -- Pass "claude" as the provider so project_init generates CLAUDE.md.
  return init_project.run(args, 'claude')
end

-- ── /memory ────────────────────────────────────────────────────────────────
-- Open the project CLAUDE.md for manual editing.

local function memory_command(args)
  local root = working_directory()
  local path = root .. '/CLAUDE.md'
  args = trim(args)

  if args == 'show' then
    if vim.fn.filereadable(path) == 1 then
      local lines = vim.fn.readfile(path)
      show_markdown_result('CLAUDE.md', lines)
    else
      append_entry('system', 'No CLAUDE.md found in ' .. root)
    end
    return true
  end

  if vim.fn.filereadable(path) == 1 then
    window.open_path_safely(path)
  else
    append_entry('system', 'No CLAUDE.md found – run /init to generate one.')
  end
  return true
end

-- ── /mcp ───────────────────────────────────────────────────────────────────
-- Manage MCP servers for Claude. Claude reads from:
--   ~/.claude/settings.json  (global)
--   <project>/.mcp.json      (project-local, shared)
--   <project>/.claude/settings.json  (project-local, personal)

local CLAUDE_MCP_PATHS = {
  global  = vim.fn.expand('~/.claude/settings.json'),
  project = nil, -- resolved lazily from working_directory()
  local_  = nil, -- resolved lazily from working_directory()
}

local function claude_mcp_config_paths()
  local wd = working_directory()
  return {
    global  = vim.fn.expand('~/.claude/settings.json'),
    project = wd .. '/.mcp.json',
    local_  = wd .. '/.claude/settings.json',
  }
end

local function read_json_file(path)
  if vim.fn.filereadable(path) ~= 1 then
    return nil
  end
  local ok, decoded = pcall(vim.fn.json_decode, table.concat(vim.fn.readfile(path), '\n'))
  if ok and type(decoded) == 'table' then
    return decoded
  end
  return nil
end

local function list_claude_mcp_servers()
  local paths = claude_mcp_config_paths()
  local entries = {}

  for scope, path in pairs(paths) do
    local payload = read_json_file(path)
    if payload then
      -- Claude uses { "mcpServers": { ... } } in settings.json
      -- and { "mcpServers": { ... } } or { "servers": { ... } } in .mcp.json
      local servers = payload.mcpServers or payload.servers or {}
      if type(servers) == 'table' then
        for name, config in pairs(servers) do
          if type(config) == 'table' then
            entries[#entries + 1] = {
              name   = name,
              scope  = scope,
              source = path,
              kind   = config.type or (config.command and 'stdio' or 'unknown'),
              url    = config.url,
              cmd    = config.command,
            }
          end
        end
      end
    end
  end

  return entries
end

local function mcp_show_command()
  local entries = list_claude_mcp_servers()
  if #entries == 0 then
    append_entry('system', 'No MCP servers configured for Claude.')
    return true
  end

  local lines = { '## MCP servers (Claude)', '' }
  for _, e in ipairs(entries) do
    local detail = e.kind
    if e.url then
      detail = detail .. '  ' .. e.url
    elseif e.cmd then
      detail = detail .. '  ' .. table.concat(e.cmd, ' ')
    end
    lines[#lines + 1] = string.format('- **%s** `[%s]` %s  _(source: %s)_',
      e.name, e.scope, detail, vim.fn.fnamemodify(e.source, ':~:.'))
  end

  show_markdown_result('MCP Servers – Claude', lines)
  return true
end

local function mcp_command(args)
  local action = trim(vim.split(trim(args), '%s+')[1])
  if action == '' or action == 'show' or action == 'list' then
    return mcp_show_command()
  end
  -- For add/remove/edit, open the project .mcp.json in the editor.
  if action == 'edit' or action == 'add' or action == 'remove' then
    local paths = claude_mcp_config_paths()
    window.open_path_safely(paths.project)
    return true
  end
  -- Global settings
  if action == 'global' then
    local paths = claude_mcp_config_paths()
    window.open_path_safely(paths.global)
    return true
  end
  append_entry('system', 'Usage: /mcp [show|edit|add|remove|global]')
  return true
end

-- ── /permissions ───────────────────────────────────────────────────────────
-- Show or set Claude tool-permission rules.

local function permissions_command(args)
  args = trim(args)
  dispatch_prompt('/permissions ' .. args, { provider = 'claude' })
  return true
end

-- ── /effort ────────────────────────────────────────────────────────────────

local EFFORT_LEVELS = { low = true, medium = true, high = true }

local function effort_command(args)
  local level = trim(args):lower()
  if level == '' then
    -- Show current effort level
    local current = state.session_id and
      (cfg.active_session_effort and cfg.active_session_effort(state.session_id)) or 'default'
    append_entry('system', 'Current reasoning effort: ' .. tostring(current))
    return true
  end
  if not EFFORT_LEVELS[level] then
    append_entry('system', 'Invalid effort level. Choose: low, medium, high')
    return true
  end
  if type(cfg.set_session_effort) == 'function' then
    cfg.set_session_effort(state.session_id, level)
  end
  append_entry('system', 'Reasoning effort set to: ' .. level)
  return true
end

-- ── /plan ──────────────────────────────────────────────────────────────────

local function plan_command()
  -- Delegate to copilot_slash plan_mode_command equivalent
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash.set_input_mode) == 'function' then
    copilot_slash.set_input_mode('plan')
  else
    append_entry('system', 'Switched to plan mode.')
  end
  return true
end

-- ── /compact ───────────────────────────────────────────────────────────────

local function compact_command()
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._compact_history) == 'function' then
    return copilot_slash._compact_history()
  end
  dispatch_prompt(
    'Please summarise the conversation so far into a compact form that preserves all context needed to continue this task.',
    { hidden = true }
  )
  return true
end

-- ── /context ───────────────────────────────────────────────────────────────

local function context_command()
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._context_command) == 'function' then
    return copilot_slash._context_command()
  end
  append_entry('system', 'Context window info is not available.')
  return true
end

-- ── /diff ──────────────────────────────────────────────────────────────────

local function diff_command(args)
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._diff_command) == 'function' then
    return copilot_slash._diff_command(args)
  end
  -- Fallback: run git diff and show in a float
  local wd = working_directory()
  local result = vim.fn.system('git -C ' .. vim.fn.shellescape(wd) .. ' diff --stat HEAD 2>&1')
  local lines = vim.split(result, '\n', { plain = true })
  show_markdown_result('Git diff (HEAD)', lines)
  return true
end

-- ── /simplify ──────────────────────────────────────────────────────────────

local function simplify_command(args)
  local target = trim(args)
  local prompt
  if target ~= '' then
    prompt = string.format(
      'Review `%s`. Apply quality and efficiency improvements: remove dead code, simplify logic, improve naming, and ensure consistent style. Make only safe changes that preserve behaviour.',
      target
    )
  else
    prompt = 'Review the files changed most recently in this session. Apply quality and efficiency improvements: remove dead code, simplify logic, improve naming, and ensure consistent style. Make only safe changes that preserve behaviour.'
  end
  dispatch_prompt(prompt)
  return true
end

-- ── /review ────────────────────────────────────────────────────────────────

local function review_command(args)
  local target = trim(args)
  local prompt
  if target ~= '' then
    prompt = 'Review `' .. target .. '` for correctness, clarity, and maintainability. Read-only – do not modify files.'
  else
    prompt = 'Review the code changed in this session for correctness, clarity, and maintainability. Read-only – do not modify files.'
  end
  dispatch_prompt(prompt)
  return true
end

-- ── /security-review ───────────────────────────────────────────────────────

local function security_review_command(args)
  local target = trim(args)
  local prompt
  if target ~= '' then
    prompt = 'Perform a security review of `' .. target .. '`. Look for injection, auth bypass, data exposure, and unsafe dependencies. Read-only – do not modify files.'
  else
    prompt = 'Perform a security review of the code changed in this session. Look for injection, auth bypass, data exposure, and unsafe dependencies. Read-only – do not modify files.'
  end
  dispatch_prompt(prompt)
  return true
end

-- ── /clear ─────────────────────────────────────────────────────────────────
-- Start fresh on a new task while keeping CLAUDE.md project memory.

local function clear_command()
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._clear_session_command) == 'function' then
    return copilot_slash._clear_session_command()
  end
  session.clear_and_new_session()
  return true
end

-- ── /rewind ────────────────────────────────────────────────────────────────

local function rewind_command(args)
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._rewind_checkpoint) == 'function' then
    return copilot_slash._rewind_checkpoint(args)
  end
  append_entry('system', 'Rewind is not available – no checkpoint backend found.')
  return true
end

-- ── /background ────────────────────────────────────────────────────────────
-- Detach the current session to run as a background agent.

local function background_command()
  dispatch_prompt(
    'Continue working on the current task in the background. Report back when done or if you need input.',
    { background = true }
  )
  append_entry('system', 'Session detached to background.')
  return true
end

-- ── /agents ────────────────────────────────────────────────────────────────

local function agents_command(args)
  local copilot_slash = require('copilot_agent.copilot_slash')
  if type(copilot_slash._agent_command) == 'function' then
    return copilot_slash._agent_command(args)
  end
  append_entry('system', 'Agent manager is not available.')
  return true
end

-- ── /batch ─────────────────────────────────────────────────────────────────
-- Decompose a large change into independent units and run each in its own
-- worktree via the tasks system.

local function batch_command(args)
  local prompt = trim(args)
  if prompt == '' then
    append_entry('system', 'Usage: /batch <description of the large change to decompose>')
    return true
  end
  dispatch_prompt(
    'Decompose the following task into independent sub-tasks that can each be completed in their own worktree without conflicts, then execute them in parallel:\n\n' .. prompt
  )
  return true
end

-- ── /doctor ────────────────────────────────────────────────────────────────

local function doctor_command()
  local lines = { '## Claude provider diagnostics', '' }

  -- Check ANTHROPIC_API_KEY
  local api_key = os.getenv('ANTHROPIC_API_KEY') or ''
  lines[#lines + 1] = api_key ~= ''
    and '✅ ANTHROPIC_API_KEY is set (' .. #api_key .. ' chars)'
    or  '❌ ANTHROPIC_API_KEY is not set'

  -- Check ANTHROPIC_BASE_URL
  local base_url = os.getenv('ANTHROPIC_BASE_URL') or ''
  lines[#lines + 1] = base_url ~= ''
    and '✅ ANTHROPIC_BASE_URL: ' .. base_url
    or  'ℹ️  ANTHROPIC_BASE_URL not set (will use https://api.anthropic.com)'

  -- Check CLAUDE.md
  local wd = working_directory()
  local claude_md = wd .. '/CLAUDE.md'
  lines[#lines + 1] = vim.fn.filereadable(claude_md) == 1
    and '✅ CLAUDE.md found at ' .. vim.fn.fnamemodify(claude_md, ':~:.')
    or  'ℹ️  No CLAUDE.md – run /init to generate one'

  -- Check .mcp.json
  local mcp_json = wd .. '/.mcp.json'
  lines[#lines + 1] = vim.fn.filereadable(mcp_json) == 1
    and '✅ .mcp.json found'
    or  'ℹ️  No .mcp.json (no project MCP servers configured)'

  -- Check ~/.claude directory
  local claude_dir = vim.fn.expand('~/.claude')
  lines[#lines + 1] = vim.fn.isdirectory(claude_dir) == 1
    and '✅ ~/.claude directory exists'
    or  '❌ ~/.claude directory not found – is Claude CLI installed?'

  -- Check debug mode
  local debug_env = os.getenv('COPILOT_DEBUG') or ''
  lines[#lines + 1] = (debug_env == '1' or debug_env == 'true')
    and '✅ Debug logging enabled (COPILOT_DEBUG=' .. debug_env .. ')'
    or  'ℹ️  Debug logging off (set COPILOT_DEBUG=1 to enable)'

  lines[#lines + 1] = ''
  show_markdown_result('Doctor – Claude', lines)
  return true
end

-- ── /debug ─────────────────────────────────────────────────────────────────
-- Toggle COPILOT_DEBUG at runtime for the current process.

local _debug_enabled = (os.getenv('COPILOT_DEBUG') == '1' or os.getenv('COPILOT_DEBUG') == 'true')

local function debug_command(args)
  args = trim(args):lower()
  if args == 'on' or args == '1' or args == 'true' then
    _debug_enabled = true
  elseif args == 'off' or args == '0' or args == 'false' then
    _debug_enabled = false
  else
    _debug_enabled = not _debug_enabled
  end
  -- Propagate to environment for child processes / server reads.
  vim.fn.setenv('COPILOT_DEBUG', _debug_enabled and '1' or '0')
  append_entry('system', 'Claude debug logging: ' .. (_debug_enabled and 'ON' or 'OFF'))
  return true
end

-- ── /feedback ──────────────────────────────────────────────────────────────

local function feedback_command(args)
  local description = trim(args)
  if description == '' then
    append_entry('system', 'Usage: /feedback <description of the issue>')
    return true
  end

  local lines = { '## Bug report', '' }
  lines[#lines + 1] = '**Description:** ' .. description
  lines[#lines + 1] = ''
  lines[#lines + 1] = '**Session ID:** ' .. tostring(state.session_id or 'n/a')
  lines[#lines + 1] = '**Provider:** claude'
  lines[#lines + 1] = '**Working directory:** ' .. working_directory()
  lines[#lines + 1] = '**Neovim version:** ' .. tostring(vim.version())
  lines[#lines + 1] = ''
  lines[#lines + 1] = '_Please open an issue at https://github.com/ray-x/copilot-agent.nvim with the above context._'

  show_markdown_result('Feedback', lines)
  return true
end

-- ── /btw ───────────────────────────────────────────────────────────────────
-- A quick aside that is sent as a low-weight system note rather than a full
-- user turn, so it doesn't meaningfully bloat conversation history.

local function btw_command(args)
  local note = trim(args)
  if note == '' then
    append_entry('system', 'Usage: /btw <quick aside>')
    return true
  end
  -- Prepend with a soft marker so the model treats it as background context.
  dispatch_prompt('[btw, just so you know: ' .. note .. ']', { weight = 'low' })
  return true
end

-- ── Command dispatch table ─────────────────────────────────────────────────

local handlers = {
  init               = init_command,
  memory             = memory_command,
  mcp                = mcp_command,
  permissions        = permissions_command,
  effort             = effort_command,
  plan               = plan_command,
  compact            = compact_command,
  context            = context_command,
  diff               = diff_command,
  simplify           = simplify_command,
  review             = review_command,
  ['security-review'] = security_review_command,
  clear              = clear_command,
  rewind             = rewind_command,
  background         = background_command,
  agents             = agents_command,
  batch              = batch_command,
  doctor             = doctor_command,
  debug              = debug_command,
  feedback           = feedback_command,
  btw                = btw_command,
}

--- Execute a Claude-specific slash command.
--- Returns true if the command was handled, false otherwise.
function M.execute(command, args, opts)
  local handler = handlers[command]
  if not handler then
    return false
  end
  return handler(args, opts or {}) == true
end

--- Return the list of command names handled by this module.
function M.command_names()
  local names = {}
  for k in pairs(handlers) do
    names[#names + 1] = k
  end
  table.sort(names)
  return names
end

return M
