-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Prompt/permission/user-input functions extracted from events.lua.
-- All module dependencies are accessed lazily (via require at call time) so that
-- test harnesses that reload package.loaded entries continue to work.

local M = {}

-- Module-level state.
local active_prompt_id = nil
local queued_prompt_requests = {}
local queued_prompt_request_ids = {}
local PROMPT_HANDOFF_DELAY_MS = 20

-- These helpers must be injected from events.lua since they are defined there.
local _extract_shell_command_text
local _sanitize_permission_text
local _show_diff_float

function M.set_helpers(helpers)
  _extract_shell_command_text = helpers.extract_shell_command_text
  _sanitize_permission_text = helpers.sanitize_permission_text
  _show_diff_float = helpers.show_diff_float
  -- Reset queue state so that reloading events.lua (e.g. in tests) starts fresh.
  active_prompt_id = nil
  queued_prompt_requests = {}
  queued_prompt_request_ids = {}
end

local function build_permission_prompt(permission)
  permission = permission or {}
  local kind = permission.kind or 'unknown'
  local parts = {}

  if kind == 'shell' then
    local cmd = _extract_shell_command_text(permission) or '(shell command)'
    parts[#parts + 1] = 'Run shell command'
    parts[#parts + 1] = cmd
  elseif kind == 'write' then
    parts[#parts + 1] = 'Write file'
    parts[#parts + 1] = _sanitize_permission_text(permission.fileName or permission.path) or '(unknown file)'
  elseif kind == 'read' then
    parts[#parts + 1] = 'Read'
    parts[#parts + 1] = _sanitize_permission_text(permission.path or permission.fileName) or '(unknown path)'
  elseif kind == 'mcp' or kind == 'custom-tool' then
    local tool = _sanitize_permission_text(permission.toolTitle or permission.toolName) or 'unknown tool'
    local server = _sanitize_permission_text(permission.serverName) or ''
    parts[#parts + 1] = tool
    if server ~= '' then
      parts[#parts + 1] = '(' .. server .. ')'
    end
    local description = _sanitize_permission_text(permission.toolDescription)
    if description then
      parts[#parts + 1] = '— ' .. description
    end
  elseif kind == 'url' then
    parts[#parts + 1] = 'Fetch URL'
    parts[#parts + 1] = _sanitize_permission_text(permission.url) or '(unknown URL)'
  elseif kind == 'memory' then
    parts[#parts + 1] = 'Memory ' .. tostring(permission.action or 'access')
    local fact = _sanitize_permission_text(permission.fact)
    if fact then
      parts[#parts + 1] = fact
    end
  elseif kind == 'hook' then
    parts[#parts + 1] = 'Hook'
    local hook_message = _sanitize_permission_text(permission.hookMessage)
    if hook_message then
      parts[#parts + 1] = hook_message
    end
  else
    parts[#parts + 1] = _sanitize_permission_text(permission.toolTitle or permission.toolName or kind) or kind
  end

  local intention = _sanitize_permission_text(permission.intention)
  if intention and kind ~= 'shell' and kind ~= 'read' and kind ~= 'write' then
    parts[#parts + 1] = '— ' .. intention
  end

  return 'Allow: ' .. table.concat(parts, ' ')
end

local function prompt_request_id(payload)
  local req = payload and payload.data and payload.data.request or {}
  local req_id = req and req.id or nil
  if type(req_id) ~= 'string' or req_id == '' then
    return nil
  end
  return req_id
end

function M.reset_prompt_state()
  active_prompt_id = nil
  queued_prompt_requests = {}
  queued_prompt_request_ids = {}
end

local function enqueue_prompt(kind, payload)
  local req_id = prompt_request_id(payload)
  if not req_id then
    return false
  end
  if active_prompt_id == req_id or queued_prompt_request_ids[req_id] then
    return false
  end
  queued_prompt_request_ids[req_id] = true
  queued_prompt_requests[#queued_prompt_requests + 1] = {
    kind = kind,
    payload = payload,
  }
  return true
end

local function pop_prompt()
  while #queued_prompt_requests > 0 do
    local entry = table.remove(queued_prompt_requests, 1)
    local req_id = prompt_request_id(entry.payload)
    if req_id then
      queued_prompt_request_ids[req_id] = nil
      return entry
    end
  end
  return nil
end

local function finish_prompt(req_id)
  if active_prompt_id == req_id then
    active_prompt_id = nil
  end
end

local function defer_prompt(callback)
  vim.defer_fn(function()
    callback()
  end, PROMPT_HANDOFF_DELAY_MS)
end

function M.answer_permission(session_id, request_id, approved, callback)
  require('copilot_agent.http').request('POST', '/sessions/' .. session_id .. '/permission/' .. request_id, { approved = approved }, callback or function(_, err)
    if err then
      require('copilot_agent.config').require('copilot_agent.config').notify('Failed to send permission answer: ' .. tostring(err), vim.log.levels.WARN)
    end
  end)
end

function M.sync_model_state(model, reasoning_effort, session_id)
  if type(model) == 'string' and model ~= '' then
    require('copilot_agent.config').state.current_model = model
    local active_session_id = type(session_id) == 'string' and session_id ~= '' and session_id or require('copilot_agent.config').state.session_id
    if type(active_session_id) == 'string' and active_session_id ~= '' then
      local _cfg = require('copilot_agent.config')
      local _session_model_key = _cfg.session_model_key or function(sid) return (type(sid) == 'string' and sid ~= '') and sid or nil end
      local _provider_for_session = _cfg.session_provider or function() end
      local key = _session_model_key(active_session_id, _provider_for_session(active_session_id))
      if key then
        require('copilot_agent.config').state.session_models[key] = model
        require('copilot_agent.session_models').set(key, model)
      else
        require('copilot_agent.config').state.session_models[active_session_id] = model
        require('copilot_agent.session_models').set(active_session_id, model)
      end
    end
  elseif model == '' or model == nil then
    require('copilot_agent.config').state.current_model = nil
  end

  if type(reasoning_effort) == 'string' and reasoning_effort ~= '' then
    require('copilot_agent.config').state.reasoning_effort = reasoning_effort
  elseif reasoning_effort == '' or reasoning_effort == nil then
    require('copilot_agent.config').state.reasoning_effort = nil
  end
end

function M.sync_config_counts(data)
  require('copilot_agent.config').state.instruction_count = tonumber(data.instructionCount) or 0
  require('copilot_agent.config').state.agent_count = tonumber(data.agentCount) or 0
  require('copilot_agent.config').state.skill_count = tonumber(data.skillCount) or 0
  require('copilot_agent.config').state.mcp_count = tonumber(data.mcpCount) or 0
end

function M.refresh_session_name_from_server(session_id)
  if type(session_id) ~= 'string' or session_id == '' then
    return
  end

  require('copilot_agent.http').request('GET', '/sessions/' .. session_id, nil, function(response, err)
    if err or type(response) ~= 'table' then
      return
    end
    if require('copilot_agent.config').state.session_id ~= session_id then
      return
    end
    if require('copilot_agent.session_names').get(session_id) then
      return
    end

    local summary = type(response.summary) == 'string' and vim.trim(response.summary) or ''
    if summary ~= '' and summary ~= require('copilot_agent.config').state.session_name then
      require('copilot_agent.config').state.session_name = summary
      require('copilot_agent.statusline').refresh_statuslines()
    end
  end)
end

local show_next_prompt

local function present_user_input_picker(payload)
  local request_payload = payload and payload.data and payload.data.request or nil
  if type(request_payload) ~= 'table' or type(request_payload.id) ~= 'string' then
    return
  end

  local req_id = request_payload.id
  local session_id = payload.data.sessionId or require('copilot_agent.config').state.session_id
  if type(session_id) ~= 'string' or session_id == '' then
    finish_prompt(req_id)
    defer_prompt(show_next_prompt)
    return
  end

  active_prompt_id = req_id
  local choices = type(request_payload.choices) == 'table' and request_payload.choices or {}
  local allow_freeform = request_payload.allowFreeform ~= false

  local function complete()
    finish_prompt(req_id)
    defer_prompt(show_next_prompt)
  end

  local function answer(value, was_freeform)
    if value == nil or value == '' then
      return
    end
    require('copilot_agent.config').state.pending_user_input = nil
    require('copilot_agent.render').append_entry('user', value)
    require('copilot_agent.http').request('POST', string.format('/sessions/%s/user-input/%s', session_id, request_payload.id), {
      answer = value,
      wasFreeform = was_freeform,
    }, function(_, err)
      if err then
        require('copilot_agent.render').append_entry('error', 'Failed to answer user input: ' .. err)
      end
    end)
    complete()
  end

  local function ask_freeform()
    vim.ui.input({ prompt = request_payload.question .. ' ' }, function(input)
      if input == nil or input == '' then
        require('copilot_agent.config').require('copilot_agent.config').notify('Input dismissed — use :CopilotAgentRetryInput to try again', vim.log.levels.WARN)
        complete()
        return
      end
      answer(input, true)
    end)
  end

  if #choices > 0 then
    local items = vim.deepcopy(choices)
    if allow_freeform then
      table.insert(items, 'Custom...')
    end
    vim.ui.select(items, { prompt = request_payload.question }, function(choice)
      if choice == nil then
        require('copilot_agent.config').require('copilot_agent.config').notify('Selection dismissed — use :CopilotAgentRetryInput to try again', vim.log.levels.WARN)
        complete()
        return
      end
      if choice == 'Custom...' then
        ask_freeform()
        return
      end
      answer(choice, false)
    end)
    return
  end

  if allow_freeform then
    ask_freeform()
    return
  end

  complete()
end

function M.show_user_input_picker(payload)
  if not enqueue_prompt('user_input', payload) then
    return
  end

  vim.schedule(show_next_prompt)
end

function M.handle_user_input(payload)
  local request_payload = payload and payload.data and payload.data.request or nil
  if type(request_payload) ~= 'table' or type(request_payload.id) ~= 'string' then
    return
  end

  -- Store for retry if dismissed.
  require('copilot_agent.config').state.pending_user_input = payload

  require('copilot_agent.render').append_entry('system', 'Input requested: ' .. request_payload.question)
  M.show_user_input_picker(payload)
end

function M.present_permission_picker(payload)
  local data = payload and payload.data or {}
  local req = data.request or {}
  local req_id = req.id
  local sid = require('copilot_agent.config').state.session_id
  local event_session_id = data.sessionId
  if not sid or sid == '' then
    finish_prompt(req_id)
    defer_prompt(show_next_prompt)
    return
  end
  if type(event_session_id) == 'string' and sid ~= event_session_id then
    finish_prompt(req_id)
    defer_prompt(show_next_prompt)
    return
  end

  local perm = req.request or {}
  local kind = perm.kind or 'unknown'
  local prompt_str = build_permission_prompt(perm)

  -- Build choices: Allow, Deny, Allow all for session.
  -- For read/write with a file path, also offer "Allow this directory".
  local choices = { 'Allow', 'Deny', 'Allow all for this session' }
  local dir_path = nil
  local tool_label = require('copilot_agent.approvals').tool_label(perm)
  local allow_tool_choice = nil
  local has_diff = kind == 'write' and perm.diff and perm.diff ~= ''
  if kind == 'read' or kind == 'write' then
    local file = perm.path or perm.fileName
    if file and file ~= '' then
      dir_path = vim.fn.fnamemodify(file, ':h')
      if dir_path and dir_path ~= '' and dir_path ~= '.' then
        table.insert(choices, 3, 'Allow this directory (' .. vim.fn.fnamemodify(dir_path, ':~') .. ')')
      end
    end
  elseif tool_label then
    allow_tool_choice = 'Allow ' .. tool_label .. ' for the rest of this session'
    table.insert(choices, 2, allow_tool_choice)
  end
  if has_diff then
    table.insert(choices, 2, 'Show diff')
  end

  active_prompt_id = req_id

  -- Permission picker (extracted so "Show diff" can re-invoke it).
  local function show_permission_picker()
    vim.ui.select(choices, { prompt = prompt_str }, function(choice)
      if not require('copilot_agent.config').state.session_id or require('copilot_agent.config').state.session_id ~= sid then
        finish_prompt(req_id)
        defer_prompt(show_next_prompt)
        return
      end
      if not choice then
        finish_prompt(req_id)
        defer_prompt(show_next_prompt)
        return
      end

      if choice == 'Show diff' then
        _show_diff_float(perm.diff, function()
          vim.schedule(show_permission_picker)
        end)
        return
      end

      if choice == 'Allow all for this session' then
        -- Approve this request, then switch to approve-all mode.
        M.answer_permission(sid, req_id, true)
        require('copilot_agent.config').state.permission_mode = 'approve-all'
        require('copilot_agent.http').request('POST', '/sessions/' .. sid .. '/permission-mode', { mode = 'approve-all' }, function(_, err)
          if err then
            require('copilot_agent.config').require('copilot_agent.config').notify('Failed to set permission mode: ' .. tostring(err), vim.log.levels.WARN)
          else
            require('copilot_agent.config').require('copilot_agent.config').notify('Permission mode set to approve-all for this session', vim.log.levels.INFO)
            require('copilot_agent.statusline').refresh_statuslines()
          end
        end)
      elseif allow_tool_choice and choice == allow_tool_choice then
        local ok, allow_err = require('copilot_agent.approvals').allow_tool(perm)
        if not ok then
          require('copilot_agent.config').require('copilot_agent.config').notify('Failed to allow tool: ' .. tostring(allow_err), vim.log.levels.WARN)
          return
        end
        M.answer_permission(sid, req_id, true)
        require('copilot_agent.config').require('copilot_agent.config').notify('Allowed ' .. tool_label .. ' for this session', vim.log.levels.INFO)
      elseif choice:match('^Allow this directory') then
        M.answer_permission(sid, req_id, true)
        if dir_path then
          local normalized = require('copilot_agent.approvals').add_directory(dir_path)
          if normalized then
            require('copilot_agent.config').require('copilot_agent.config').notify('Added directory: ' .. vim.fn.fnamemodify(normalized, ':~'), vim.log.levels.INFO)
          end
        end
      else
        local approved = (choice == 'Allow')
        M.answer_permission(sid, req_id, approved)
      end

      finish_prompt(req_id)
      defer_prompt(show_next_prompt)
    end)
  end

  vim.schedule(show_permission_picker)
end

show_next_prompt = function()
  if active_prompt_id then
    return
  end

  local next_prompt = pop_prompt()
  if not next_prompt then
    return
  end

  if next_prompt.kind == 'permission' then
    M.present_permission_picker(next_prompt.payload)
  elseif next_prompt.kind == 'user_input' then
    present_user_input_picker(next_prompt.payload)
  end
end

M.enqueue_prompt = enqueue_prompt
M.show_next_prompt = show_next_prompt

return M
