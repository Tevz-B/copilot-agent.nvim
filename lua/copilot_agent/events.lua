-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- SSE event stream: parser, host/session event handlers.

local utils = require('copilot_agent.utils')
local approvals = require('copilot_agent.approvals')
local cfg = require('copilot_agent.config')
local http = require('copilot_agent.http')
local logger = require('copilot_agent.log')
local service = require('copilot_agent.service')
local session_names = require('copilot_agent.session_names')
local sl = require('copilot_agent.statusline')
local render = require('copilot_agent.render')
local checkpoints = require('copilot_agent.checkpoints')
local checkpoint_diff = require('copilot_agent.checkpoint_diff')
local window = require('copilot_agent.window')
local log_content_length = cfg.log_content_length
local state = cfg.state
local notify = cfg.notify
local log = logger.log
local should_log = logger.should_log
local serialize_log_value = logger.serialize_log_value
local resolve_log_level = logger.resolve_log_level

local decode_json = http.decode_json
local request = http.request
local build_url = http.build_url

local working_directory = service.working_directory
local TRACE_LOG_LEVEL = vim.log.levels.TRACE or vim.log.levels.DEBUG

local refresh_statuslines = sl.refresh_statuslines

local render_chat = render.render_chat
local schedule_render = render.schedule_render
local stream_update = render.stream_update
local append_entry = render.append_entry
local clear_transcript = render.clear_transcript
local ensure_assistant_entry = render.ensure_assistant_entry
local reset_pending_assistant_entry = render.reset_pending_assistant_entry
local append_reasoning_delta = render.append_reasoning_delta
local clear_reasoning_preview = render.clear_reasoning_preview
local refresh_reasoning_overlay = render.refresh_reasoning_overlay
local reset_frozen_render = render.reset_frozen_render
local invalidate_frozen_render_from = type(render.invalidate_frozen_render_from) == 'function' and render.invalidate_frozen_render_from or function() end
local scroll_to_bottom = render.scroll_to_bottom

local is_thinking_content = utils.is_thinking_content
local tilde_home_path = utils.tilde_home_path

local M = {}
local prompts = require('copilot_agent.events.prompts')
local reset_prompt_state = prompts.reset_prompt_state
local show_user_input_picker = prompts.show_user_input_picker
local handle_user_input = prompts.handle_user_input
local answer_permission = prompts.answer_permission
local sync_model_state = prompts.sync_model_state
local sync_config_counts = prompts.sync_config_counts
local refresh_session_name_from_server = prompts.refresh_session_name_from_server
local enqueue_prompt = prompts.enqueue_prompt
local show_next_prompt = prompts.show_next_prompt
local LOG_PREVIEW_MIN_CHARS = 16 -- Keep log previews readable even when callers request very small snippets.
local DELTA_BOUNDARY_CONTEXT_CHARS = 24 -- Inspect roughly one short word of context on each side when debugging streamed joins.
local DELTA_BOUNDARY_PREVIEW_CHARS = 40 -- Keep boundary diagnostics compact enough to fit on a single log line.
local NEAR_PREFIX_MIN_CHARS = 24 -- Require a meaningful shared prefix before replacing an assistant message with a near-match payload.
local NEAR_PREFIX_MATCH_RATIO = 0.35 -- Treat messages as near-prefix variants only when they share at least 35% of the shorter canonicalized text.
local TRACE_LOG_MAX_CHARS = 3200 -- Large raw event traces are useful for debugging, but still need an upper bound for log files.
local TRACE_LOG_DEPTH = 8 -- Deep enough to inspect nested SDK payloads without exploding trace output.
local ERROR_LOG_MAX_CHARS = 1200 -- Error payloads need more context than routine logs, but should still stay concise.
local DIFF_FLOAT_WIDTH_RATIO = 0.8 -- Use most of the editor width so diffs stay readable without fully covering the workspace.
local DIFF_FLOAT_HEIGHT_RATIO = 0.7 -- Leave visible editor context above and below the proposed-change float.
local FLOAT_VERTICAL_BORDER_LINES = 2 -- Account for the rounded-border float frame when sizing content buffers.
local TURN_END_PROMPT_PREVIEW_CHARS = 200 -- Show enough of the prompt in turn-end logs to identify the completed request.
local intentionally_stopped_event_jobs = {}
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

local function preload_history_checkpoint_ids(session_id)
  local ids = {}
  for _, checkpoint in ipairs(checkpoints.list(session_id)) do
    if type(checkpoint.assistant_message_id) == 'string' and checkpoint.assistant_message_id ~= '' and type(checkpoint.id) == 'string' and checkpoint.id ~= '' then
      ids[checkpoint.assistant_message_id] = {
        id = checkpoint.id,
        prompt = checkpoint.prompt,
      }
    end
  end
  state.history_checkpoint_ids = ids
end

local function assign_history_checkpoint_id(assistant_message_id)
  if type(assistant_message_id) ~= 'string' or assistant_message_id == '' then
    return nil
  end
  local mapping = state.history_checkpoint_ids and state.history_checkpoint_ids[assistant_message_id] or nil
  if type(mapping) ~= 'table' or type(mapping.id) ~= 'string' or mapping.id == '' then
    return
  end
  for idx, pending in ipairs(state.history_pending_user_entries) do
    local entry = state.entries[pending.entry_index]
    local prompt_matches = type(mapping.prompt) ~= 'string' or mapping.prompt == '' or pending.content == mapping.prompt
    if entry and entry.kind == 'user' and prompt_matches then
      entry.checkpoint_id = mapping.id
      table.remove(state.history_pending_user_entries, idx)
      return
    end
  end
end

local function reset_todo_state()
  state.todo_items = {}
  state.todo_item_serial = 0
  state.todo_item_counter = 0
  state.todo_root_id = nil
end

local function next_todo_item_id(prefix)
  state.todo_item_counter = (tonumber(state.todo_item_counter) or 0) + 1
  return string.format('%s-%d', prefix or 'todo', state.todo_item_counter)
end

local function ensure_todo_item(id, data)
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

local function ensure_todo_root()
  local root_id = state.todo_root_id
  if type(state.todo_items) ~= 'table' then
    state.todo_items = {}
  end
  if type(root_id) == 'string' and root_id ~= '' and type(state.todo_items[root_id]) == 'table' then
    return root_id
  end
  root_id = 'intent'
  state.todo_root_id = root_id
  ensure_todo_item(root_id, {
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

local function finish_running_todos(status)
  if type(state.todo_items) ~= 'table' then
    return
  end
  for _, item in pairs(state.todo_items) do
    if type(item) == 'table' and (item.status == 'running' or item.status == 'pending') then
      item.status = status
    end
  end
end

local first_non_empty = utils.first_non_empty

local function preview_log_text(text, max_len)
  return utils.preview_log_text(text, max_len, LOG_PREVIEW_MIN_CHARS, log_content_length)
end

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

local sanitize_log_value = logger.sanitize_log_value
local decode_json_silently = utils.decode_json_silently

local function contains_tool_call_scaffolding_table(value, seen)
  if type(value) ~= 'table' then
    return false
  end

  seen = seen or {}
  if seen[value] then
    return false
  end
  seen[value] = true

  local function item_contains_scaffolding(item)
    if type(item) == 'string' then
      local lowered = item:lower()
      return lowered:find('functions.', 1, true) ~= nil or lowered:find('multi_tool_use.', 1, true) ~= nil
    end
    return contains_tool_call_scaffolding_table(item, seen)
  end

  if vim.islist(value) then
    for _, item in ipairs(value) do
      if item_contains_scaffolding(item) then
        seen[value] = nil
        return true
      end
    end
    seen[value] = nil
    return false
  end

  for key, item in pairs(value) do
    local key_name = type(key) == 'string' and key:lower() or nil
    if key_name == 'tool_uses' or key_name == 'recipient_name' or key_name == 'recipientname' or key_name == 'recipient' then
      seen[value] = nil
      return true
    end
    if item_contains_scaffolding(item) then
      seen[value] = nil
      return true
    end
  end

  seen[value] = nil
  return false
end

local function looks_like_tool_call_scaffolding(text)
  if type(text) ~= 'string' then
    return false
  end

  local trimmed = vim.trim(text)
  if trimmed == '' then
    return false
  end

  local lowered = trimmed:lower()
  if
    lowered:find('<tool_call>', 1, true)
    or lowered:find('<tool_use>', 1, true)
    or lowered:find('to=functions.', 1, true)
    or lowered:find('to=multi_tool_use.', 1, true)
    or lowered:find('recipient="functions.', 1, true)
    or lowered:find('recipient="multi_tool_use.', 1, true)
    or lowered:find('"tool_uses"', 1, true)
    or lowered:find('"recipient_name"', 1, true)
  then
    return true
  end

  local decoded = decode_json_silently(trimmed)
  return contains_tool_call_scaffolding_table(decoded)
end

local function clear_leaked_tool_call_content(entry, idx, key, message_id, source, replacement_content)
  entry.content = type(replacement_content) == 'string' and replacement_content or ''
  log(
    string.format(
      '%s cleared leaked tool-call markup message_id=%s idx=%s key=%s',
      tostring(source or 'assistant.message'),
      tostring(message_id or '<none>'),
      tostring(idx or '<none>'),
      tostring(key or '<none>')
    ),
    vim.log.levels.DEBUG
  )
end

local function log_session_event_payload(event_name, raw_data)
  if event_name ~= 'session.event' or not should_log(TRACE_LOG_LEVEL) then
    return
  end

  local decoded = decode_json_silently(raw_data)
  if type(decoded) == 'table' then
    log_debug_trace('sse.event raw event=' .. tostring(event_name) .. ' payload=', function()
      return sanitize_log_value(decoded)
    end, {
      max_len = TRACE_LOG_MAX_CHARS,
      depth = TRACE_LOG_DEPTH,
    })
    return
  end

  log(string.format('sse.event raw event=%s string=%s', tostring(event_name), serialize_log_value(raw_data, { max_len = TRACE_LOG_MAX_CHARS })), vim.log.levels.DEBUG)
end

local function preview_delta_boundary(current, delta)
  current = type(current) == 'string' and current or ''
  delta = type(delta) == 'string' and delta or ''
  local current_tail = current:sub(math.max(1, #current - (DELTA_BOUNDARY_CONTEXT_CHARS - 1)), #current)
  local delta_head = delta:sub(1, DELTA_BOUNDARY_CONTEXT_CHARS)
  local tail_last = current_tail:sub(-1)
  local head_first = delta_head:sub(1, 1)
  local suspicious_join = tail_last ~= '' and head_first ~= '' and tail_last:match('[%w%)]') and head_first:match('[%w%(]') and not tail_last:match('%s') and not head_first:match('%s')

  return string.format(
    'tail=%s head=%s suspicious=%s',
    preview_log_text(current_tail, DELTA_BOUNDARY_PREVIEW_CHARS),
    preview_log_text(delta_head, DELTA_BOUNDARY_PREVIEW_CHARS),
    tostring(suspicious_join and true or false)
  )
end

-- Reconcile the final assistant.message snapshot with the live draft that was
-- built from raw assistant.message_delta appends.
--
-- Important:
-- - This helper is intentionally for assistant.message only.
-- - Do not route assistant.message_delta through these heuristics; repeated or
--   overlapping-looking substrings may be legitimate streamed output.
-- - Some branches below are intentionally conservative repairs for known final
--   snapshot patterns (punctuation cleanup, shorter authoritative suffixes,
--   line-overlap snapshots). Applied too broadly, the same rules could hide
--   genuine repetition from the model.
--
-- Future work:
-- - Revisit or remove the more aggressive canonical/substring heuristics after
--   the live render path is fully hardened and we have enough trace-backed
--   evidence about the SDK's final assistant.message payload shapes.
-- - Keep the regression coverage around repeated text, suffix replacement, and
--   blank-line overlap up to date before simplifying this function.
local function merge_assistant_message_content(current, incoming, ctx)
  current = type(current) == 'string' and current or ''
  incoming = type(incoming) == 'string' and incoming or ''
  local function finish(decision, result, extra)
    extra = extra or {}
    log(
      string.format(
        'assistant.message merge decision=%s message_id=%s key=%s idx=%s current_len=%d incoming_len=%d result_len=%d overlap=%s current=%s incoming=%s result=%s',
        tostring(decision),
        tostring((ctx or {}).message_id or '<none>'),
        tostring((ctx or {}).entry_key or '<none>'),
        tostring((ctx or {}).entry_index or '<none>'),
        #current,
        #incoming,
        #(type(result) == 'string' and result or ''),
        tostring(extra.overlap or '<none>'),
        preview_log_text(current),
        preview_log_text(incoming),
        preview_log_text(result)
      ),
      vim.log.levels.DEBUG
    )
    return result
  end
  if incoming == '' then
    return finish('ignore-empty-incoming', current)
  end
  if current == '' then
    return finish('replace-empty-or-thinking', incoming)
  end
  local function canonicalize(text)
    -- Use vim.trim() instead of match to avoid O(n²) backtracking on large strings
    return vim.trim(text:gsub('\r\n?', '\n'):gsub('’', "'"):gsub('‘', "'"):gsub('“', '"'):gsub('”', '"'):lower():gsub('[%p%c]+', ' '):gsub('%s+', ' '))
  end
  local function split_message_lines(text)
    return vim.split(text:gsub('\r\n?', '\n'), '\n', { plain = true })
  end
  local function is_blank_line(line)
    return canonicalize(line or '') == ''
  end
  local function trim_blank_edges(lines)
    local first = 1
    local last = #lines
    while first <= last and is_blank_line(lines[first]) do
      first = first + 1
    end
    while last >= first and is_blank_line(lines[last]) do
      last = last - 1
    end
    local trimmed = {}
    for idx = first, last do
      trimmed[#trimmed + 1] = lines[idx]
    end
    return trimmed
  end
  local function trim_leading_blank_lines(lines)
    local first = 1
    while first <= #lines and is_blank_line(lines[first]) do
      first = first + 1
    end
    local trimmed = {}
    for idx = first, #lines do
      trimmed[#trimmed + 1] = lines[idx]
    end
    return trimmed
  end
  local function common_prefix_len(left, right)
    local max_len = math.min(#left, #right)
    local shared = 0
    for idx = 1, max_len do
      if left:sub(idx, idx) ~= right:sub(idx, idx) then
        break
      end
      shared = idx
    end
    return shared
  end
  local function overlap_line_count(current_lines, incoming_lines)
    local max_overlap = math.min(#current_lines, #incoming_lines)
    for overlap = max_overlap, 1, -1 do
      local matches = true
      for idx = 1, overlap do
        local current_line = canonicalize(current_lines[#current_lines - overlap + idx] or '')
        local incoming_line = canonicalize(incoming_lines[idx] or '')
        if current_line ~= incoming_line and (current_line ~= '' or incoming_line ~= '') then
          matches = false
          break
        end
      end
      if matches then
        return overlap
      end
    end
    return 0
  end
  local function literal_suffix_prefix_overlap(left, right)
    local max_overlap = math.min(#left, #right)
    for overlap = max_overlap, 1, -1 do
      if left:sub(#left - overlap + 1) == right:sub(1, overlap) then
        return overlap
      end
    end
    return 0
  end
  local canonical_current = canonicalize(current)
  local canonical_incoming = canonicalize(incoming)
  if current == incoming then
    return finish('keep-identical-literal', current)
  end
  if canonical_current ~= '' and canonical_current == canonical_incoming then
    return finish('replace-canonical-equal', incoming)
  end
  if incoming:sub(1, #current) == current then
    return finish('replace-prefix-extension', incoming)
  end
  -- TODO: Narrow these canonicalized comparisons if future SDK traces show
  -- final assistant.message snapshots are stricter than the current observed
  -- mix of punctuation-only edits, suffix snapshots, and line-overlap updates.
  if canonical_current ~= '' and canonical_incoming ~= '' then
    if vim.startswith(canonical_incoming, canonical_current) then
      return finish('replace-canonical-prefix-extension', incoming)
    end
    if ctx and ctx.prefer_incoming_suffix_replacement == true and #canonical_incoming < #canonical_current and canonical_current:sub(-#canonical_incoming) == canonical_incoming then
      return finish('replace-live-canonical-suffix', incoming)
    end
    if canonical_current:find(canonical_incoming, 1, true) then
      return finish('keep-canonical-substring', current)
    end
  end
  if ctx and ctx.prefer_incoming_suffix_replacement == true and #incoming < #current and current:sub(-#incoming) == incoming then
    return finish('replace-live-literal-suffix', incoming)
  end
  if current:find(incoming, 1, true) then
    return finish('keep-literal-substring', current)
  end
  local current_lines = split_message_lines(current)
  local incoming_lines = split_message_lines(incoming)
  local overlap = overlap_line_count(current_lines, incoming_lines)
  if overlap > 0 then
    local overlap_lines = {}
    local suffix_lines = {}
    for idx = 1, overlap do
      overlap_lines[#overlap_lines + 1] = incoming_lines[idx]
    end
    for idx = overlap + 1, #incoming_lines do
      suffix_lines[#suffix_lines + 1] = incoming_lines[idx]
    end
    overlap_lines = trim_blank_edges(overlap_lines)
    suffix_lines = trim_leading_blank_lines(suffix_lines)
    local merged = {}
    for idx = 1, #current_lines - overlap do
      merged[#merged + 1] = current_lines[idx]
    end
    for _, line in ipairs(overlap_lines) do
      merged[#merged + 1] = line
    end
    for _, line in ipairs(suffix_lines) do
      merged[#merged + 1] = line
    end
    return finish('merge-line-overlap', table.concat(merged, '\n'), { overlap = overlap })
  end
  if ctx and ctx.prefer_incoming_suffix_replacement == true then
    local literal_overlap = literal_suffix_prefix_overlap(current, incoming)
    if literal_overlap >= math.max(8, math.floor(math.min(#current, #incoming) * 0.15)) then
      return finish('merge-live-literal-overlap', current .. incoming:sub(literal_overlap + 1), { overlap = literal_overlap })
    end
  end
  if canonical_current ~= '' and canonical_incoming ~= '' and #canonical_incoming >= #canonical_current then
    local shared_prefix = common_prefix_len(canonical_current, canonical_incoming)
    local min_prefix = math.max(NEAR_PREFIX_MIN_CHARS, math.floor(math.min(#canonical_current, #canonical_incoming) * NEAR_PREFIX_MATCH_RATIO))
    if shared_prefix >= min_prefix then
      return finish('replace-canonical-near-prefix', incoming, { overlap = shared_prefix })
    end
  end
  local separator = current:match('\n$') and '' or '\n'
  return finish('append-fallback', current .. separator .. incoming)
end


-- Tool activity/overlay functions extracted to events/tool_activity.lua
local tool_activity = require('copilot_agent.events.tool_activity')
local overlay_now_ms = tool_activity.overlay_now_ms
local cancel_overlay_tool_schedule = tool_activity.cancel_overlay_tool_schedule
local min_overlay_tool_duration_ms = tool_activity.min_overlay_tool_duration_ms
local overlay_run_id_for_tool_call = tool_activity.overlay_run_id_for_tool_call
local overlay_run_id_for_tool_identity = tool_activity.overlay_run_id_for_tool_identity
local begin_overlay_tool_post_result = tool_activity.begin_overlay_tool_post_result
local set_overlay_tool_result_text = tool_activity.set_overlay_tool_result_text
local touch_overlay_tool_activity = tool_activity.touch_overlay_tool_activity
local extract_shell_command_text = tool_activity.extract_shell_command_text
local extract_shell_tool_detail = tool_activity.extract_shell_tool_detail
local is_file_change_activity_item = tool_activity.is_file_change_activity_item
local fallback_tool_activity_summary = tool_activity.fallback_tool_activity_summary
local capture_turn_activity_summary = tool_activity.capture_turn_activity_summary

local activity_output = tool_activity.activity_output
local summarize_tool_activity = activity_output.summarize_tool_activity

local assistant_usage = tool_activity.assistant_usage

local complete_overlay_tool
local pending_compaction_activity = nil

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

local function entry_compaction_start_snapshot(entry)
  local items = type(entry) == 'table' and type(entry.activity_items) == 'table' and entry.activity_items or {}
  for _, item in ipairs(items) do
    if type(item) == 'table' and item.kind == 'session_compaction' and item.phase == 'start' then
      return type(item.snapshot) == 'table' and item.snapshot or nil
    end
  end
  return nil
end

local function handle_compaction_start(data)
  local snapshot = build_compaction_start_snapshot(data)
  local index = append_entry('activity', format_compaction_start_line(snapshot), nil, {
    activity_items = { compaction_start_item(snapshot) },
  })
  pending_compaction_activity = {
    session_id = state.session_id,
    entry_index = index,
  }
end

local function handle_compaction_complete(data)
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
      local snapshot = entry_compaction_start_snapshot(entry)
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

local function flush_recent_activity_summary()
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

local function schedule_overlay_tool_clear(delay_ms, run_id)
  cancel_overlay_tool_schedule()
  local token = state.overlay_tool_schedule_token
  vim.defer_fn(function()
    if state.overlay_tool_schedule_token ~= token then
      return
    end
    local current = state.overlay_tool_display
    if not current or current.run_id ~= run_id or current.completed ~= true then
      return
    end
    state.overlay_tool_display = nil
    refresh_reasoning_overlay(true)
  end, math.max(0, math.floor(tonumber(delay_ms) or 0)))
end

complete_overlay_tool = function(run_id)
  if type(run_id) ~= 'number' then
    return
  end

  local current = state.overlay_tool_display
  if not current or current.run_id ~= run_id then
    return
  end

  current.last_activity_ms = overlay_now_ms()
  current.completed = true
  current.post_tool_use_pending = nil
  local elapsed = overlay_now_ms() - (tonumber(current.result_updated_ms) or tonumber(current.display_started_ms) or 0)
  local remaining = math.max(0, min_overlay_tool_duration_ms() - elapsed)
  schedule_overlay_tool_clear(remaining, run_id)
end
tool_activity.set_complete_overlay_tool(complete_overlay_tool)

local function reset_overlay_tool_state()
  cancel_overlay_tool_schedule()
  state.overlay_tool_display = nil
  state.overlay_tool_queue = {}
end

local active_background_task_status = {
  running = true,
  inbox = true,
  idle = true,
}

local function reset_live_activity_state()
  state.pending_checkpoint_ops = 0
  state.pending_workspace_updates = 0
  state.background_tasks = {}
  state.active_tool = nil
  state.active_tool_run_id = nil
  state.active_tool_detail = nil
  state.pending_tool_detail = nil
  state.recent_activity_lines = {}
  reset_overlay_tool_state()
  state.active_turn_assistant_index = nil
  state.live_assistant_entry_index = nil
  state.active_turn_assistant_message_id = nil
  state.active_assistant_merge_group = nil
  state.assistant_chunk_state = {}
  state.current_intent = nil
  pending_compaction_activity = nil
  clear_reasoning_preview('live activity reset')
end

local function set_background_task(key, opts)
  if type(key) ~= 'string' or key == '' then
    return
  end

  opts = opts or {}
  local status = opts.status
  if not active_background_task_status[status] then
    if state.background_tasks[key] ~= nil then
      state.background_tasks[key] = nil
      refresh_statuslines()
      refresh_reasoning_overlay()
    end
    return
  end

  local task = state.background_tasks[key] or { id = key }
  task.kind = opts.kind or task.kind
  task.status = status
  task.title = first_non_empty(opts.title, task.title)
  task.description = first_non_empty(opts.description, task.description)
  state.background_tasks[key] = task
  refresh_statuslines()
  refresh_reasoning_overlay()
end

-- Show a diff in a floating window.
-- Tries the configured external diff command (e.g. delta) in a terminal buffer;
-- falls back to a plain diff buffer if the command is unavailable.
local function show_diff_float(diff_text, after_close)
  local lines = vim.split(diff_text, '\n', { plain = true })
  local width = math.min(math.floor(vim.o.columns * DIFF_FLOAT_WIDTH_RATIO), log_content_length)
  local height = math.min(#lines + FLOAT_VERTICAL_BORDER_LINES, math.floor(vim.o.lines * DIFF_FLOAT_HEIGHT_RATIO))
  local win_opts = {
    relative = 'editor',
    width = width,
    height = height,
    row = math.floor((vim.o.lines - height) / 2),
    col = math.floor((vim.o.columns - width) / 2),
    style = 'minimal',
    border = 'rounded',
    title = ' Proposed changes (<C-c> to exit) ',
    title_pos = 'center',
  }

  local function setup_close_keys(buf, win)
    local function close()
      if vim.api.nvim_win_is_valid(win) then
        vim.api.nvim_win_close(win, true)
      end
      if after_close then
        after_close()
      end
    end
    vim.keymap.set('n', 'q', close, { buffer = buf, nowait = true })
    vim.keymap.set('n', '<Esc><Esc>', close, { buffer = buf, nowait = true })
    vim.keymap.set({ 'n', 'i', 't' }, '<C-c>', close, { buffer = buf, nowait = true })
  end

  local function open_ansi_float(output)
    local buf = vim.api.nvim_create_buf(false, true)
    vim.bo[buf].buftype = 'nofile'
    vim.bo[buf].bufhidden = 'wipe'
    vim.bo[buf].swapfile = false
    local win = vim.api.nvim_open_win(buf, true, win_opts)
    window.disable_folds(win)
    setup_close_keys(buf, win)
    local chan = vim.api.nvim_open_term(buf, {})
    if chan and chan > 0 then
      vim.api.nvim_chan_send(chan, output)
    end
  end

  local diff_cmd = state.config.chat.diff_cmd
  if diff_cmd and type(diff_cmd) == 'table' and #diff_cmd > 0 then
    diff_cmd = vim.list_extend({}, diff_cmd)
    if diff_cmd[1] == 'delta' then
      if width >= 100 and not vim.tbl_contains(diff_cmd, '--side-by-side') then
        table.insert(diff_cmd, '--side-by-side')
      end
      if not vim.tbl_contains(diff_cmd, '--paging=never') then
        table.insert(diff_cmd, '--paging=never')
      end
    end
  end
  if diff_cmd and type(diff_cmd) == 'table' and #diff_cmd > 0 and vim.fn.executable(diff_cmd[1]) == 1 then
    local ok, output = pcall(vim.fn.system, diff_cmd, diff_text)
    if ok and type(output) == 'string' and output ~= '' then
      open_ansi_float(output)
      return
    end
  end

  local buf = vim.api.nvim_create_buf(false, true)
  vim.api.nvim_buf_set_lines(buf, 0, -1, false, lines)
  vim.bo[buf].buftype = 'nofile'
  vim.bo[buf].bufhidden = 'wipe'
  vim.bo[buf].swapfile = false
  vim.bo[buf].filetype = 'diff'
  vim.bo[buf].modifiable = false
  local win = vim.api.nvim_open_win(buf, true, win_opts)
  window.disable_folds(win)
  setup_close_keys(buf, win)
end

local function is_recoverable_stream_exit(code, stderr_message)
  if code == 7 or code == 18 or code == 52 or code == 56 then
    return true
  end
  if utils.is_connection_error(stderr_message) then
    return true
  end
  return false
end

local function format_stream_exit_message(code, stderr_message)
  if type(stderr_message) == 'string' and stderr_message ~= '' then
    return stderr_message
  end
  return 'Event stream stopped with exit code ' .. tostring(code)
end

local function neovim_is_exiting()
  if state.shutting_down then
    return true
  end
  local exiting = vim.v.exiting
  if exiting == vim.NIL or exiting == nil then
    return false
  end
  if type(exiting) == 'number' then
    return exiting ~= 0
  end
  if type(exiting) == 'string' then
    return exiting ~= '' and exiting ~= '0'
  end
  return true
end

function M._handle_event_stream_exit(session_id, code, stderr_message, opts)
  opts = opts or {}
  local exit_message = format_stream_exit_message(code, stderr_message)

  if opts.stopped_intentionally or code == 0 or state.session_id ~= session_id or neovim_is_exiting() then
    return
  end

  if state.config.service.auto_start ~= true or not is_recoverable_stream_exit(code, stderr_message) then
    append_entry('error', exit_message)
    return
  end

  if state.event_stream_recovery_session_id == session_id then
    return
  end

  local debug_reconnect = should_log(vim.log.levels.DEBUG)

  state.event_stream_recovery_session_id = session_id
  state.history_loading = false
  state.chat_busy = false
  refresh_statuslines()
  append_entry('system', 'Event stream disconnected. Reconnecting...')

  if debug_reconnect then
    local detail = string.format(
      '[%s] SSE exit code=%s stderr=%s session=%s base_url=%s',
      os.date('%H:%M:%S'),
      tostring(code),
      tostring(stderr_message or ''):sub(1, 200),
      tostring(session_id),
      tostring(state.config.base_url or '')
    )
    notify(detail, vim.log.levels.DEBUG)
  end

  service.ensure_service_live(function(service_err)
    if neovim_is_exiting() then
      state.event_stream_recovery_session_id = nil
      return
    end
    if state.session_id ~= session_id then
      state.event_stream_recovery_session_id = nil
      return
    end

    if service_err then
      state.event_stream_recovery_session_id = nil
      append_entry('error', exit_message)
      append_entry('error', 'Failed to recover event stream: ' .. service_err)
      return
    end

    if debug_reconnect then
      notify(string.format('[%s] Service alive at %s, resuming session %s', os.date('%H:%M:%S'), tostring(state.config.base_url or ''), tostring(session_id)), vim.log.levels.DEBUG)
    end

    state.creating_session = true
    local session = require('copilot_agent.session')
    session.resume_session(session_id, function(_, resume_err)
      if neovim_is_exiting() then
        state.event_stream_recovery_session_id = nil
        return
      end
      if state.session_id ~= session_id then
        state.event_stream_recovery_session_id = nil
        return
      end
      if resume_err and session.is_missing_session_error(resume_err) then
        if debug_reconnect then
          notify(string.format('[%s] Session %s missing on server, recovering after restart', os.date('%H:%M:%S'), tostring(session_id)), vim.log.levels.DEBUG)
        end
        session.recover_after_service_restart(session_id, function(_, recovery_err)
          if neovim_is_exiting() then
            state.event_stream_recovery_session_id = nil
            return
          end
          state.event_stream_recovery_session_id = nil
          if recovery_err then
            append_entry('error', 'Failed to recover event stream: ' .. recovery_err)
          elseif debug_reconnect then
            notify(string.format('[%s] Session recovered after service restart', os.date('%H:%M:%S')), vim.log.levels.DEBUG)
          end
        end)
        return
      end
      state.event_stream_recovery_session_id = nil
      if resume_err then
        append_entry('error', 'Failed to recover event stream: ' .. resume_err)
      elseif debug_reconnect then
        notify(string.format('[%s] SSE reconnected session=%s url=%s', os.date('%H:%M:%S'), tostring(session_id), tostring(state.config.base_url or '')), vim.log.levels.DEBUG)
      end
    end, {
      guard_current_session_id = session_id,
      suppress_error_ui = true,
    })
  end)
end

prompts.set_helpers({
  extract_shell_command_text = extract_shell_command_text,
  sanitize_permission_text = sanitize_permission_text,
  show_diff_float = show_diff_float,
})


local function live_turn_cleanup_needed()
  return state.chat_busy == true
    or state.pending_checkpoint_turn ~= nil
    or state.stream_line_start ~= nil
    or type(state.active_turn_assistant_index) == 'number'
    or type(state.live_assistant_entry_index) == 'number'
    or (type(state.active_turn_assistant_message_id) == 'string' and state.active_turn_assistant_message_id ~= '')
    or (type(state.active_assistant_merge_group) == 'string' and state.active_assistant_merge_group ~= '')
    or (type(state.reasoning_entry_key) == 'string' and state.reasoning_entry_key ~= '')
    or (state.reasoning_text or '') ~= ''
    or #(state.reasoning_lines or {}) > 0
    or state.active_tool ~= nil
    or state.active_tool_run_id ~= nil
    or state.active_tool_detail ~= nil
    or state.pending_tool_detail ~= nil
    or state.current_intent ~= nil
    or #(state.recent_activity_lines or {}) > 0
    or #(state.recent_activity_items or {}) > 0
end

local function clear_live_turn_state(reason, opts)
  opts = opts or {}
  local had_live_state = live_turn_cleanup_needed()

  if opts.flush_activity ~= false then
    flush_recent_activity_summary()
  end
  reset_pending_assistant_entry()
  if opts.clear_pending_checkpoint_turn then
    state.pending_checkpoint_turn = nil
  end
  clear_reasoning_preview(reason)
  state.stream_line_start = nil
  state.chat_busy = false
  complete_overlay_tool(state.active_tool_run_id)
  state.recent_activity_lines = {}
  state.recent_activity_items = {}
  state.recent_activity_tool_calls = {}
  state.post_tool_use_hooks = {}
  state.active_tool = nil
  state.active_tool_run_id = nil
  state.active_tool_detail = nil
  state.pending_tool_detail = nil
  state.active_turn_assistant_index = nil
  state.live_assistant_entry_index = nil
  state.active_turn_assistant_message_id = nil
  state.active_assistant_merge_group = nil
  state.assistant_chunk_state = {}
  state.current_intent = nil
  window.sync_chat_markdown_conceal(state.chat_winid)
  refresh_statuslines()
  refresh_reasoning_overlay()
  return had_live_state
end

local function handle_host_event(event_name, payload)
  log_debug_trace('host.event received event=' .. tostring(event_name) .. ' payload=', function()
    return payload
  end, { max_len = 2400 })
  if event_name == 'host.user_input_requested' then
    handle_user_input(payload)
    return
  end

  local data = payload and payload.data or {}
  if event_name == 'host.session_attached' then
    clear_reasoning_preview('session attached')
    sync_model_state(data.model, data.reasoningEffort, data.sessionId or state.session_id)
    sync_config_counts(data)
    state.last_assistant_usage = nil
    state.context_tokens = nil
    state.context_limit = nil
    state.session_name = session_names.resolve(data.summary, data.sessionId or state.session_id)
    if not state.session_name or state.session_name == '' then
      refresh_session_name_from_server(data.sessionId or state.session_id)
    end
    refresh_statuslines()
    append_entry('system', 'Connected to session ' .. (data.sessionId or state.session_id or '<unknown>'))
  elseif event_name == 'host.session_name_updated' then
    if not session_names.get(data.sessionId or state.session_id) then
      state.session_name = data.name or state.session_name
    end
    refresh_statuslines()
  elseif event_name == 'host.model_changed' then
    sync_model_state(data.model, data.reasoningEffort, data.sessionId or state.session_id)
    refresh_statuslines()
    append_entry('system', 'Model changed to ' .. tostring(data.model or '<unknown>'))
  elseif event_name == 'host.session_disconnected' then
    state.instruction_count = 0
    state.agent_count = 0
    state.skill_count = 0
    state.mcp_count = 0
    state.last_assistant_usage = nil
    state.context_tokens = nil
    state.context_limit = nil
    state.session_name = nil
    state.pending_user_input = nil
    reset_todo_state()
    reset_live_activity_state()
    reset_prompt_state()
    refresh_statuslines()
    append_entry('system', 'Session disconnected')
  elseif event_name == 'host.turn_aborted' then
    local sid = state.session_id
    local event_session_id = data.sessionId
    if type(event_session_id) == 'string' and sid and event_session_id ~= sid then
      log(string.format('host.turn_aborted ignored event_session_id=%s active_session_id=%s', tostring(event_session_id), tostring(sid)), vim.log.levels.DEBUG)
      return
    end
    if clear_live_turn_state('turn aborted', { clear_pending_checkpoint_turn = true }) then
      append_entry('system', 'Turn cancelled')
    end
  elseif event_name == 'host.permission_requested' then
    -- In interactive mode, Go sends a request object with an ID; ask the user.
    local req = data.request or {}
    local req_id = req.id
    local mode = data.mode or 'interactive'
    local perm = req.request or {}
    if perm.kind == 'shell' then
      state.pending_tool_detail = extract_shell_command_text(perm)
    elseif perm.kind == 'mcp' or perm.kind == 'custom-tool' then
      state.pending_tool_detail = sanitize_permission_text(perm.toolDescription or perm.intention)
    else
      state.pending_tool_detail = nil
    end
    if mode == 'interactive' and req_id then
      local kind = perm.kind or 'unknown'
      local sid = state.session_id
      local event_session_id = data.sessionId
      if type(event_session_id) == 'string' and sid and event_session_id ~= sid then
        log(string.format('host.permission_requested ignored event_session_id=%s active_session_id=%s', tostring(event_session_id), tostring(sid)), vim.log.levels.DEBUG)
        return
      end
      local auto_allowed = (kind == 'read' or kind == 'write') and approvals.directory_allowed(perm.path or perm.fileName) or approvals.tool_allowed(perm)
      if auto_allowed and sid then
        log(
          string.format('host.permission_requested auto-approved kind=%s request_id=%s detail=%s', tostring(kind), tostring(req_id), serialize_log_value(perm, { max_len = log_content_length * 10 })),
          vim.log.levels.DEBUG
        )
        answer_permission(sid, req_id, true)
        return
      end
      if not enqueue_prompt('permission', payload) then
        log(string.format('host.permission_requested dropped duplicate request_id=%s kind=%s', tostring(req_id), tostring(kind)), vim.log.levels.DEBUG)
        return
      end
      log(
        string.format('host.permission_requested enqueued request_id=%s kind=%s detail=%s', tostring(req_id), tostring(kind), serialize_log_value(perm, { max_len = ERROR_LOG_MAX_CHARS })),
        vim.log.levels.DEBUG
      )
      vim.schedule(show_next_prompt)
    end
    -- Auto-approved/rejected decisions are reflected in the statusline; no notify needed.
  elseif event_name == 'host.permission_decision' then
    -- Silently update the statusline; avoid noisy "Permission approved (autopilot)" spam.
    refresh_statuslines()
  elseif event_name == 'host.permission_mode_changed' then
    state.permission_mode = data.mode or state.permission_mode
    refresh_statuslines()
  elseif event_name == 'host.history_done' then
    if should_log(vim.log.levels.DEBUG) then
      notify(string.format('[%s] History replay finished session=%s entries=%d', os.date('%H:%M:%S'), tostring(state.session_id), #state.entries), vim.log.levels.DEBUG)
    end
    state.history_loading = false
    state.history_replay_turn_index = 0
    state.history_checkpoint_ids = nil
    state.history_pending_user_entries = {}
    refresh_statuslines()
    reset_frozen_render()
    render_chat()
    scroll_to_bottom()
  end
end

-- Open a diff split for a file that the agent just modified.
-- Uses git to show the diff if the file is tracked; otherwise skips.
local function offer_diff_review(abs_path, rel_path)
  -- Only offer if file is git-tracked (has a HEAD version).
  local wd = working_directory()
  vim.fn.systemlist({ 'git', '-C', wd, 'cat-file', '-e', 'HEAD:' .. rel_path })
  if vim.v.shell_error ~= 0 then
    return -- not tracked or no HEAD version
  end

  vim.ui.select({ 'Open diff', 'Skip' }, {
    prompt = 'Review changes to ' .. rel_path .. '?',
  }, function(choice)
    if choice ~= 'Open diff' then
      return
    end
    -- Get the old (HEAD) version.
    local old_lines = vim.fn.systemlist({ 'git', '-C', wd, 'show', 'HEAD:' .. rel_path })
    if vim.v.shell_error ~= 0 then
      notify('Could not read old version of ' .. rel_path, vim.log.levels.WARN)
      return
    end

    -- Open the current file.
    vim.cmd('tabnew ' .. vim.fn.fnameescape(abs_path))
    vim.cmd('diffthis')

    -- Create a scratch buffer with the old version.
    vim.cmd('vnew')
    local scratch = vim.api.nvim_get_current_buf()
    vim.bo[scratch].buftype = 'nofile'
    vim.bo[scratch].bufhidden = 'wipe'
    vim.bo[scratch].swapfile = false
    vim.api.nvim_buf_set_name(scratch, rel_path .. ' (before agent)')
    vim.api.nvim_buf_set_lines(scratch, 0, -1, false, old_lines)
    -- Set filetype from the original for syntax highlighting.
    local ft = vim.filetype.match({ filename = abs_path }) or ''
    if ft ~= '' then
      vim.bo[scratch].filetype = ft
    end
    vim.bo[scratch].modifiable = false
    window.disable_folds(vim.api.nvim_get_current_win())
    vim.cmd('diffthis')
  end)
end

local function read_disk_lines(abs_path)
  local ok, lines = pcall(vim.fn.readfile, abs_path)
  if not ok or type(lines) ~= 'table' then
    return nil, lines
  end
  return lines
end

local function file_change_summary(old_lines, new_lines)
  old_lines = old_lines or {}
  new_lines = new_lines or {}
  local old_text = table.concat(old_lines, '\n')
  local new_text = table.concat(new_lines, '\n')
  if old_text == new_text then
    return 'no content change'
  end

  if not vim.text.diff then
    local delta = #new_lines - #old_lines
    if delta > 0 then
      return string.format('+%d lines', delta)
    elseif delta < 0 then
      return string.format('-%d lines', math.abs(delta))
    end
    return 'content updated'
  end

  local hunks = vim.text.diff(old_text, new_text, { result_type = 'indices' }) or {}
  local added = 0
  local removed = 0
  local changed = 0
  for _, hunk in ipairs(hunks) do
    local old_count = tonumber(hunk[2]) or 0
    local new_count = tonumber(hunk[4]) or 0
    changed = changed + math.min(old_count, new_count)
    if new_count > old_count then
      added = added + (new_count - old_count)
    elseif old_count > new_count then
      removed = removed + (old_count - new_count)
    end
  end

  local parts = {}
  if added > 0 then
    parts[#parts + 1] = '+' .. added
  end
  if removed > 0 then
    parts[#parts + 1] = '-' .. removed
  end
  if changed > 0 then
    parts[#parts + 1] = '~' .. changed
  end
  parts[#parts + 1] = string.format('%d %s', #hunks, #hunks == 1 and 'hunk' or 'hunks')
  return table.concat(parts, ' ')
end

local function restore_window_views(bufnr, views)
  for _, winid in ipairs(vim.fn.win_findbuf(bufnr)) do
    if vim.api.nvim_win_is_valid(winid) and views[winid] then
      pcall(vim.api.nvim_win_call, winid, function()
        vim.fn.winrestview(views[winid])
      end)
    end
  end
end

local external_reload_prompt = 'The open buffer has been updated externally. Do you want to reload it? (yes/no)'
local buffer_disk_state = {}

local function confirm_external_buffer_reload()
  return vim.fn.confirm(external_reload_prompt, '&yes\n&no', 2) == 1
end

local function file_stat_signature(abs_path)
  local uv = vim.uv or vim.loop
  local stat = uv.fs_stat(abs_path)
  if not stat then
    return nil
  end
  local mtime = stat.mtime or {}
  return table.concat({
    tostring(tonumber(mtime.sec) or 0),
    tostring(tonumber(mtime.nsec) or 0),
    tostring(tonumber(stat.size) or 0),
    tostring(tonumber(stat.mode) or 0),
  }, ':')
end

local function buffer_abs_path(bufnr)
  if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
    return nil
  end
  local name = vim.api.nvim_buf_get_name(bufnr)
  if name == '' then
    return nil
  end
  return vim.fn.fnamemodify(name, ':p')
end

local function remember_buffer_disk_state(bufnr, abs_path)
  abs_path = abs_path or buffer_abs_path(bufnr)
  if not abs_path then
    buffer_disk_state[bufnr] = nil
    return nil
  end
  buffer_disk_state[bufnr] = file_stat_signature(abs_path)
  return buffer_disk_state[bufnr]
end

local function forget_buffer_disk_state(bufnr)
  buffer_disk_state[bufnr] = nil
end

local function remember_open_buffer_disk_state()
  for _, bufnr in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_valid(bufnr) and vim.api.nvim_buf_is_loaded(bufnr) then
      if vim.api.nvim_get_option_value('buftype', { buf = bufnr }) == '' then
        remember_buffer_disk_state(bufnr)
      end
    end
  end
end

local function modified_buffer_changed_on_disk(bufnr, abs_path)
  local current = file_stat_signature(abs_path)
  if not current then
    return false
  end
  local known = buffer_disk_state[bufnr]
  if known == nil then
    buffer_disk_state[bufnr] = current
    return false
  end
  return known ~= current
end

local function clean_buffer_changed_on_disk(bufnr, abs_path)
  local current = file_stat_signature(abs_path)
  if not current then
    return false
  end
  local known = buffer_disk_state[bufnr]
  if known == nil then
    buffer_disk_state[bufnr] = current
    return false
  end
  return known ~= current
end

local function reload_buffer_from_disk(bufnr, abs_path, opts)
  opts = opts or {}
  local new_lines, read_err = read_disk_lines(abs_path)
  if not new_lines then
    return nil, read_err
  end

  local old_lines = vim.api.nvim_buf_get_lines(bufnr, 0, -1, false)
  local summary = file_change_summary(old_lines, new_lines)
  local wins = vim.fn.win_findbuf(bufnr)
  local views = {}
  for _, winid in ipairs(wins) do
    if vim.api.nvim_win_is_valid(winid) then
      views[winid] = vim.api.nvim_win_call(winid, function()
        return vim.fn.winsaveview()
      end)
    end
  end

  local modified = vim.bo[bufnr].modified
  if modified and opts.force ~= true then
    return summary, 'buffer has unsaved changes'
  end

  if #wins > 0 then
    local ok, reload_err = pcall(vim.api.nvim_win_call, wins[1], function()
      vim.cmd('silent keepalt keepjumps edit')
    end)
    if not ok then
      return summary, reload_err
    end
    restore_window_views(bufnr, views)
    remember_buffer_disk_state(bufnr, abs_path)
    return summary, nil
  end

  if modified then
    return summary, 'buffer has unsaved changes'
  end

  local was_modifiable = vim.bo[bufnr].modifiable
  local was_readonly = vim.bo[bufnr].readonly
  vim.bo[bufnr].modifiable = true
  vim.bo[bufnr].readonly = false
  vim.api.nvim_buf_set_lines(bufnr, 0, -1, false, new_lines)
  vim.bo[bufnr].modified = false
  vim.bo[bufnr].modifiable = was_modifiable
  vim.bo[bufnr].readonly = was_readonly
  restore_window_views(bufnr, views)
  remember_buffer_disk_state(bufnr, abs_path)
  return summary, nil
end

local function sanitize_reload_error(err)
  if err == nil then
    return nil
  end

  local line = tostring(err):match('([^\n]+)')
  return line or tostring(err)
end

local function is_expected_reload_attention(err)
  local message = sanitize_reload_error(err)
  if not message then
    return false
  end

  return message == 'buffer has unsaved changes' or message:find('E37:', 1, true) ~= nil or message:find('No write since last change', 1, true) ~= nil
end

local function reload_attention_level(err)
  if is_expected_reload_attention(err) then
    return vim.log.levels.INFO
  end
  return vim.log.levels.WARN
end

local function check_open_buffers_for_external_changes(opts)
  opts = opts or {}
  local uv = vim.uv or vim.loop
  local target_path = type(opts.path) == 'string' and vim.fn.fnamemodify(opts.path, ':p') or nil
  local target_prefix = type(opts.prefix) == 'string' and vim.fn.fnamemodify(opts.prefix, ':p') or nil
  local skip_path = type(opts.skip_path) == 'string' and vim.fn.fnamemodify(opts.skip_path, ':p') or nil

  for _, bufnr in ipairs(vim.api.nvim_list_bufs()) do
    if vim.api.nvim_buf_is_valid(bufnr) and vim.api.nvim_buf_is_loaded(bufnr) then
      if vim.api.nvim_get_option_value('buftype', { buf = bufnr }) == '' then
        local name = vim.api.nvim_buf_get_name(bufnr)
        if name ~= '' then
          local path = vim.fn.fnamemodify(name, ':p')
          local matches_path = not target_path or path == target_path
          local matches_prefix = not target_prefix or vim.startswith(path, target_prefix)
          local is_skipped = skip_path and path == skip_path
          if matches_path and matches_prefix and not is_skipped and uv.fs_stat(path) then
            if vim.bo[bufnr].modified then
              if modified_buffer_changed_on_disk(bufnr, path) and confirm_external_buffer_reload() then
                local summary, reload_err = reload_buffer_from_disk(bufnr, path, { force = true })
                if reload_err then
                  notify(
                    'External reload needs attention: ' .. vim.fn.fnamemodify(path, ':t') .. ' (' .. tostring(summary or 'content updated') .. '); ' .. tostring(sanitize_reload_error(reload_err)),
                    reload_attention_level(reload_err)
                  )
                end
              end
            else
              if clean_buffer_changed_on_disk(bufnr, path) then
                reload_buffer_from_disk(bufnr, path)
              end
            end
          end
        end
      end
    end
  end
end

local open_buffer_refresh_pending = false

local function schedule_open_buffer_refresh()
  if open_buffer_refresh_pending then
    return
  end

  open_buffer_refresh_pending = true
  vim.defer_fn(function()
    open_buffer_refresh_pending = false
    check_open_buffers_for_external_changes()
  end, 60)
end

local function clear_reasoning_preview_on_assistant_content(content, reason)
  if state.history_loading then
    return
  end
  if is_thinking_content(content) then
    if (state.reasoning_text or '') ~= '' or #(state.reasoning_lines or {}) > 0 then
      log(
        string.format(
          'assistant content preserved reasoning because content still looks like thinking reason=%s len=%d preview=%s',
          tostring(reason or 'unspecified'),
          #(content or ''),
          preview_log_text(content)
        ),
        vim.log.levels.DEBUG
      )
    end
    return
  end
  if (state.reasoning_text or '') == '' and #(state.reasoning_lines or {}) == 0 then
    log(
      string.format('assistant content had no reasoning preview to clear reason=%s len=%d preview=%s', tostring(reason or 'unspecified'), #(content or ''), preview_log_text(content)),
      vim.log.levels.DEBUG
    )
    return
  end
  log(string.format('assistant content clearing reasoning reason=%s len=%d preview=%s', tostring(reason or 'unspecified'), #(content or ''), preview_log_text(content)), vim.log.levels.DEBUG)
  clear_reasoning_preview(reason)
end

local function preserve_reasoning_preview_on_late_turn_start()
  local has_reasoning = (state.reasoning_text or '') ~= '' or #(state.reasoning_lines or {}) > 0
  if not has_reasoning then
    return false
  end

  local has_live_assistant = type(state.active_turn_assistant_index) == 'number'
    or type(state.live_assistant_entry_index) == 'number'
    or (type(state.active_turn_assistant_message_id) == 'string' and state.active_turn_assistant_message_id ~= '')
  if not has_live_assistant then
    return false
  end

  log(
    string.format(
      'assistant.turn_start preserved reasoning because live turn activity already exists key=%s active_index=%s live_index=%s message_id=%s text_len=%d lines=%d',
      tostring(state.reasoning_entry_key or '<none>'),
      tostring(state.active_turn_assistant_index or '<none>'),
      tostring(state.live_assistant_entry_index or '<none>'),
      tostring(state.active_turn_assistant_message_id or '<none>'),
      #(state.reasoning_text or ''),
      #(state.reasoning_lines or {})
    ),
    vim.log.levels.DEBUG
  )
  return true
end

local function handle_todo_event(event_type, data)
  if event_type == 'assistant.intent' then
    state.current_intent = sanitize_permission_text(data.intent)
    local todo = ensure_todo_item(ensure_todo_root(), {
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
    local todo = ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      description = data.agentDescription,
      status = 'running',
    })
    todo.parent_id = ensure_todo_root()
    todo.title = first_non_empty(data.agentDisplayName, data.agentName, todo.title, 'Subagent')
    todo.description = data.agentDescription
    return true
  end

  if event_type == 'subagent.completed' then
    set_background_task('subagent:' .. (data.toolCallId or ''), {
      kind = 'subagent',
      status = 'completed',
    })
    local todo = ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      status = 'done',
    })
    todo.parent_id = ensure_todo_root()
    todo.status = 'done'
    schedule_open_buffer_refresh()
    return true
  end

  if event_type == 'subagent.failed' then
    set_background_task('subagent:' .. (data.toolCallId or ''), {
      kind = 'subagent',
      status = 'failed',
    })
    local todo = ensure_todo_item(sanitize_permission_text(data.toolCallId) or next_todo_item_id('subagent'), {
      kind = 'subagent',
      parent_id = ensure_todo_root(),
      title = first_non_empty(data.agentDisplayName, data.agentName, 'Subagent'),
      status = 'blocked',
    })
    todo.parent_id = ensure_todo_root()
    todo.status = 'blocked'
    append_todo_progress(todo, type(data.error) == 'table' and data.error.message or data.message)
    schedule_open_buffer_refresh()
    return true
  end

  if event_type == 'tool.execution_start' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    local detail = extract_shell_tool_detail(data.toolName, data) or state.pending_tool_detail
    local todo = ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = ensure_todo_root(),
      title = summarize_tool_activity(data.toolName, data) or fallback_tool_activity_summary(data.toolName, detail),
      tool_name = sanitize_permission_text(data.toolName),
      tool_call_id = tool_call_id,
      tool_detail = detail,
      status = 'running',
    })
    todo.parent_id = ensure_todo_root()
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
    local todo = ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = ensure_todo_root(),
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
    local todo = ensure_todo_item(tool_call_id or next_todo_item_id('tool'), {
      kind = 'tool',
      parent_id = ensure_todo_root(),
      title = summarize_tool_activity(state.active_tool or data.toolName, data) or fallback_tool_activity_summary(state.active_tool or data.toolName, state.active_tool_detail),
      tool_name = sanitize_permission_text(state.active_tool or data.toolName),
      tool_call_id = tool_call_id,
      status = data.success == true and 'done' or 'blocked',
    })
    todo.parent_id = ensure_todo_root()
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

-- High-frequency event types that need no activity capture and minimal work.
-- Checked first to short-circuit before capture_turn_activity_summary,
-- log_debug_trace, and the long if-chain in handle_session_event.
local HIGH_FREQ_SKIP_CAPTURE = {
  ['assistant.message_delta'] = true,
  ['assistant.streaming_delta'] = true,
  ['assistant.reasoning_delta'] = true,
  ['session.background_tasks_changed'] = true,
  ['permission.requested'] = true,
  ['permission.completed'] = true,
}

local function handle_session_event(payload)
  local event_type = payload and payload.type or nil
  local data = payload and payload.data or {}

  -- Fast path for high-frequency events that don't need activity capture.
  if not HIGH_FREQ_SKIP_CAPTURE[event_type] then
    log_debug_trace('session.event received type=' .. tostring(event_type) .. ' data=', function()
      return sanitize_log_value(data)
    end, { max_len = 2400, depth = 8 })
    capture_turn_activity_summary(event_type, data)
  end

  if event_type == 'assistant.message_delta' then
    -- Only redraw statuslines on the busy state *transition* (false→true).
    -- Calling refresh_statuslines() on every token (potentially 50+/sec)
    -- causes continuous Neovim statusline redraws and is a primary CPU hotspot.
    local was_busy = state.chat_busy
    state.chat_busy = true
    if not was_busy then
      refresh_statuslines()
    end
    local pending_turn = state.pending_checkpoint_turn
    if not state.history_loading and pending_turn and pending_turn.session_id == state.session_id and type(data.messageId) == 'string' and data.messageId ~= '' then
      pending_turn.assistant_message_id = data.messageId
    end

    -- Drop duplicates / late replays of the same chunk by tracking each
    -- (messageId, messageChunkIndex) pair we've already applied. The server
    -- stamps chunkIndex monotonically per messageId in
    -- enrichSessionEventPayload (server/main.go) so any repeat is either a
    -- replay or an out-of-order resend that would corrupt the live draft.
    local message_id = type(data.messageId) == 'string' and data.messageId ~= '' and data.messageId or nil
    local chunk_index = tonumber(data.messageChunkIndex)
    if message_id and chunk_index then
      state.assistant_chunk_state = state.assistant_chunk_state or {}
      local bucket = state.assistant_chunk_state[message_id]
      if not bucket then
        bucket = { applied = {} }
        state.assistant_chunk_state[message_id] = bucket
      end
      if bucket.applied[chunk_index] then
        log(string.format('assistant.message_delta dropped duplicate chunk message_id=%s chunk_index=%d', message_id, chunk_index), vim.log.levels.DEBUG)
        return
      end
      bucket.applied[chunk_index] = true
    end

    local entry, idx, key = ensure_assistant_entry(data.messageId)
    local delta = data.deltaContent or ''

    -- Live assistant.message_delta display is append-only per the SDK event
    -- contract. Preserve the exact streamed chunk order/content in the
    -- transcript state and leave any final reconciliation to assistant.message.
    local previous_content = entry.content or ''
    entry._assistant_saw_delta = true
    if looks_like_tool_call_scaffolding(delta) or (previous_content == '' and looks_like_tool_call_scaffolding(previous_content .. delta)) then
      clear_leaked_tool_call_content(entry, idx, key, data.messageId, 'assistant.message_delta', previous_content)
      return
    end
    entry.content = previous_content .. delta
    clear_reasoning_preview_on_assistant_content(entry.content, 'assistant content started')
    window.sync_chat_markdown_conceal(state.chat_winid)
    if should_log(TRACE_LOG_LEVEL) then
      log(
        string.format(
          'assistant.message_delta appended message_id=%s key=%s idx=%s result_len=%d boundary={%s} content=%s',
          tostring(data.messageId or '<none>'),
          tostring(key or '<none>'),
          tostring(idx or '<none>'),
          #(entry.content or ''),
          preview_delta_boundary(previous_content, delta),
          preview_log_text(entry.content)
        ),
        TRACE_LOG_LEVEL
      )
    end
    if idx then
      stream_update(entry, idx)
    else
      schedule_render()
    end
    return
  end

  -- streaming_delta and reasoning_delta are the highest-frequency events
  -- (~20-40/sec during active streaming). Check them early to avoid falling
  -- through the entire if-chain below.
  if event_type == 'assistant.streaming_delta' then
    if should_log(TRACE_LOG_LEVEL) then
      log('assistant.streaming_delta ignored payload=' .. serialize_log_value(data, { max_len = 1600 }), TRACE_LOG_LEVEL)
    end
    return
  end

  if event_type == 'assistant.reasoning_delta' then
    local was_busy = state.chat_busy
    state.chat_busy = true
    if not was_busy then
      refresh_statuslines()
    end
    local _, _, key = ensure_assistant_entry(data.messageId)
    local delta = first_non_empty(data.deltaContent, data.content, data.delta, data.text) or ''
    if should_log(TRACE_LOG_LEVEL) then
      local reasoning_cfg = (((state.config or {}).chat or {}).reasoning or {})
      log(
        string.format(
          'reasoning_delta received message_id=%s key=%s len=%d chat_open=%s configured_enabled=%s reasoning_effort=%s stored_lines=%d text_len=%d',
          tostring(data.messageId or '<none>'),
          tostring(key or '<none>'),
          #delta,
          tostring(state.chat_bufnr ~= nil and vim.api.nvim_buf_is_valid(state.chat_bufnr)),
          tostring(reasoning_cfg.enabled),
          tostring(state.reasoning_effort or '<none>'),
          #(state.reasoning_lines or {}),
          #(state.reasoning_text or '')
        ),
        TRACE_LOG_LEVEL
      )
    end
    append_reasoning_delta(key, delta)
    return
  end

  if event_type == 'user.message' then
    -- Only add during history replay; during active conversation the input
    -- handler already called append_entry('user', ...) before sending.
    if state.history_loading then
      local content = type(data.content) == 'string' and data.content or ''
      if content ~= '' then
        local entry_index = append_entry('user', content)
        state.history_pending_user_entries[#state.history_pending_user_entries + 1] = {
          entry_index = entry_index,
          content = content,
        }
      end
    else
      log(string.format('user.message ignored during live session message_id=%s', tostring(data.messageId or '<none>')), vim.log.levels.DEBUG)
    end
    return
  end

  if event_type == 'assistant.message' then
    local entry, idx, key = ensure_assistant_entry(data.messageId)
    local first_message_for_entry = (entry.content or '') == ''
    local had_stream_start = state.stream_line_start ~= nil
    local has_tool_requests = type(data.toolRequests or data.ToolRequests) == 'table' and not vim.tbl_isempty(data.toolRequests or data.ToolRequests)
    local entry_content_changed = false
    if type(data.content) == 'string' then
      if state.history_loading then
        -- During history replay, skip the expensive merge logic (which uses
        -- O(n²) canonicalize/overlap checks on large strings).  The replayed
        -- assistant.message events already carry the final snapshot, so a
        -- simple assignment is correct and avoids pathological backtracking.
        if has_tool_requests and looks_like_tool_call_scaffolding(data.content) then
          clear_leaked_tool_call_content(entry, idx, key, data.messageId, 'assistant.message')
        else
          entry_content_changed = entry.content ~= data.content
          entry.content = data.content
        end
      else
        if not (state.chat_busy == true or (type(data.content) == 'string' and (data.content:match('^%s*\n') ~= nil or (entry.content or ''):match('\n%s*$') ~= nil))) then
          entry_content_changed = entry.content ~= data.content
          entry.content = data.content
          if has_tool_requests and looks_like_tool_call_scaffolding(entry.content or '') then
            clear_leaked_tool_call_content(entry, idx, key, data.messageId, 'assistant.message')
          else
            clear_reasoning_preview_on_assistant_content(entry.content, 'assistant content started')
          end
        else
          local merged_content = merge_assistant_message_content(entry.content, data.content, {
            message_id = data.messageId,
            entry_key = key,
            entry_index = idx,
            prefer_incoming_suffix_replacement = entry._assistant_saw_delta == true or had_stream_start,
          })
          if has_tool_requests and looks_like_tool_call_scaffolding(merged_content) then
            clear_leaked_tool_call_content(entry, idx, key, data.messageId, 'assistant.message')
          else
            entry_content_changed = entry.content ~= merged_content
            entry.content = merged_content
            clear_reasoning_preview_on_assistant_content(entry.content, 'assistant content started')
          end
        end
      end
    elseif has_tool_requests and looks_like_tool_call_scaffolding(entry.content or '') then
      clear_leaked_tool_call_content(entry, idx, key, data.messageId, 'assistant.message')
    end
    if entry_content_changed and not state.history_loading and idx then
      invalidate_frozen_render_from(idx)
    end
    entry._assistant_saw_delta = false
    if state.history_loading and first_message_for_entry then
      assign_history_checkpoint_id(data.messageId)
    elseif not state.history_loading then
      local pending_turn = state.pending_checkpoint_turn
      if pending_turn and pending_turn.session_id == state.session_id and type(data.messageId) == 'string' and data.messageId ~= '' then
        pending_turn.assistant_message_id = data.messageId
      end
    end
    log(
      string.format(
        'assistant.message preserving stream start message_id=%s idx=%s stream_line_start=%s had_stream_start=%s',
        tostring(data.messageId or '<none>'),
        tostring(idx or '<none>'),
        tostring(state.stream_line_start or '<none>'),
        tostring(had_stream_start)
      ),
      vim.log.levels.DEBUG
    )
    schedule_render()
    return
  end

  if handle_todo_event(event_type, data) then
    return
  end

  if event_type == 'assistant.turn_end' then
    if not state.history_loading then
      refresh_session_name_from_server(state.session_id)
      local pending_turn = state.pending_checkpoint_turn
      if pending_turn and pending_turn.session_id == state.session_id then
        log(
          string.format(
            'assistant.turn_end creating checkpoint session_id=%s entry_index=%s message_id=%s prompt=%s',
            tostring(state.session_id or '<none>'),
            tostring(pending_turn.entry_index or '<none>'),
            tostring(pending_turn.assistant_message_id or '<none>'),
            preview_log_text(pending_turn.prompt or '', TURN_END_PROMPT_PREVIEW_CHARS)
          ),
          vim.log.levels.DEBUG
        )
        state.pending_checkpoint_turn = nil
        state.pending_checkpoint_ops = state.pending_checkpoint_ops + 1
        checkpoints.create(state.session_id, pending_turn.prompt, function(checkpoint_err)
          state.pending_checkpoint_ops = math.max((tonumber(state.pending_checkpoint_ops) or 1) - 1, 0)
          if checkpoint_err then
            notify('Checkpoint unavailable: ' .. checkpoint_err, vim.log.levels.WARN)
          end
          if not checkpoint_err and pending_turn and pending_turn.session_id == state.session_id then
            checkpoint_diff.summarize_checkpoint_code_change(state.session_id)
          end
          refresh_statuslines()
          render_chat()
        end, {
          assistant_message_id = pending_turn.assistant_message_id,
          entry_index = pending_turn.entry_index,
        })
      end
    end
    finish_running_todos('done')
    clear_live_turn_state('turn end')
    -- The previous render may have frozen only the pre-assistant prefix while a
    -- checkpoint write was still pending. Force a full rebuild here so the
    -- completed assistant block stays visible even if a later tool/assistant
    -- event arrives before the checkpoint callback finishes.
    reset_frozen_render()
    render_chat() -- immediate full render on turn completion
    schedule_open_buffer_refresh()
    return
  end

  if event_type == 'assistant.turn_start' then
    if state.history_loading then
      state.history_replay_turn_index = (tonumber(state.history_replay_turn_index) or 0) + 1
    end
    local preserve_reasoning = preserve_reasoning_preview_on_late_turn_start()
    if not preserve_reasoning then
      clear_reasoning_preview('turn start')
    end
    reset_todo_state()
    state.recent_activity_lines = {}
    state.recent_activity_items = {}
    state.recent_activity_tool_calls = {}
    state.post_tool_use_hooks = {}
    if not preserve_reasoning then
      state.active_turn_assistant_index = nil
      state.live_assistant_entry_index = nil
      state.active_turn_assistant_message_id = nil
      state.active_assistant_merge_group = nil
      state.assistant_chunk_state = {}
    end
    if state.overlay_tool_display and state.overlay_tool_display.completed ~= true then
      complete_overlay_tool(state.overlay_tool_display.run_id)
    end
    state.active_tool = nil
    state.active_tool_run_id = nil
    state.active_tool_detail = nil
    state.current_intent = nil
    if not state.history_loading then
      refresh_statuslines()
      refresh_reasoning_overlay()
    end
    return
  end

  if event_type == 'tool.execution_partial_result' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    touch_overlay_tool_activity(overlay_run_id_for_tool_call(tool_call_id))
    return
  end

  if event_type == 'tool.execution_progress' then
    local tool_call_id = sanitize_permission_text(data.toolCallId)
    touch_overlay_tool_activity(overlay_run_id_for_tool_call(tool_call_id))
    return
  end

  if event_type == 'tool.execution_complete' then
    if handle_todo_event(event_type, data) then
      return
    end
    return
  end

  if event_type == 'hook.start' and data.hookType == 'postToolUse' then
    local hook_id = sanitize_permission_text(data.hookInvocationId)
    local hook_state = type(state.post_tool_use_hooks) == 'table' and hook_id and state.post_tool_use_hooks[hook_id] or nil
    local tool_name = first_non_empty(hook_state and hook_state.tool_name or nil, type(data.input) == 'table' and data.input.toolName or nil, state.active_tool)
    local run_id = overlay_run_id_for_tool_identity(hook_state and hook_state.tool_call_id or nil, tool_name)
    if hook_state then
      hook_state.run_id = run_id
    end
    log(
      string.format(
        'hook.start postToolUse hook=%s tool=%s run_id=%s payload=%s',
        tostring(hook_id or '<none>'),
        tostring(tool_name or '<none>'),
        tostring(run_id or '<none>'),
        serialize_log_value(data, { max_len = 1600 })
      ),
      vim.log.levels.DEBUG
    )
    begin_overlay_tool_post_result(run_id, hook_id)
    refresh_reasoning_overlay()
    return
  end

  if event_type == 'hook.end' and data.hookType == 'postToolUse' then
    local hook_id = sanitize_permission_text(data.hookInvocationId)
    local hook_state = type(state.post_tool_use_hooks) == 'table' and hook_id and state.post_tool_use_hooks[hook_id] or nil
    local tool_name = first_non_empty(hook_state and hook_state.tool_name or nil, state.active_tool)
    local run_id = overlay_run_id_for_tool_identity(hook_state and hook_state.tool_call_id or nil, tool_name)
    local overlay_result_text = nil
    if hook_state and hook_state.success == true then
      overlay_result_text = hook_state.result_text
    elseif hook_state and hook_state.error_message then
      overlay_result_text = 'postToolUse hook failed: ' .. hook_state.error_message
    end
    log(
      string.format(
        'hook.end postToolUse hook=%s tool=%s run_id=%s success=%s payload=%s',
        tostring(hook_id or '<none>'),
        tostring(tool_name or '<none>'),
        tostring(run_id or '<none>'),
        tostring(hook_state and hook_state.success or false),
        serialize_log_value(data, { max_len = 1600 })
      ),
      vim.log.levels.DEBUG
    )
    set_overlay_tool_result_text(run_id, overlay_result_text)
    complete_overlay_tool(run_id)
    if hook_id and type(state.post_tool_use_hooks) == 'table' then
      state.post_tool_use_hooks[hook_id] = nil
    end
    refresh_reasoning_overlay()
    return
  end

  if event_type == 'session.model_change' then
    sync_model_state(data.model or data.newModel, data.reasoningEffort or data.newReasoningEffort, data.sessionId or state.session_id)
    refresh_statuslines()
    return
  end

  if event_type == 'session.usage_info' then
    state.context_tokens = data.currentTokens
    state.context_limit = data.tokenLimit
    local quota_snapshots = data.quotaSnapshots or data.quota_snapshots
    if quota_snapshots then
      local quotas, primary = assistant_usage.normalize_quotas(quota_snapshots)
      if primary then
        state.last_assistant_usage_snapshot = {
          model = data.model or state.current_model or cfg.active_session_model(state.session_id),
          quotas = quotas,
          primary_quota = primary,
        }
      end
    end
    refresh_statuslines()
    return
  end

  if event_type == 'session.compaction_start' then
    handle_compaction_start(data)
    return
  end

  if event_type == 'session.compaction_complete' then
    handle_compaction_complete(data)
    return
  end

  if event_type == 'assistant.usage' then
    local usage = state.last_assistant_usage or assistant_usage.capture(data, { record_activity = false })
    local primary_quota = usage and usage.primary_quota or nil
    log(
      string.format(
        'assistant.usage model=%s cost=%s input=%s output=%s duration_ms=%s quota=%s payload=%s',
        tostring(usage and usage.model or data.model or '<none>'),
        tostring(usage and assistant_usage.format_decimal(usage.cost, 2) or assistant_usage.format_decimal(data.cost, 2) or '<none>'),
        tostring(usage and assistant_usage.format_summary_tokens(usage.input_tokens) or assistant_usage.format_summary_tokens(data.inputTokens) or '<none>'),
        tostring(usage and assistant_usage.format_summary_tokens(usage.output_tokens) or assistant_usage.format_summary_tokens(data.outputTokens) or '<none>'),
        tostring(usage and assistant_usage.format_decimal(usage.duration_ms, 0) or assistant_usage.format_decimal(data.duration, 0) or '<none>'),
        tostring(primary_quota and (primary_quota.display_name or primary_quota.id or '<none>') or '<none>'),
        serialize_log_value(data, { max_len = 1600 })
      ),
      vim.log.levels.DEBUG
    )
    refresh_statuslines()
    return
  end

  if event_type == 'system.notification' then
    local kind = data.kind or {}
    local key = first_non_empty(kind.agentId, kind.entryId)
    if not key then
      log('system.notification ignored without key payload=' .. serialize_log_value(data, { max_len = 1600 }), vim.log.levels.DEBUG)
      return
    end

    if kind.type == 'agent_idle' then
      set_background_task('background:' .. key, {
        kind = 'background',
        status = 'idle',
        title = first_non_empty(kind.description, kind.summary, kind.agentType, 'Background agent'),
        description = kind.description,
      })
    elseif kind.type == 'new_inbox_message' then
      set_background_task('background:' .. key, {
        kind = 'background',
        status = 'inbox',
        title = first_non_empty(kind.description, kind.summary, kind.agentType, 'Background agent'),
        description = kind.description,
      })
    elseif kind.type == 'agent_completed' then
      set_background_task('background:' .. key, {
        kind = 'background',
        status = kind.status == 'failed' and 'failed' or 'completed',
      })
      schedule_open_buffer_refresh()
    end
    return
  end

  if event_type == 'session.workspace_file_changed' then
    local op = data.operation or 'update'
    local rel_path = data.path or ''
    if rel_path == '' then
      log('session.workspace_file_changed ignored without path payload=' .. serialize_log_value(data, { max_len = 1600 }), vim.log.levels.DEBUG)
      return
    end

    -- Resolve to absolute path relative to working directory.
    local wd = working_directory()
    local abs_path = vim.fn.fnamemodify(wd .. '/' .. rel_path, ':p')

    state.pending_workspace_updates = state.pending_workspace_updates + 1
    refresh_statuslines()
    vim.schedule(function()
      -- Find any loaded buffer for this file.
      local bufnr_match = nil
      for _, b in ipairs(vim.api.nvim_list_bufs()) do
        if vim.api.nvim_buf_is_loaded(b) then
          local bname = vim.api.nvim_buf_get_name(b)
          if bname ~= '' and vim.fn.fnamemodify(bname, ':p') == abs_path then
            bufnr_match = b
            break
          end
        end
      end

      if op == 'create' then
        log(string.format('workspace change create path=%s abs=%s', rel_path, abs_path), vim.log.levels.DEBUG)
        notify('Agent created: ' .. rel_path, vim.log.levels.INFO)
      elseif op == 'update' and bufnr_match then
        local summary, reload_err = reload_buffer_from_disk(bufnr_match, abs_path)
        if reload_err == 'buffer has unsaved changes' then
          if confirm_external_buffer_reload() then
            summary, reload_err = reload_buffer_from_disk(bufnr_match, abs_path, { force = true })
          else
            reload_err = 'user declined reload'
          end
        end
        if reload_err then
          log(
            string.format('workspace change update reload skipped path=%s summary=%s error=%s', rel_path, tostring(summary or '<none>'), tostring(sanitize_reload_error(reload_err))),
            reload_attention_level(reload_err)
          )
          if reload_err ~= 'user declined reload' then
            notify(
              'Agent updated on disk: ' .. rel_path .. ' (' .. tostring(summary or 'content updated') .. '); reload skipped: ' .. tostring(sanitize_reload_error(reload_err)),
              reload_attention_level(reload_err)
            )
          end
        else
          log(string.format('workspace change update reloaded path=%s summary=%s', rel_path, tostring(summary or '<none>')), vim.log.levels.DEBUG)
          notify('Agent reloaded: ' .. rel_path .. ' (' .. tostring(summary or 'content updated') .. ')', vim.log.levels.INFO)
        end
        -- Offer diff review if the file is tracked by git.
        if state.config.chat.diff_review ~= false and not reload_err then
          offer_diff_review(abs_path, rel_path)
        end
      else
        log(string.format('workspace change update without open buffer path=%s abs=%s', rel_path, abs_path), vim.log.levels.DEBUG)
        notify('Agent updated: ' .. rel_path, vim.log.levels.INFO)
      end
      check_open_buffers_for_external_changes({ skip_path = abs_path })
      state.pending_workspace_updates = math.max((tonumber(state.pending_workspace_updates) or 1) - 1, 0)
      refresh_statuslines()
    end)
    return
  end

  if event_type == 'error' then
    log('session.error payload=' .. serialize_log_value(sanitize_log_value(data), { max_len = 2400, depth = 8 }), vim.log.levels.WARN)
    clear_live_turn_state('error', { clear_pending_checkpoint_turn = true })
    append_entry('error', vim.inspect(data))
    return
  end

  if should_log(vim.log.levels.DEBUG) then
    log('session.event unhandled type=' .. tostring(event_type) .. ' data=' .. serialize_log_value(sanitize_log_value(data), { max_len = 1600, depth = 8 }), vim.log.levels.DEBUG)
  end
end

local function flush_sse_event()
  local raw_data = table.concat(state.sse_event.data, '\n')
  local event_name = state.sse_event.event or 'message'
  state.sse_event = { event = 'message', data = {} }

  if raw_data == '' then
    return
  end

  log_session_event_payload(event_name, raw_data)
  local payload, decode_err = decode_json(raw_data, { log = false })
  if not payload then
    log(
      string.format('sse.event decode failed event=%s error=%s data=%s', tostring(event_name), tostring(decode_err), serialize_log_value(raw_data, { max_len = TRACE_LOG_MAX_CHARS })),
      vim.log.levels.WARN
    )
    append_entry('error', 'Failed to decode event ' .. event_name .. ': ' .. tostring(decode_err or raw_data))
    return
  end

  if event_name == 'session.event' then
    handle_session_event(payload)
    return
  end

  handle_host_event(event_name, payload)
end

local function consume_sse_line(line)
  if line == '' then
    flush_sse_event()
    return
  end

  if line:sub(1, 1) == ':' then
    return
  end

  local field, value = line:match('^([^:]+):%s?(.*)$')
  if not field then
    return
  end

  if field == 'event' then
    state.sse_event.event = value
  elseif field == 'data' then
    table.insert(state.sse_event.data, value)
  end
end

local function handle_sse_chunk(data)
  if not data or vim.tbl_isempty(data) then
    return
  end

  data[1] = state.sse_partial .. data[1]
  state.sse_partial = table.remove(data) or ''
  for _, line in ipairs(data) do
    consume_sse_line(line)
  end
end

local function history_replay_limits()
  local session_cfg = ((state.config or {}).session or {})
  local turn_limit = math.max(0, math.floor(tonumber(session_cfg.history_turn_limit) or 0))
  local activity_limit = math.max(0, math.floor(tonumber(session_cfg.history_activity_turn_limit) or 0))
  local preview_chars = math.max(0, math.floor(tonumber(session_cfg.history_preview_chars) or 0))
  if turn_limit > 0 and activity_limit > turn_limit then
    activity_limit = turn_limit
  end
  return turn_limit, activity_limit, preview_chars
end

function M.stop_event_stream()
  if state.events_job_id then
    log(string.format('sse.stream stop job_id=%s', tostring(state.events_job_id)), vim.log.levels.DEBUG)
    intentionally_stopped_event_jobs[state.events_job_id] = true
    pcall(vim.fn.jobstop, state.events_job_id)
    state.events_job_id = nil
  end
  state.sse_partial = ''
  state.sse_event = { event = 'message', data = {} }
end

function M.start_event_stream(session_id)
  M.stop_event_stream()
  state.sse_event = { event = 'message', data = {} }
  state.sse_partial = ''
  reset_live_activity_state()
  state.history_loading = true -- suppress rendering until host.history_done arrives
  state.history_replay_turn_index = 0
  state.history_pending_user_entries = {}

  if should_log(vim.log.levels.DEBUG) then
    notify(string.format('[%s] Starting SSE stream session=%s history=true', os.date('%H:%M:%S'), tostring(session_id)), vim.log.levels.DEBUG)
  end

  preload_history_checkpoint_ids(session_id)
  refresh_statuslines()

  -- Safety timeout: if host.history_done never arrives, force-clear
  -- history_loading so the UI doesn't stay frozen indefinitely.
  local HISTORY_TIMEOUT_MS = 120000 -- 2 minutes
  local history_timeout_session = session_id
  vim.defer_fn(function()
    if state.history_loading and state.session_id == history_timeout_session then
      log(string.format('history_loading safety timeout fired after %dms session_id=%s', HISTORY_TIMEOUT_MS, tostring(session_id)), vim.log.levels.WARN)
      state.history_loading = false
      state.history_replay_turn_index = 0
      state.history_checkpoint_ids = nil
      state.history_pending_user_entries = {}
      refresh_statuslines()
      reset_frozen_render()
      render_chat()
      scroll_to_bottom()
    end
  end, HISTORY_TIMEOUT_MS)

  local replay_permission_history = state.config.session and state.config.session.replay_permission_history == true
  local history_query = string.format('/sessions/%s/events?history=true', session_id)
  local history_turn_limit, history_activity_limit, history_preview_chars = history_replay_limits()
  if history_turn_limit > 0 then
    history_query = history_query .. '&history_turn_limit=' .. tostring(history_turn_limit)
  end
  if history_activity_limit > 0 then
    history_query = history_query .. '&history_activity_turn_limit=' .. tostring(history_activity_limit)
  end
  if history_preview_chars > 0 then
    history_query = history_query .. '&history_preview_chars=' .. tostring(history_preview_chars)
  end
  if replay_permission_history then
    history_query = history_query .. '&replay_permission_history=true'
  end
  local args = {
    state.config.curl_bin,
    '-sS',
    '-N',
    '-H',
    'Accept: text/event-stream',
    build_url(history_query),
  }
  log(string.format('sse.stream start session_id=%s url=%s', tostring(session_id or '<none>'), build_url(history_query)), vim.log.levels.DEBUG)

  -- Batch incoming SSE chunks to avoid flooding the Neovim event loop.
  -- on_stdout fires for every curl output chunk (potentially hundreds/sec);
  -- we accumulate chunks and drain them on a debounced timer.
  local uv = vim.uv or vim.loop
  local sse_batch = {}
  local sse_stderr = {}
  local sse_batch_timer = uv.new_timer()
  local sse_batch_pending = false
  local SSE_BATCH_MS = 50

  local function drain_sse_batch()
    sse_batch_pending = false
    local chunks = sse_batch
    sse_batch = {}
    local drain_t0 = os.clock()
    local chunk_count = #chunks
    for _, chunk in ipairs(chunks) do
      handle_sse_chunk(chunk)
    end
    local drain_elapsed_ms = (os.clock() - drain_t0) * 1000
    if drain_elapsed_ms > 20 then
      log(string.format('drain_sse_batch perf elapsed_ms=%.1f chunks=%d', drain_elapsed_ms, chunk_count), vim.log.levels.DEBUG)
    end
  end

  local job_id = vim.fn.jobstart(args, {
    stdout_buffered = false,
    stderr_buffered = false,
    on_stdout = function(_, data)
      table.insert(sse_batch, data)
      if not sse_batch_pending then
        sse_batch_pending = true
        sse_batch_timer:start(SSE_BATCH_MS, 0, vim.schedule_wrap(drain_sse_batch))
      end
    end,
    on_stderr = function(_, data)
      if data and not vim.tbl_isempty(data) then
        for _, line in ipairs(data) do
          if type(line) == 'string' and line ~= '' then
            table.insert(sse_stderr, line)
            log('sse.stream stderr line=' .. serialize_log_value(line, { max_len = 1000 }), vim.log.levels.WARN)
          end
        end
      end
    end,
    on_exit = function(exited_job_id, code)
      -- Drain any remaining chunks before handling exit.
      sse_batch_timer:stop()
      pcall(function()
        sse_batch_timer:close()
      end)
      vim.schedule(function()
        local stopped_intentionally = intentionally_stopped_event_jobs[exited_job_id] == true
        intentionally_stopped_event_jobs[exited_job_id] = nil
        -- Process any remaining buffered chunks.
        for _, chunk in ipairs(sse_batch) do
          handle_sse_chunk(chunk)
        end
        sse_batch = {}
        if state.events_job_id == exited_job_id then
          state.events_job_id = nil
        end
        M._handle_event_stream_exit(session_id, code, table.concat(sse_stderr, '\n'), {
          stopped_intentionally = stopped_intentionally,
        })
      end)
    end,
  })
  if job_id <= 0 then
    pcall(function()
      sse_batch_timer:close()
    end)
    state.history_loading = false
    state.history_pending_user_entries = {}
    log(string.format('sse.stream failed to start session_id=%s', tostring(session_id or '<none>')), vim.log.levels.WARN)
    append_entry('error', 'failed to start event stream')
    return
  end
  log(string.format('sse.stream started session_id=%s job_id=%s', tostring(session_id or '<none>'), tostring(job_id)), vim.log.levels.DEBUG)
  state.events_job_id = job_id
end

function M.reload_session_history(session_id, callback)
  callback = callback or function() end
  session_id = session_id or state.session_id
  if not session_id then
    callback('No active session')
    return
  end

  clear_transcript()
  state.history_loading = true
  state.history_pending_user_entries = {}
  preload_history_checkpoint_ids(session_id)
  request('GET', string.format('/sessions/%s/messages', session_id), nil, function(response, err)
    if err then
      log(string.format('session.history reload failed session_id=%s error=%s', tostring(session_id), serialize_log_value(err, { max_len = ERROR_LOG_MAX_CHARS })), vim.log.levels.WARN)
      state.history_loading = false
      state.history_replay_turn_index = 0
      state.history_checkpoint_ids = nil
      state.history_pending_user_entries = {}
      callback(err)
      return
    end

    for _, event in ipairs((response and response.events) or {}) do
      handle_session_event(event)
    end

    log(string.format('session.history reloaded session_id=%s event_count=%d', tostring(session_id), #((response and response.events) or {})), vim.log.levels.DEBUG)

    state.history_loading = false
    state.history_replay_turn_index = 0
    state.history_checkpoint_ids = nil
    state.history_pending_user_entries = {}
    reset_frozen_render()
    render_chat()
    scroll_to_bottom()
    callback(nil, #((response and response.events) or {}))
  end)
end

M.show_user_input_picker = show_user_input_picker
M.handle_user_input = handle_user_input
M.handle_host_event = handle_host_event
M.check_open_buffers_for_external_changes = check_open_buffers_for_external_changes
M.confirm_external_buffer_reload = confirm_external_buffer_reload
M.remember_buffer_disk_state = remember_buffer_disk_state
M.remember_open_buffer_disk_state = remember_open_buffer_disk_state
M.forget_buffer_disk_state = forget_buffer_disk_state
M.offer_diff_review = offer_diff_review
-- Expose checkpoint diff review helper so higher-level modules and tests can call it
M.review_diff = checkpoint_diff.review_diff
M.handle_session_event = handle_session_event
M.flush_sse_event = flush_sse_event
M.consume_sse_line = consume_sse_line
M.handle_sse_chunk = handle_sse_chunk

return M
