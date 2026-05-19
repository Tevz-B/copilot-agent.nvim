-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Todo item state management and todo-related event handling.

local cfg = require('copilot_agent.config')
local logger = require('copilot_agent.log')
local utils = require('copilot_agent.utils')

local state = cfg.state
local log = logger.log
local first_non_empty = utils.first_non_empty

local M = {}

-- Internal dependencies injected via M.setup()
local sanitize_permission_text
local set_background_task
local refresh_statuslines
local refresh_reasoning_overlay
local schedule_open_buffer_refresh
local extract_shell_tool_detail
local summarize_tool_activity
local fallback_tool_activity_summary
local overlay_run_id_for_tool_call
local complete_overlay_tool
local serialize_log_value

--- Inject event-local dependencies that cannot be required directly.
--- Call this once from events.lua after all locals are defined.
---@param deps table
function M.setup(deps)
  sanitize_permission_text = deps.sanitize_permission_text
  set_background_task = deps.set_background_task
  refresh_statuslines = deps.refresh_statuslines
  refresh_reasoning_overlay = deps.refresh_reasoning_overlay
  schedule_open_buffer_refresh = deps.schedule_open_buffer_refresh
  extract_shell_tool_detail = deps.extract_shell_tool_detail
  summarize_tool_activity = deps.summarize_tool_activity
  fallback_tool_activity_summary = deps.fallback_tool_activity_summary
  overlay_run_id_for_tool_call = deps.overlay_run_id_for_tool_call
  complete_overlay_tool = deps.complete_overlay_tool
  serialize_log_value = deps.serialize_log_value
end

---------------------------------------------------------------------------
-- Core todo state helpers
---------------------------------------------------------------------------

function M.reset_todo_state()
  state.todo_items = {}
  state.todo_item_serial = 0
  state.todo_item_counter = 0
  state.todo_root_id = nil
end

local function next_todo_item_id(prefix)
  state.todo_item_counter = (tonumber(state.todo_item_counter) or 0) + 1
  return string.format('%s-%d', prefix or 'todo', state.todo_item_counter)
end

function M.ensure_todo_item(id, data)
  if type(id) ~= 'string' or id == '' then
    id = next_todo_item_id('todo')
  end
  if type(state.todo_items) ~= 'table' then
    state.todo_items = {}
  end
  local item = state.todo_items[id]
  if type(item) ~= 'table' then
    state.todo_item_serial = (tonumber(state.todo_item_serial) or 0) + 1
    item = {
      id = id,
      order = state.todo_item_serial,
      progress_messages = {},
    }
    state.todo_items[id] = item
  end
  if type(data) == 'table' then
    for key, value in pairs(data) do
      item[key] = value
    end
  end
  item.id = id
  if type(item.progress_messages) ~= 'table' then
    item.progress_messages = {}
  end
  item.order = tonumber(item.order) or state.todo_item_serial
  state.todo_item_serial = math.max(tonumber(state.todo_item_serial) or 0, tonumber(item.order) or 0)
  return item
end

function M.ensure_todo_root()
  local root_id = state.todo_root_id
  if type(state.todo_items) ~= 'table' then
    state.todo_items = {}
  end
  if type(root_id) == 'string' and root_id ~= '' and type(state.todo_items[root_id]) == 'table' then
    return root_id
  end
  root_id = 'intent'
  state.todo_root_id = root_id
  M.ensure_todo_item(root_id, {
    kind = 'intent',
    title = state.current_intent or 'Current task',
    status = 'running',
    order = 1,
  })
  return root_id
end

local function append_todo_progress(item, message)
  message = sanitize_permission_text(message)
  if type(item) ~= 'table' or not message then
    return
  end
  item.progress_messages = item.progress_messages or {}
  if #item.progress_messages == 0 or item.progress_messages[#item.progress_messages] ~= message then
    item.progress_messages[#item.progress_messages + 1] = message
  end
end

function M.finish_running_todos(status)
  if type(state.todo_items) ~= 'table' then
    return
  end
  for _, item in pairs(state.todo_items) do
    if type(item) == 'table' and (item.status == 'running' or item.status == 'pending') then
      item.status = status
    end
  end
end

---------------------------------------------------------------------------
-- Event handler
---------------------------------------------------------------------------

--- Handle todo-related session events.
---@param event_type string
---@param data table
---@return boolean true if the event was handled
function M.handle_todo_event(event_type, data)
  if event_type == 'assistant.intent' then
    state.current_intent = sanitize_permission_text(data.intent)
    local todo = M.ensure_todo_item(M.ensure_todo_root(), {
      kind = 'intent',
      title = state.current_intent or 'Current task',
      status = 'running',
      order = 1,
    })
    todo.kind = 'intent'
    todo.title = state.current_intent or todo.title or 'Current task'
    todo.status = 'running'
    refresh_statuslines()
    refresh_reasoning_overlay()
    return true
  end

  if event_type == 'subagent.started' then
    set_background_task('subagent:' .. (data.toolCallId or ''), {
      kind = 'subagent',
      status = 'running',
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      description = data.agentDescription,
    })
    local todo = M.ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = M.ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      description = data.agentDescription,
      status = 'running',
    })
    todo.parent_id = M.ensure_todo_root()
    todo.title = first_non_empty(data.agentDisplayName, data.agentName, todo.title, 'Subagent')
    todo.description = data.agentDescription
    return true
  end

  if event_type == 'subagent.completed' then
    set_background_task('subagent:' .. (data.toolCallId or ''), {
      kind = 'subagent',
      status = 'completed',
    })
    local todo = M.ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = M.ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      status = 'done',
    })
    todo.parent_id = M.ensure_todo_root()
    todo.status = 'done'
    schedule_open_buffer_refresh()
    return true
  end

  if event_type == 'subagent.failed' then
    set_background_task('subagent:' .. (data.toolCallId or ''), {
      kind = 'subagent',
      status = 'failed',
    })
    local todo = M.ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = M.ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      status = 'blocked',
    })
    todo.parent_id = M.ensure_todo_root()
    todo.status = 'blocked'
    append_todo_progress(todo, type(data.error) == 'table' and data.error.message or data.message)
    schedule_open_buffer_refresh()
    return true
  end

  if event_type == 'tool.execution_start' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    local detail = extract_shell_tool_detail(data.toolName, data) or state.pending_tool_detail
    local todo = M.ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = M.ensure_todo_root(),
      title = summarize_tool_activity(data.toolName, data) or fallback_tool_activity_summary(data.toolName, detail),
      tool_name = sanitize_permission_text(data.toolName),
      tool_call_id = tool_call_id,
      tool_detail = detail,
      status = 'running',
    })
    todo.parent_id = M.ensure_todo_root()
    todo.kind = 'tool'
    todo.tool_name = sanitize_permission_text(data.toolName)
    todo.tool_call_id = tool_call_id or todo.tool_call_id
    todo.tool_detail = detail or todo.tool_detail
    todo.title = summarize_tool_activity(data.toolName, data) or todo.title
    todo.status = 'running'
    state.pending_tool_detail = nil
    return true
  end

  if event_type == 'tool.execution_progress' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    local todo = M.ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = M.ensure_todo_root(),
      title = summarize_tool_activity(state.active_tool, data) or fallback_tool_activity_summary(state.active_tool, state.active_tool_detail),
      tool_name = sanitize_permission_text(state.active_tool or data.toolName),
      tool_call_id = tool_call_id,
      status = 'running',
    })
    append_todo_progress(todo, data.progressMessage)
    return true
  end

  if event_type == 'tool.execution_complete' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    local completion_run_id = overlay_run_id_for_tool_call(tool_call_id)
    if not completion_run_id and not tool_call_id then
      completion_run_id = state.active_tool_run_id
    end

    log(string.format('tool.execution_complete tool=%s payload=%s', tostring(state.active_tool or data.toolName or '<none>'), serialize_log_value(data, { max_len = 1600 })), vim.log.levels.DEBUG)
    complete_overlay_tool(completion_run_id)
    local todo = M.ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = M.ensure_todo_root(),
      title = summarize_tool_activity(state.active_tool or data.toolName, data) or fallback_tool_activity_summary(state.active_tool or data.toolName, state.active_tool_detail),
      tool_name = sanitize_permission_text(state.active_tool or data.toolName),
      tool_call_id = tool_call_id,
      status = data.success == true and 'done' or 'blocked',
    })
    todo.parent_id = M.ensure_todo_root()
    todo.status = data.success == true and 'done' or 'blocked'
    if type(data.error) == 'table' and type(data.error.message) == 'string' then
      append_todo_progress(todo, data.error.message)
    end

    local clear_active_tool_state = completion_run_id ~= nil and state.active_tool_run_id == completion_run_id
    if not clear_active_tool_state and not tool_call_id then
      clear_active_tool_state = true
    end
    if clear_active_tool_state then
      state.active_tool = nil
      state.active_tool_run_id = nil
      state.active_tool_detail = nil
      state.pending_tool_detail = nil
    end
    refresh_statuslines()
    refresh_reasoning_overlay()
    return true
  end

  return false
end

return M
