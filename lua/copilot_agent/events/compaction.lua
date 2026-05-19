--- Compaction event handling extracted from events.lua
local M = {}

-- Dependencies injected via M.init()
local state
local append_entry
local schedule_render
local invalidate_frozen_render_from
local sanitize_permission_text
local summarize_tool_activity
local fallback_tool_activity_summary
local is_file_change_activity_item
local assistant_usage

-- Module-level state
local pending_compaction_activity = nil

--- Inject dependencies. Must be called before any other function.
---@param deps table
function M.init(deps)
  state = deps.state
  append_entry = deps.append_entry
  schedule_render = deps.schedule_render
  invalidate_frozen_render_from = deps.invalidate_frozen_render_from
  sanitize_permission_text = deps.sanitize_permission_text
  summarize_tool_activity = deps.summarize_tool_activity
  fallback_tool_activity_summary = deps.fallback_tool_activity_summary
  is_file_change_activity_item = deps.is_file_change_activity_item
  assistant_usage = deps.assistant_usage
end

local function summarize_compaction_tokens(value, suffix)
  local n = tonumber(value)
  if not n then
    return nil
  end
  local formatted = assistant_usage.format_summary_tokens(n) or tostring(math.floor(n + 0.5))
  if type(suffix) == 'string' and suffix ~= '' then
    return formatted .. ' ' .. suffix
  end
  return formatted
end

local function build_compaction_start_snapshot(data)
  return {
    conversation_tokens = tonumber(data.conversationTokens) or nil,
    system_tokens = tonumber(data.systemTokens) or nil,
    tool_tokens = tonumber(data.toolDefinitionsTokens) or nil,
  }
end

local function compact_checkpoint_label(data)
  local checkpoint_number = tonumber(data.checkpointNumber)
  if checkpoint_number then
    return string.format('checkpoint #%d', checkpoint_number)
  end
  local path = sanitize_permission_text(data.checkpointPath)
  if type(path) == 'string' and path ~= '' then
    local basename = path:match('([^/\\]+)$')
    return basename and ('checkpoint ' .. basename) or ('checkpoint ' .. path)
  end
  return nil
end

local function compaction_start_details(snapshot)
  local parts = {}
  if type(snapshot) == 'table' then
    local conversation = summarize_compaction_tokens(snapshot.conversation_tokens, 'conversation')
    local system = summarize_compaction_tokens(snapshot.system_tokens, 'system')
    local tools = summarize_compaction_tokens(snapshot.tool_tokens, 'tools')
    if conversation then
      parts[#parts + 1] = conversation
    end
    if system then
      parts[#parts + 1] = system
    end
    if tools then
      parts[#parts + 1] = tools
    end
  end
  if #parts == 0 then
    return nil
  end
  return table.concat(parts, ', ')
end

local function format_compaction_start_line(snapshot)
  local detail = compaction_start_details(snapshot)
  if detail then
    return 'Compaction started — ' .. detail
  end
  return 'Compaction started'
end

local function format_compaction_complete_line(data, start_snapshot)
  local headline = data.success == false and 'Compaction failed' or 'Compaction completed'
  local parts = {}
  local checkpoint_label = compact_checkpoint_label(data)
  if checkpoint_label then
    parts[#parts + 1] = checkpoint_label
  end
  local pre_tokens = summarize_compaction_tokens(data.preCompactionTokens, 'tokens')
  if pre_tokens then
    parts[#parts + 1] = 'pre ' .. pre_tokens
  end
  local pre_messages = tonumber(data.preCompactionMessagesLength)
  if pre_messages then
    parts[#parts + 1] = string.format('%d messages', math.floor(pre_messages + 0.5))
  end
  local start_detail = compaction_start_details(start_snapshot)
  if start_detail then
    parts[#parts + 1] = start_detail
  end
  local compaction_tokens_used = type(data.compactionTokensUsed) == 'table' and data.compactionTokensUsed or {}
  local model = sanitize_permission_text(compaction_tokens_used.model)
  if model then
    parts[#parts + 1] = 'model ' .. model
  end
  local summary_input = summarize_compaction_tokens(compaction_tokens_used.inputTokens)
  local summary_output = summarize_compaction_tokens(compaction_tokens_used.outputTokens)
  if summary_input and summary_output then
    parts[#parts + 1] = 'summary ' .. summary_input .. '/' .. summary_output .. ' tokens'
  elseif summary_input then
    parts[#parts + 1] = 'summary ' .. summary_input .. ' tokens in'
  elseif summary_output then
    parts[#parts + 1] = 'summary ' .. summary_output .. ' tokens out'
  end
  if #parts == 0 then
    return headline
  end
  return headline .. ' — ' .. table.concat(parts, ', ')
end

local function compaction_start_item(snapshot)
  return {
    kind = 'session_compaction',
    phase = 'start',
    snapshot = vim.deepcopy(snapshot),
  }
end

local function compaction_complete_item(data)
  local item = {
    kind = 'session_compaction',
    phase = 'complete',
    success = data.success == true,
    checkpoint_number = tonumber(data.checkpointNumber) or nil,
    checkpoint_path = sanitize_permission_text(data.checkpointPath),
    pre_compaction_tokens = tonumber(data.preCompactionTokens) or nil,
    pre_compaction_messages = tonumber(data.preCompactionMessagesLength) or nil,
  }
  local used = type(data.compactionTokensUsed) == 'table' and data.compactionTokensUsed or {}
  item.compaction_tokens_used = {
    model = sanitize_permission_text(used.model),
    input_tokens = tonumber(used.inputTokens) or nil,
    output_tokens = tonumber(used.outputTokens) or nil,
  }
  return item
end

function M.entry_compaction_start_snapshot(entry)
  local items = type(entry) == 'table' and type(entry.activity_items) == 'table' and entry.activity_items or {}
  for _, item in ipairs(items) do
    if type(item) == 'table' and item.kind == 'session_compaction' and item.phase == 'start' then
      return type(item.snapshot) == 'table' and item.snapshot or nil
    end
  end
  return nil
end

function M.handle_compaction_start(data)
  local snapshot = build_compaction_start_snapshot(data)
  local index = append_entry('activity', format_compaction_start_line(snapshot), nil, {
    activity_items = { compaction_start_item(snapshot) },
  })
  pending_compaction_activity = {
    session_id = state.session_id,
    entry_index = index,
  }
end

function M.handle_compaction_complete(data)
  local pending = pending_compaction_activity
  pending_compaction_activity = nil
  if pending and pending.session_id == state.session_id then
    local intervening_activity = false
    for idx = (tonumber(pending.entry_index) or 0) + 1, #state.entries do
      local between = state.entries[idx]
      if type(between) == 'table' and between.kind == 'activity' then
        intervening_activity = true
        break
      end
    end
    local entry = state.entries[pending.entry_index]
    if not intervening_activity and type(entry) == 'table' and entry.kind == 'activity' then
      local snapshot = M.entry_compaction_start_snapshot(entry)
      if snapshot then
        entry.content = format_compaction_complete_line(data, snapshot)
        entry.activity_items = entry.activity_items or {}
        entry.activity_items[#entry.activity_items + 1] = compaction_complete_item(data)
        invalidate_frozen_render_from(pending.entry_index)
        schedule_render()
        return
      end
    end
  end

  append_entry('activity', format_compaction_complete_line(data, nil), nil, {
    activity_items = { compaction_complete_item(data) },
  })
end

function M.flush_recent_activity_summary()
  local lines = state.recent_activity_lines or {}
  local items = state.recent_activity_items or {}
  local appended_activity = false
  if #items > 0 then
    local file_lines, file_items = {}, {}
    local other_lines, other_items = {}, {}
    for _, item in ipairs(items) do
      local summary = nil
      if type(item) == 'table' then
        summary = item.summary
        if not summary and item.kind == 'tool' then
          local tool_name = item.tool_name
          local detail = item.tool_detail
          local summary_data = item.start_data or item.start_input or (detail and { fullCommandText = detail }) or item.complete_data
          summary = summarize_tool_activity(tool_name, summary_data) or fallback_tool_activity_summary(tool_name, detail)
          item.summary = summary
        end
      end
      local target_lines, target_items = other_lines, other_items
      if is_file_change_activity_item(item) then
        target_lines, target_items = file_lines, file_items
      end
      if type(summary) == 'string' and summary ~= '' then
        target_lines[#target_lines + 1] = summary
      end
      target_items[#target_items + 1] = item
    end

    local function collect_group_code_change(group_items)
      local files = {}
      local apply_patch_text
      for _, item in ipairs(group_items or {}) do
        if type(item) == 'table' and type(item.code_change) == 'table' and type(item.code_change.files) == 'table' then
          for _, file in ipairs(item.code_change.files) do
            if type(file) == 'table' and type(file.path) == 'string' and file.path ~= '' then
              local seen = false
              for _, existing in ipairs(files) do
                if type(existing) == 'table' and existing.path == file.path then
                  seen = true
                  break
                end
              end
              if not seen then
                files[#files + 1] = vim.deepcopy(file)
              end
            end
          end
          if not apply_patch_text and type(item.code_change.apply_patch_text) == 'string' and item.code_change.apply_patch_text ~= '' then
            apply_patch_text = item.code_change.apply_patch_text
          end
        end
      end
      if #files == 0 then
        return nil
      end
      return {
        source = 'apply_patch',
        files = files,
        apply_patch_text = apply_patch_text,
      }
    end

    local function append_group(group_lines, group_items, is_file_group)
      if #group_lines == 0 then
        return
      end
      local preserved_items = group_items
      local code_change = is_file_group and collect_group_code_change(preserved_items) or nil
      if state.history_loading ~= true then
        preserved_items = vim.deepcopy(group_items)
        for _, item in ipairs(preserved_items) do
          if type(item) == 'table' then
            item.start_data = nil
            item.complete_data = nil
          end
        end
      end
      local entry_data = {
        activity_items = preserved_items,
      }
      if code_change then
        entry_data.code_change = code_change
      end
      append_entry('activity', table.concat(group_lines, '\n'), nil, entry_data)
      appended_activity = true
    end

    append_group(file_lines, file_items, true)
    append_group(other_lines, other_items, false)
  elseif #lines > 0 then
    append_entry('activity', table.concat(lines, '\n'), nil, {
      activity_items = items,
    })
    appended_activity = true
  end

  state.recent_activity_lines = {}
  state.recent_activity_items = {}
  state.recent_activity_tool_calls = {}
  if appended_activity then
    pending_compaction_activity = nil
  end
end

return M
