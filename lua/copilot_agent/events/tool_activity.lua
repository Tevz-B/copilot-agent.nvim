-- Tool activity/overlay functions extracted from events.lua
-- Manages tool execution lifecycle tracking and overlay display state.

local utils = require('copilot_agent.utils')
local cfg = require('copilot_agent.config')
local logger = require('copilot_agent.log')
local service = require('copilot_agent.service')
local sl = require('copilot_agent.statusline')
local render = require('copilot_agent.render')
local apply_patch = require('copilot_agent.apply_patch')

local state = cfg.state
local log = logger.log
local should_log = logger.should_log
local serialize_log_value = logger.serialize_log_value
local resolve_log_level = logger.resolve_log_level
local TRACE_LOG_LEVEL = vim.log.levels.TRACE or vim.log.levels.DEBUG

local refresh_statuslines = sl.refresh_statuslines
local refresh_reasoning_overlay = render.refresh_reasoning_overlay

local working_directory = service.working_directory
local tilde_home_path = utils.tilde_home_path
local first_non_empty = utils.first_non_empty
local append_unique = utils.append_unique
local summarize_file_group = utils.summarize_file_group
local append_unique_activity_output = utils.append_unique_activity_output

local RECENT_ACTIVITY_LINE_MAX_CHARS = 120

local M = {}

-- Forward reference for complete_overlay_tool (set via set_complete_overlay_tool)
local complete_overlay_tool = function(_run_id) end

function M.set_complete_overlay_tool(fn)
  complete_overlay_tool = fn
end

local function sanitize_permission_text(text)
  if type(text) ~= 'string' then
    return nil
  end
  text = text:gsub('[\r\n]+', ' '):gsub('\t', ' ')
  text = vim.trim(text)
  if text == '' then
    return nil
  end
  return tilde_home_path(text)
end
M.sanitize_permission_text = sanitize_permission_text

local function log_debug_trace(message, payload, opts)
  opts = opts or {}
  local level = resolve_log_level(opts.level) or TRACE_LOG_LEVEL
  if not should_log(level) then
    return
  end
  if type(payload) == 'function' then
    payload = payload()
  end
  local suffix = payload == nil and '' or ' ' .. serialize_log_value(payload, opts)
  log(message .. suffix, level)
end

local function looks_like_shell_tool(name)
  if type(name) ~= 'string' or name == '' then
    return false
  end
  name = vim.trim(name):lower()
  return name == 'bash' or name == 'sh' or name == 'zsh' or name == 'fish' or name == 'pwsh' or name == 'powershell' or name == 'cmd'
end

local function overlay_now_ms()
  local hrtime = (vim.uv or vim.loop).hrtime
  return math.floor(hrtime() / 1e6)
end

local function cancel_overlay_tool_schedule()
  state.overlay_tool_schedule_token = (tonumber(state.overlay_tool_schedule_token) or 0) + 1
end

local function min_overlay_tool_duration_ms()
  return 3000
end

local function overlay_run_id_for_tool_call(tool_call_id)
  local current = state.overlay_tool_display
  if not current then
    return nil
  end

  if type(tool_call_id) == 'string' and tool_call_id ~= '' then
    if current.tool_call_id == tool_call_id then
      return current.run_id
    end
    return nil
  end

  if type(state.active_tool_run_id) == 'number' and current.run_id == state.active_tool_run_id then
    return state.active_tool_run_id
  end

  return nil
end

local function overlay_run_id_for_tool_identity(tool_call_id, tool_name)
  local run_id = overlay_run_id_for_tool_call(tool_call_id)
  if run_id then
    return run_id
  end

  local current = state.overlay_tool_display
  if not current then
    return nil
  end

  local current_tool = sanitize_permission_text(current.tool)
  local desired_tool = sanitize_permission_text(tool_name)
  if current_tool and desired_tool and current_tool:lower() == desired_tool:lower() then
    return current.run_id
  end
  if not desired_tool and not tool_call_id then
    return current.run_id
  end
  return nil
end

local function begin_overlay_tool_post_result(run_id, hook_invocation_id)
  if type(run_id) ~= 'number' then
    return
  end

  local current = state.overlay_tool_display
  if not current or current.run_id ~= run_id then
    return
  end

  cancel_overlay_tool_schedule()
  current.last_activity_ms = overlay_now_ms()
  current.post_tool_use_pending = true
  current.post_tool_use_hook_invocation_id = sanitize_permission_text(hook_invocation_id)
end

local function set_overlay_tool_result_text(run_id, result_text)
  if type(run_id) ~= 'number' then
    return
  end

  local current = state.overlay_tool_display
  if not current or current.run_id ~= run_id then
    return
  end

  current.last_activity_ms = overlay_now_ms()
  current.post_tool_use_pending = nil
  current.post_tool_use_hook_invocation_id = nil
  if type(result_text) == 'string' then
    result_text = result_text:gsub('\r\n?', '\n')
    if result_text == '' then
      result_text = nil
    end
  else
    result_text = nil
  end
  current.result_text = result_text
  current.result_updated_ms = current.result_text and current.last_activity_ms or nil
  refresh_reasoning_overlay(true)
end

local function touch_overlay_tool_activity(run_id)
  if type(run_id) ~= 'number' then
    return
  end

  local current = state.overlay_tool_display
  if not current or current.run_id ~= run_id or current.completed == true then
    return
  end

  current.last_activity_ms = overlay_now_ms()
end

local function begin_overlay_tool_display(tool_call_id, tool_name, detail)
  local current = state.overlay_tool_display
  local now = overlay_now_ms()
  local run_id = state.active_tool_run_id
  if type(run_id) ~= 'number' then
    state.overlay_tool_serial = (tonumber(state.overlay_tool_serial) or 0) + 1
    run_id = state.overlay_tool_serial
    state.active_tool_run_id = run_id
  end

  if type(current) ~= 'table' or current.run_id ~= run_id then
    current = {
      run_id = run_id,
      display_started_ms = now,
      last_activity_ms = now,
    }
    state.overlay_tool_display = current
  end

  current.tool_call_id = sanitize_permission_text(tool_call_id) or current.tool_call_id
  current.tool = sanitize_permission_text(tool_name) or current.tool
  current.detail = sanitize_permission_text(detail) or current.detail
  current.completed = false
  current.post_tool_use_pending = nil
  current.post_tool_use_hook_invocation_id = nil
  current.result_text = nil
  current.result_updated_ms = nil
  current.last_activity_ms = now

  state.active_tool = current.tool
  state.active_tool_detail = current.detail
  refresh_statuslines()
  refresh_reasoning_overlay(true)
  return current
end

local function extract_shell_command_text(data)
  data = type(data) == 'table' and data or {}
  local function assemble_command_with_args(command_value, arg_values)
    local parts = {}
    local command = sanitize_permission_text(command_value)
    if command then
      parts[#parts + 1] = command
    end

    if type(arg_values) == 'table' then
      for _, value in ipairs(arg_values) do
        local arg = sanitize_permission_text(value)
        if arg then
          parts[#parts + 1] = arg
        end
      end
    elseif type(arg_values) == 'string' then
      local args = sanitize_permission_text(arg_values)
      if args then
        parts[#parts + 1] = args
      end
    end

    if #parts == 0 then
      return nil
    end
    return sanitize_permission_text(table.concat(parts, ' '))
  end

  local visited = {}
  local nested_keys = {
    'input',
    'toolInput',
    'tool_input',
    'parameters',
    'params',
    'payload',
    'request',
    'call',
    'invocation',
    'details',
    'metadata',
  }

  local function extract_from_table(value, depth)
    if type(value) ~= 'table' or depth > 3 or visited[value] then
      return nil
    end
    visited[value] = true

    local command_with_args =
      assemble_command_with_args(value.command or value.executable or value.program or value.cmd, value.arguments or value.args or value.argv or value.commandArgs or value.command_args)
    if command_with_args then
      return command_with_args
    end

    local detail = sanitize_permission_text(
      value.fullCommandText
        or value.commandLine
        or value.commandText
        or value.shellCommand
        or value.rawCommand
        or value.raw_command
        or value.invocation
        or value.command
        or value.toolDescription
        or value.description
        or value.intention
    )
    if detail then
      return detail
    end

    for _, key in ipairs(nested_keys) do
      local nested = extract_from_table(value[key], depth + 1)
      if nested then
        return nested
      end
    end

    for _, nested in pairs(value) do
      local detail_from_nested = extract_from_table(nested, depth + 1)
      if detail_from_nested then
        return detail_from_nested
      end
    end

    return nil
  end

  return extract_from_table(data, 1)
end

local function extract_shell_tool_detail(tool_name, data)
  local detail = extract_shell_command_text(data)
  if detail then
    return detail
  end

  if looks_like_shell_tool(tool_name) then
    return state.pending_tool_detail
  end
  return nil
end

local activity_nested_keys = {
  'input',
  'toolInput',
  'tool_input',
  'parameters',
  'params',
  'payload',
  'request',
  'call',
  'invocation',
  'details',
  'metadata',
  'options',
}

local function find_activity_value(value, depth, visited, extractor)
  if type(value) ~= 'table' or depth > 4 or visited[value] then
    return nil
  end
  visited[value] = true

  local direct = extractor(value)
  if direct ~= nil then
    visited[value] = nil
    return direct
  end

  for _, key in ipairs(activity_nested_keys) do
    local nested = find_activity_value(value[key], depth + 1, visited, extractor)
    if nested ~= nil then
      visited[value] = nil
      return nested
    end
  end

  for _, nested_value in pairs(value) do
    local nested = find_activity_value(nested_value, depth + 1, visited, extractor)
    if nested ~= nil then
      visited[value] = nil
      return nested
    end
  end

  visited[value] = nil
  return nil
end

local function find_activity_string(data, keys)
  return find_activity_value(data, 1, {}, function(tbl)
    for _, key in ipairs(keys) do
      local value = sanitize_permission_text(tbl[key])
      if value then
        return value
      end
    end
    return nil
  end)
end

local function find_activity_raw_string(data, predicate)
  return find_activity_value(data, 1, {}, function(tbl)
    for _, value in pairs(tbl) do
      if type(value) == 'string' and predicate(value) then
        return value
      end
    end
    return nil
  end)
end

local function normalize_activity_path(path)
  path = sanitize_permission_text(path)
  if not path then
    return nil
  end

  local normalized = path:gsub('\\', '/')

  local wd = working_directory()
  if type(wd) == 'string' and wd ~= '' then
    local root = vim.fn.fnamemodify(wd, ':p'):gsub('\\', '/')
    local prefix = root:sub(-1) == '/' and root or (root .. '/')
    if vim.startswith(normalized, prefix) then
      return normalized:sub(#prefix + 1)
    end

    local root_name = vim.fn.fnamemodify(root:gsub('/+$', ''), ':t')
    if type(root_name) == 'string' and root_name ~= '' and root_name ~= '/' then
      local marker = '/' .. root_name .. '/'
      local match_index
      local search_from = 1
      while true do
        local found = normalized:find(marker, search_from, true)
        if not found then
          break
        end
        match_index = found
        search_from = found + 1
      end
      if match_index then
        return normalized:sub(match_index + #marker)
      end
    end
  end

  return normalized
end

local function normalize_apply_patch_changes(changes)
  local normalized = {}
  if type(changes) ~= 'table' then
    return normalized
  end
  for _, change in ipairs(changes) do
    if type(change) == 'table' then
      local path = change.path
      if type(path) == 'string' then
        path = normalize_activity_path(path) or path
      end
      normalized[#normalized + 1] = {
        verb = change.verb,
        path = path,
        additions = change.additions,
        deletions = change.deletions,
      }
    end
  end
  return normalized
end

local function is_file_change_activity_item(item)
  if type(item) ~= 'table' then
    return false
  end
  if item.kind == 'code_change' then
    return true
  end
  if type(item.code_change) == 'table' and type(item.code_change.files) == 'table' and #item.code_change.files > 0 then
    return true
  end
  return sanitize_permission_text(item.tool_name) == 'apply_patch'
end

local function remember_recent_activity_line(text)
  text = sanitize_permission_text(text)
  if not text then
    return
  end
  if #text > RECENT_ACTIVITY_LINE_MAX_CHARS then
    text = text:sub(1, RECENT_ACTIVITY_LINE_MAX_CHARS - 1) .. '…'
  end
  if type(state.recent_activity_lines) ~= 'table' then
    state.recent_activity_lines = {}
  end
  for _, existing in ipairs(state.recent_activity_lines) do
    if existing == text then
      return
    end
  end
  state.recent_activity_lines[#state.recent_activity_lines + 1] = text
end

local function ensure_recent_activity_items()
  if type(state.recent_activity_items) ~= 'table' then
    state.recent_activity_items = {}
  end
  if type(state.recent_activity_tool_calls) ~= 'table' then
    state.recent_activity_tool_calls = {}
  end
  return state.recent_activity_items, state.recent_activity_tool_calls
end

local function remember_recent_activity_item(item)
  if type(item) ~= 'table' then
    return nil, nil
  end
  local items, tool_calls = ensure_recent_activity_items()
  items[#items + 1] = item
  local idx = #items
  local tool_call_id = type(item.tool_call_id) == 'string' and item.tool_call_id ~= '' and item.tool_call_id or nil
  if tool_call_id then
    tool_calls[tool_call_id] = idx
  end
  return item, idx
end

local function find_recent_tool_activity_item(tool_call_id, tool_name)
  local items = type(state.recent_activity_items) == 'table' and state.recent_activity_items or {}
  local tool_calls = type(state.recent_activity_tool_calls) == 'table' and state.recent_activity_tool_calls or {}
  local idx = type(tool_call_id) == 'string' and tool_call_id ~= '' and tool_calls[tool_call_id] or nil
  if type(idx) == 'number' and type(items[idx]) == 'table' then
    return items[idx], idx
  end
  local normalized_tool_name = sanitize_permission_text(tool_name)
  for i = #items, 1, -1 do
    local item = items[i]
    if type(item) == 'table' and item.kind == 'tool' and (normalized_tool_name == nil or item.tool_name == normalized_tool_name) then
      return item, i
    end
  end
  return nil, nil
end

local function normalize_activity_output_text(text)
  if type(text) ~= 'string' then
    return nil
  end
  text = text:gsub('\r\n?', '\n')
  if text == '' then
    return nil
  end
  return text
end

-- Initialize activity_output sub-module
local activity_output = require('copilot_agent.activity_output')({
  utils = utils,
  sanitize_permission_text = sanitize_permission_text,
  normalize_activity_output_text = normalize_activity_output_text,
  find_activity_string = find_activity_string,
  find_activity_raw_string = find_activity_raw_string,
  find_activity_value = find_activity_value,
  normalize_activity_path = normalize_activity_path,
  summarize_file_group = summarize_file_group,
  append_unique = append_unique,
  apply_patch = apply_patch,
  working_directory = working_directory,
  state = state,
  first_non_empty = first_non_empty,
  looks_like_shell_tool = looks_like_shell_tool,
})

local extract_tool_result_contents_text = activity_output.extract_tool_result_contents_text
local extract_activity_output_value_text = activity_output.extract_activity_output_value_text
local summarize_tool_activity = activity_output.summarize_tool_activity

local function fallback_tool_activity_summary(tool_name, detail)
  tool_name = sanitize_permission_text(tool_name) or tostring(tool_name or '<tool>')
  if type(detail) == 'string' and detail ~= '' then
    return 'Used ' .. tool_name .. ' — ' .. detail
  end
  return 'Used ' .. tool_name
end

local function extract_tool_execution_output_text(data)
  data = type(data) == 'table' and data or {}
  local result = type(data.result) == 'table' and data.result or nil
  local parts = {}

  if result then
    append_unique_activity_output(parts, result.detailedContent)
    if #parts == 0 then
      append_unique_activity_output(parts, extract_tool_result_contents_text(result.contents))
    end
    if #parts == 0 then
      append_unique_activity_output(parts, result.content)
    end
  end

  if #parts == 0 and type(data.error) == 'table' then
    append_unique_activity_output(parts, data.error.message)
  end

  if #parts == 0 then
    return nil
  end
  return table.concat(parts, '\n\n')
end

local function capture_tool_execution_complete(data)
  data = type(data) == 'table' and data or {}
  local tool_call_id = sanitize_permission_text(data.toolCallId or data.tool_call_id)
  local run_id = overlay_run_id_for_tool_call(tool_call_id)
  local item = find_recent_tool_activity_item(tool_call_id, data.toolName)
  local out = extract_tool_execution_output_text(data) or (item and item.post_tool_use_raw_result_text) or nil
  out = normalize_activity_output_text(out)
  if item then
    item.output_text = out
    item.complete_data = data
    item.success = data.success == true
    item.progress_messages = item.progress_messages or {}
    -- Preserve any tool telemetry provided by the host (normalize camelCase/underscore)
    item.tool_telemetry = type(data.toolTelemetry) == 'table' and data.toolTelemetry or (type(data.tool_telemetry) == 'table' and data.tool_telemetry or nil)
  end
  if run_id then
    set_overlay_tool_result_text(run_id, out)
    complete_overlay_tool(run_id)
  end

  -- Extract code_change for edit/apply_patch tools (needed when postToolUse
  -- hooks are not emitted, e.g. Claude provider).
  if item then
    local normalized_tool = sanitize_permission_text((item.tool_name) or data.toolName)
    if normalized_tool == 'apply_patch' or normalized_tool == 'edit' then
      if not item.code_change or not item.code_change.files or #item.code_change.files == 0 then
        local patch_source = item.start_input or item.start_data or data
        local output_source = item.output_text or item.post_tool_use_raw_result_text
        local changes = normalize_apply_patch_changes(apply_patch.extract_patch_changes(patch_source))
        if type(changes) == 'table' and #changes > 0 then
          item.code_change = item.code_change or {}
          item.code_change.source = item.code_change.source or normalized_tool
          item.code_change.files = item.code_change.files or changes
          item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_patch_text(patch_source)
        else
          local unified_src = output_source or patch_source
          local unified_changes = apply_patch.extract_unified_patch_changes(unified_src)
          if (not unified_changes or #unified_changes == 0) and unified_src ~= patch_source then
            unified_changes = apply_patch.extract_unified_patch_changes(patch_source)
          end
          if type(unified_changes) == 'table' and #unified_changes > 0 then
            item.code_change = item.code_change or {}
            item.code_change.source = item.code_change.source or normalized_tool
            item.code_change.files = item.code_change.files or unified_changes
            item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_unified_patch_text(unified_src)
          end
        end
      end
    end
  end

  -- Add a recent activity line summarizing the tool execution so activity
  -- previews prefer tool summaries over earlier assistant intent lines.
  local tool_name = (item and item.tool_name) or data.toolName
  local detail = (item and item.tool_detail) or nil
  local summary_data = (item and (item.start_data or item.start_input or item.complete_data)) or (item and item.tool_detail and { fullCommandText = item.tool_detail }) or data
  local summary = summarize_tool_activity(tool_name, summary_data) or fallback_tool_activity_summary(tool_name, detail)
  if type(item) == 'table' and summary then
    item.summary = item.summary or summary
  end
  remember_recent_activity_line(summary)
end

-- Initialize assistant_usage sub-module
local assistant_usage = require('copilot_agent.assistant_usage')({
  sanitize_permission_text = sanitize_permission_text,
  remember_recent_activity_line = remember_recent_activity_line,
  remember_recent_activity_item = remember_recent_activity_item,
  append_entry = render.append_entry,
  schedule_render = render.schedule_render,
  state = state,
  normalize_activity_output_text = normalize_activity_output_text,
})

-- Helpers restored after refactor to satisfy lint and preserve behavior.
local function extract_post_tool_use_tool_detail(tool_name, input)
  -- Prefer structured shell command details when available.
  local detail = extract_shell_tool_detail(tool_name, input)
  if detail then
    return detail
  end
  -- Fallback to common descriptive fields.
  return sanitize_permission_text(find_activity_string(input, { 'toolDescription', 'description', 'intention', 'summary' }))
end

local function ensure_recent_tool_activity_item(tool_call_id, tool_name, detail)
  local item, idx = find_recent_tool_activity_item(tool_call_id, tool_name)
  if item then
    return item, idx
  end
  local entry = {
    kind = 'tool',
    tool_name = sanitize_permission_text(tool_name) or nil,
    tool_call_id = sanitize_permission_text(tool_call_id) or nil,
    tool_detail = sanitize_permission_text(detail) or nil,
    start_input = nil,
    output_text = nil,
  }
  local _tmp = { remember_recent_activity_item(entry) }
  idx = _tmp[2]
  return entry, idx
end

local function extract_post_tool_use_result_text(start_input, output, fallback)
  local text = extract_activity_output_value_text(output, 1, {}) or extract_activity_output_value_text(start_input and start_input.toolResult or nil, 1, {}) or fallback
  return normalize_activity_output_text(text)
end

-- Capture tool execution lifecycle events (start/partial/progress/complete)
local function capture_tool_execution_start(data)
  data = type(data) == 'table' and data or {}
  local tool_name = sanitize_permission_text(data.toolName) or find_activity_string(data, { 'toolName' })
  local tool_call_id = sanitize_permission_text(data.toolCallId or data.tool_call_id)
  local detail = extract_shell_tool_detail(tool_name, data) or state.pending_tool_detail
  local item = ensure_recent_tool_activity_item(tool_call_id, tool_name, detail)
  if item then
    item.start_data = item.start_data or data
    local summary = summarize_tool_activity(tool_name, item.start_data) or fallback_tool_activity_summary(tool_name, detail)
    item.summary = item.summary or summary
    local normalized_tool = sanitize_permission_text(tool_name)
    if normalized_tool == 'apply_patch' or normalized_tool == 'edit' then
      local patch_source = item.start_input or item.start_data or data
      local changes = normalize_apply_patch_changes(apply_patch.extract_patch_changes(patch_source))
      if type(changes) == 'table' and #changes > 0 then
        item.code_change = item.code_change or {}
        item.code_change.source = item.code_change.source or normalized_tool
        item.code_change.files = item.code_change.files or changes
        item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_patch_text(patch_source)
      else
        -- edit tool output is a unified diff (diff --git) — try that parser
        local unified_changes = apply_patch.extract_unified_patch_changes(patch_source)
        if type(unified_changes) == 'table' and #unified_changes > 0 then
          item.code_change = item.code_change or {}
          item.code_change.source = item.code_change.source or normalized_tool
          item.code_change.files = item.code_change.files or unified_changes
          item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_unified_patch_text(patch_source)
        end
      end
    end
    remember_recent_activity_line(summary)
  end
  begin_overlay_tool_display(tool_call_id, tool_name, detail)
end

local function capture_tool_execution_partial_result(data)
  data = type(data) == 'table' and data or {}
  local tool_call_id = sanitize_permission_text(data.toolCallId or data.tool_call_id)
  local run_id = overlay_run_id_for_tool_call(tool_call_id)
  touch_overlay_tool_activity(run_id)
  local item = (type(state.recent_activity_items) == 'table' and tool_call_id and state.recent_activity_tool_calls and state.recent_activity_tool_calls[tool_call_id])
      and state.recent_activity_items[state.recent_activity_tool_calls[tool_call_id]]
    or nil
  if not item then
    item = find_recent_tool_activity_item(tool_call_id, data.toolName)
  end
  if type(item) == 'table' then
    local raw_text = data.result or data.partialResult or data.partial_result or data.partialOutput or data.partial_output or data.output
    local text = extract_activity_output_value_text(raw_text, 1, {})
    if text then
      -- Update the transient preview text
      item.output_text = text
    end
    -- Preserve a separate partial_output field when the payload contains
    -- partialResult / partial_result / output fields so the activity item can
    -- expose both the short partial and the eventual full output after
    -- completion.
    if type(raw_text) == 'string' or type(raw_text) == 'table' then
      item.partial_output = normalize_activity_output_text((type(raw_text) == 'string' and raw_text) or (type(raw_text) == 'table' and extract_tool_result_contents_text(raw_text)) or nil)
    end
    item.progress_messages = item.progress_messages or {}
    local message = sanitize_permission_text(data.progressMessage or data.progress_message or data.message)
    if message and (#item.progress_messages == 0 or item.progress_messages[#item.progress_messages] ~= message) then
      item.progress_messages[#item.progress_messages + 1] = message
    end

    -- Debug trace for partial result handling
    do
      local ok, f = pcall(io.open, '/tmp/copilot_agent_debug.log', 'a')
      if ok and f then
        pcall(
          f.write,
          f,
          os.date('%Y-%m-%d %H:%M:%S')
            .. ' capture_tool_execution_partial_result tool_call_id='
            .. tostring(data.toolCallId or data.tool_call_id)
            .. ' raw_text='
            .. vim.inspect(raw_text)
            .. ' item='
            .. vim.inspect(item)
            .. '\n'
        )
        pcall(f.close, f)
      end
    end
  end
end

local function capture_tool_execution_progress(data)
  -- Treat progress similar to partial result: update preview and touch overlay
  capture_tool_execution_partial_result(data)
end

local function capture_post_tool_use_start(data)
  data = type(data) == 'table' and data or {}
  if data.hookType ~= 'postToolUse' then
    return
  end

  local input = type(data.input) == 'table' and data.input or {}
  local tool_name = sanitize_permission_text(input.toolName) or find_activity_string(input, { 'toolName' })
  local tool_call_id = find_activity_string(input, { 'toolCallId', 'tool_call_id' })
  local detail = extract_post_tool_use_tool_detail(tool_name, input)
  local item, idx = ensure_recent_tool_activity_item(tool_call_id, tool_name, detail)
  local prior_output_text = type(item) == 'table' and item.output_text or nil

  if item then
    item.tool_name = item.tool_name or tool_name
    item.tool_call_id = item.tool_call_id or tool_call_id
    item.tool_detail = item.tool_detail or detail
    item.start_input = input
    item.post_tool_use_raw_result_text = extract_activity_output_value_text(input.toolResult, 1, {}) or normalize_activity_output_text(prior_output_text)
    item.output_text = nil
  end

  local hook_id = sanitize_permission_text(data.hookInvocationId)
  if not hook_id then
    return
  end
  if type(state.post_tool_use_hooks) ~= 'table' then
    state.post_tool_use_hooks = {}
  end
  state.post_tool_use_hooks[hook_id] = {
    recent_item_index = idx,
    tool_call_id = tool_call_id,
    tool_name = tool_name,
    tool_detail = detail,
    start_input = input,
    prior_output_text = normalize_activity_output_text(prior_output_text),
  }
end

local function capture_post_tool_use_end(data)
  data = type(data) == 'table' and data or {}
  if data.hookType ~= 'postToolUse' then
    return
  end

  local hook_id = sanitize_permission_text(data.hookInvocationId)
  local hook_state = type(state.post_tool_use_hooks) == 'table' and hook_id and state.post_tool_use_hooks[hook_id] or nil
  local item, idx
  if hook_state and type(hook_state.recent_item_index) == 'number' then
    local items = type(state.recent_activity_items) == 'table' and state.recent_activity_items or {}
    if type(items[hook_state.recent_item_index]) == 'table' then
      item = items[hook_state.recent_item_index]
      idx = hook_state.recent_item_index
    end
  end
  if not item then
    item, idx = find_recent_tool_activity_item(hook_state and hook_state.tool_call_id or nil, hook_state and hook_state.tool_name or nil)
  end

  local start_input = hook_state and hook_state.start_input or nil
  if not start_input and item and type(item.start_input) == 'table' then
    start_input = item.start_input
  end

  local fallback_output_text = hook_state and hook_state.prior_output_text or (item and item.post_tool_use_raw_result_text or nil)
  local result_text = nil
  if data.success == true then
    result_text = extract_post_tool_use_result_text(start_input, data.output, fallback_output_text)
  end
  local error_message = type(data.error) == 'table' and normalize_activity_output_text(data.error.message) or nil

  if hook_state then
    hook_state.recent_item_index = idx
    hook_state.success = data.success == true
    hook_state.result_text = result_text
    hook_state.error_message = error_message
  end

  if item then
    item.tool_name = item.tool_name or (hook_state and hook_state.tool_name or nil)
    item.tool_detail = item.tool_detail or (hook_state and hook_state.tool_detail or nil)
    item.output_text = result_text
    if error_message then
      item.error_message = error_message
    end
    -- Summarize tool usage and ensure a recent activity line exists so the
    -- activity preview highlights the tool activity instead of a preceding
    -- assistant intent.
    local tool_name = item.tool_name or nil
    local detail = item.tool_detail or nil
    local summary_data = item.start_data or item.start_input or (item.tool_detail and { fullCommandText = item.tool_detail }) or data
    log_debug_trace('capture_post_tool_use_end summary_data=', function()
      return summary_data
    end, { max_len = 1200 })
    -- also write to /tmp debug log for immediate capture during headless tests
    do
      local ok, f = pcall(io.open, '/tmp/copilot_agent_debug.log', 'a')
      if ok and f then
        pcall(f.write, f, os.date('%Y-%m-%d %H:%M:%S') .. ' capture_post_tool_use_end summary_data=' .. (vim.inspect(summary_data) or 'nil') .. '\n')
        f:close()
      end
    end
    local summary = summarize_tool_activity(tool_name, summary_data) or fallback_tool_activity_summary(tool_name, detail)
    if type(item) == 'table' then
      local normalized_tool = sanitize_permission_text(tool_name)
      if normalized_tool == 'apply_patch' or normalized_tool == 'edit' then
        local patch_source = item.start_input or item.start_data or summary_data or data
        -- Also consider the tool result text for edit (the diff output)
        local output_source = item.output_text or item.post_tool_use_raw_result_text
        local changes = normalize_apply_patch_changes(apply_patch.extract_patch_changes(patch_source))
        if type(changes) == 'table' and #changes > 0 then
          item.code_change = item.code_change or {}
          item.code_change.source = item.code_change.source or normalized_tool
          item.code_change.files = item.code_change.files or changes
          item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_patch_text(patch_source)
        else
          -- edit tool emits unified diffs — try the output text first, then patch_source
          local unified_src = output_source or patch_source
          local unified_changes = apply_patch.extract_unified_patch_changes(unified_src)
          if (not unified_changes or #unified_changes == 0) and unified_src ~= patch_source then
            unified_changes = apply_patch.extract_unified_patch_changes(patch_source)
          end
          if type(unified_changes) == 'table' and #unified_changes > 0 then
            item.code_change = item.code_change or {}
            item.code_change.source = item.code_change.source or normalized_tool
            item.code_change.files = item.code_change.files or unified_changes
            item.code_change.apply_patch_text = item.code_change.apply_patch_text or apply_patch.extract_unified_patch_text(unified_src)
          end
        end
      end
      log_debug_trace('capture_post_tool_use_end item.output_text=', function()
        return item.output_text
      end, { max_len = 1200 })
      log_debug_trace('capture_post_tool_use_end result data=', function()
        return data
      end, { max_len = 1200 })
      do
        local ok, f = pcall(io.open, '/tmp/copilot_agent_debug.log', 'a')
        if ok and f then
          pcall(f.write, f, os.date('%Y-%m-%d %H:%M:%S') .. ' capture_post_tool_use_end item.output_text=' .. (vim.inspect(item.output_text) or 'nil') .. '\n')
          pcall(f.write, f, os.date('%Y-%m-%d %H:%M:%S') .. ' capture_post_tool_use_end data=' .. (vim.inspect(data) or 'nil') .. '\n')
          f:close()
        end
      end
    end
    item.summary = item.summary or summary
    remember_recent_activity_line(summary)
  end
end

local function capture_turn_activity_summary(event_type, data)
  if event_type == 'assistant.intent' then
    local summary = sanitize_permission_text(data.intent)
    remember_recent_activity_line(summary)
    if summary then
      remember_recent_activity_item({
        kind = 'intent',
        summary = summary,
        data = vim.deepcopy(data),
      })
    end
    return
  end

  if event_type == 'tool.execution_start' then
    capture_tool_execution_start(data)
    return
  end

  if event_type == 'tool.execution_partial_result' then
    capture_tool_execution_partial_result(data)
    return
  end

  if event_type == 'tool.execution_progress' then
    capture_tool_execution_progress(data)
    return
  end

  if event_type == 'tool.execution_complete' then
    capture_tool_execution_complete(data)
    return
  end

  if event_type == 'assistant.usage' then
    assistant_usage.capture(data, { record_activity = false })
    return
  end

  if event_type == 'hook.start' and data.hookType == 'postToolUse' then
    capture_post_tool_use_start(data)
    return
  end

  if event_type == 'hook.end' and data.hookType == 'postToolUse' then
    capture_post_tool_use_end(data)
    return
  end

  if event_type == 'subagent.started' then
    local title = first_non_empty(data.agentDisplayName, data.agentName, data.agentDescription)
    if title then
      local summary = 'Started ' .. title
      remember_recent_activity_line(summary)
      remember_recent_activity_item({
        kind = 'subagent',
        summary = summary,
        data = vim.deepcopy(data),
      })
    end
  end
end

-- Export all public functions
M.looks_like_shell_tool = looks_like_shell_tool
M.overlay_now_ms = overlay_now_ms
M.cancel_overlay_tool_schedule = cancel_overlay_tool_schedule
M.min_overlay_tool_duration_ms = min_overlay_tool_duration_ms
M.overlay_run_id_for_tool_call = overlay_run_id_for_tool_call
M.overlay_run_id_for_tool_identity = overlay_run_id_for_tool_identity
M.begin_overlay_tool_post_result = begin_overlay_tool_post_result
M.set_overlay_tool_result_text = set_overlay_tool_result_text
M.touch_overlay_tool_activity = touch_overlay_tool_activity
M.begin_overlay_tool_display = begin_overlay_tool_display
M.extract_shell_command_text = extract_shell_command_text
M.extract_shell_tool_detail = extract_shell_tool_detail
M.find_activity_value = find_activity_value
M.find_activity_string = find_activity_string
M.find_activity_raw_string = find_activity_raw_string
M.normalize_activity_path = normalize_activity_path
M.normalize_apply_patch_changes = normalize_apply_patch_changes
M.is_file_change_activity_item = is_file_change_activity_item
M.remember_recent_activity_line = remember_recent_activity_line
M.ensure_recent_activity_items = ensure_recent_activity_items
M.remember_recent_activity_item = remember_recent_activity_item
M.find_recent_tool_activity_item = find_recent_tool_activity_item
M.normalize_activity_output_text = normalize_activity_output_text
M.fallback_tool_activity_summary = fallback_tool_activity_summary
M.extract_tool_execution_output_text = extract_tool_execution_output_text
M.capture_tool_execution_complete = capture_tool_execution_complete
M.extract_post_tool_use_tool_detail = extract_post_tool_use_tool_detail
M.ensure_recent_tool_activity_item = ensure_recent_tool_activity_item
M.extract_post_tool_use_result_text = extract_post_tool_use_result_text
M.capture_tool_execution_start = capture_tool_execution_start
M.capture_tool_execution_partial_result = capture_tool_execution_partial_result
M.capture_tool_execution_progress = capture_tool_execution_progress
M.capture_post_tool_use_start = capture_post_tool_use_start
M.capture_post_tool_use_end = capture_post_tool_use_end
M.capture_turn_activity_summary = capture_turn_activity_summary

-- Re-export sub-modules for consumers that need them
M.activity_output = activity_output
M.assistant_usage = assistant_usage

return M
