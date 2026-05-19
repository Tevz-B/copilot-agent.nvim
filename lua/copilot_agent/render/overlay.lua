-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.
--
-- render/overlay.lua – Reasoning overlay, activity overlay, virtual lines.
-- Extracted from render.lua to reduce file size.
--
-- Usage: local overlay = require('copilot_agent.render.overlay')
--        overlay.setup(M, ctx)  -- called once from render/init.lua

local uv = vim.uv or vim.loop
local cfg = require('copilot_agent.config')
local utils = require('copilot_agent.utils')
local state = cfg.state
local log = cfg.log

local sanitize_display_text = function(text)
  return utils.tilde_home_path(utils.normalize_display_text(text))
end

local M = {}

-- ── Constants ─────────────────────────────────────────────────────────────────

local DEFAULT_REASONING_MAX_LINES = 5
local MAX_REASONING_PREVIEW_LINES = 20
local OVERLAY_WRAP_MIN_WIDTH = 20
local OVERLAY_WRAP_FALLBACK_WIDTH = 80
local OVERLAY_MIN_WINDOW_WIDTH = 24
local OVERLAY_HORIZONTAL_PADDING = 4
local OVERLAY_BREAK_THRESHOLD_RATIO = 0.3
local OVERLAY_STRONG_HL = 'CopilotAgentOverlayStrong'
local OVERLAY_EMPHASIS_HL = 'CopilotAgentOverlayEmphasis'
local OVERLAY_CODE_HL = 'CopilotAgentOverlayCode'
local OVERLAY_QUOTED_HL = 'CopilotAgentOverlayQuoted'
local OVERLAY_TAIL_SPACER_LINES = 3
local OVERLAY_BOTTOM_GUTTER_MIN_LINES = 5
local REASONING_DEBOUNCE_MS = 80
local REASONING_NS = vim.api.nvim_create_namespace('copilot_agent_reasoning')

-- ── Reasoning config ──────────────────────────────────────────────────────────

function M.reasoning_config()
  local reasoning = (((state.config or {}).chat or {}).reasoning or {})
  local enabled
  if reasoning.enabled == nil then
    enabled = #(state.reasoning_lines or {}) > 0 or (type(state.reasoning_effort) == 'string' and state.reasoning_effort ~= '')
  else
    enabled = reasoning.enabled == true
  end
  local max_lines = tonumber(reasoning.max_lines) or DEFAULT_REASONING_MAX_LINES
  max_lines = math.max(1, math.min(MAX_REASONING_PREVIEW_LINES, math.floor(max_lines)))
  return enabled, max_lines
end

function M.normalize_reasoning_lines(text)
  if type(text) ~= 'string' or text == '' then
    return {}
  end
  local lines = vim.split(sanitize_display_text(text):gsub('\r\n?', '\n'), '\n', { plain = true })
  local filtered = {}
  for _, line in ipairs(lines) do
    if line ~= '' then
      filtered[#filtered + 1] = line
    end
  end
  return filtered
end

function M.reasoning_lines(max_lines)
  local lines = vim.deepcopy(state.reasoning_lines or {})
  local non_empty = {}
  for _, line in ipairs(lines) do
    line = sanitize_display_text(line)
    if line ~= '' then
      non_empty[#non_empty + 1] = line
    end
  end
  lines = non_empty
  if max_lines == nil then
    max_lines = select(2, M.reasoning_config())
  else
    max_lines = math.max(1, math.floor(tonumber(max_lines) or DEFAULT_REASONING_MAX_LINES))
  end
  if #lines <= max_lines then
    return lines
  end
  local trimmed = {}
  for i = #lines - max_lines + 1, #lines do
    trimmed[#trimmed + 1] = lines[i]
  end
  return trimmed
end

-- ── Overlay text utilities ────────────────────────────────────────────────────

function M.wrap_overlay_text(text, max_width, opts)
  opts = type(opts) == 'table' and opts or {}
  local collapse_whitespace = opts.collapse_whitespace ~= false
  local trim_chunks = opts.trim_chunks ~= false
  local min_width = math.max(1, math.floor(tonumber(opts.min_width) or OVERLAY_WRAP_MIN_WIDTH))
  text = type(text) == 'string' and text or ''
  if collapse_whitespace then
    text = text:gsub('%s+', ' ')
  end
  if trim_chunks then
    text = vim.trim(text)
  end
  if text == '' then
    return {}
  end
  max_width = math.max(min_width, math.floor(tonumber(max_width) or OVERLAY_WRAP_FALLBACK_WIDTH))
  if vim.fn.strdisplaywidth(text) <= max_width then
    return { text }
  end
  local wrapped = {}
  local pos = 1
  while pos <= #text do
    local chunk = text:sub(pos, pos + max_width - 1)
    if pos + max_width - 1 < #text then
      local break_at = chunk:find('%s[^%s]*$')
      if break_at and break_at > math.floor(max_width * OVERLAY_BREAK_THRESHOLD_RATIO) then
        chunk = chunk:sub(1, break_at - 1)
      end
    end
    wrapped[#wrapped + 1] = trim_chunks and vim.trim(chunk) or chunk
    pos = pos + #chunk
    while pos <= #text and text:sub(pos, pos) == ' ' do
      pos = pos + 1
    end
  end
  return wrapped
end

function M.activity_overlay_width()
  local win = state.chat_winid
  if win and vim.api.nvim_win_is_valid(win) then
    return math.max(OVERLAY_MIN_WINDOW_WIDTH, vim.api.nvim_win_get_width(win) - OVERLAY_HORIZONTAL_PADDING)
  end
  return OVERLAY_WRAP_FALLBACK_WIDTH
end

function M.tool_is_displayable_in_overlay(name)
  if type(name) ~= 'string' or name == '' then
    return false
  end
  local normalized = vim.trim(name):lower()
  if normalized == 'bash' or normalized == 'sh' or normalized == 'zsh' or normalized == 'fish' or normalized == 'pwsh' or normalized == 'powershell' or normalized == 'cmd' then
    return true
  end
  return false
end

local function tail_lines(lines, max_lines)
  max_lines = math.max(0, math.floor(tonumber(max_lines) or 0))
  if max_lines <= 0 or #lines == 0 then
    return {}
  end
  if #lines <= max_lines then
    return lines
  end
  local trimmed = {}
  for idx = #lines - max_lines + 1, #lines do
    trimmed[#trimmed + 1] = lines[idx]
  end
  return trimmed
end

local function normalize_activity_overlay_result_lines(text)
  text = type(text) == 'string' and text:gsub('\r\n?', '\n') or ''
  if text == '' then
    return {}
  end
  local lines = {}
  for _, line in ipairs(vim.split(text, '\n', { plain = true })) do
    line = sanitize_display_text(line)
    if line ~= '' then
      lines[#lines + 1] = line
    end
  end
  return lines
end

local function wrap_activity_overlay_result_lines(lines)
  local wrapped = {}
  local max_w = M.activity_overlay_width()
  for _, line in ipairs(lines or {}) do
    local segments = M.wrap_overlay_text(line, max_w, {
      collapse_whitespace = false,
      min_width = 1,
      trim_chunks = true,
    })
    if #segments == 0 then
      segments = { '' }
    end
    for _, segment in ipairs(segments) do
      wrapped[#wrapped + 1] = segment
    end
  end
  return wrapped
end

function M.activity_overlay_lines(max_lines)
  local overlay_tool = state.overlay_tool_display
  if type(overlay_tool) ~= 'table' or not M.tool_is_displayable_in_overlay(overlay_tool.tool) then
    return {}
  end

  local tool = sanitize_display_text(overlay_tool.tool)
  local detail = sanitize_display_text(overlay_tool.detail)
  local line = tool
  if type(detail) == 'string' and detail ~= '' and detail ~= tool then
    line = line .. ' — ' .. detail
  end
  line = type(line) == 'string' and vim.trim(line:gsub('%s+', ' ')) or ''
  if line == '' then
    return {}
  end
  local prefix = '🔧 '
  local max_w = M.activity_overlay_width() - vim.fn.strdisplaywidth(prefix)
  local wrapped = M.wrap_overlay_text(line, max_w)
  if #wrapped == 0 then
    return {}
  end
  wrapped[1] = prefix .. wrapped[1]

  local result_text = overlay_tool.post_tool_use_pending == true and nil or overlay_tool.result_text
  local result_lines = wrap_activity_overlay_result_lines(normalize_activity_overlay_result_lines(result_text))
  if type(max_lines) == 'number' then
    max_lines = math.max(1, math.floor(max_lines))
    if #wrapped >= max_lines then
      return { unpack(wrapped, 1, max_lines) }
    end
    result_lines = tail_lines(result_lines, max_lines - #wrapped)
  end

  return vim.list_extend(wrapped, result_lines)
end

-- ── Overlay markup (bold, italic, code, quotes) ───────────────────────────────

local function append_overlay_chunk(chunks, text, highlight)
  if type(text) ~= 'string' or text == '' then
    return
  end
  if #chunks > 0 and chunks[#chunks][2] == highlight then
    chunks[#chunks][1] = chunks[#chunks][1] .. text
    return
  end
  chunks[#chunks + 1] = { text, highlight }
end

local function find_unescaped_sequence(text, sequence, start_pos)
  start_pos = math.max(1, math.floor(tonumber(start_pos) or 1))
  while start_pos <= #text do
    local found = text:find(sequence, start_pos, true)
    if not found then
      return nil
    end
    local escapes = 0
    local idx = found - 1
    while idx >= 1 and text:sub(idx, idx) == '\\' do
      escapes = escapes + 1
      idx = idx - 1
    end
    if escapes % 2 == 0 then
      return found
    end
    start_pos = found + 1
  end
  return nil
end

local function valid_overlay_emphasis_inner(inner)
  return type(inner) == 'string' and inner ~= '' and inner:find('^%s') == nil and inner:find('%s$') == nil
end

local function overlay_markup_span_at(text, pos)
  local pair = text:sub(pos, pos + 1)
  if pair == '**' or pair == '__' then
    local closing = find_unescaped_sequence(text, pair, pos + 2)
    while closing do
      if valid_overlay_emphasis_inner(text:sub(pos + 2, closing - 1)) then
        return closing + 1, OVERLAY_STRONG_HL
      end
      closing = find_unescaped_sequence(text, pair, closing + 2)
    end
  end

  local char = text:sub(pos, pos)
  if char == '*' or char == '_' then
    if text:sub(pos - 1, pos - 1) == char or text:sub(pos + 1, pos + 1) == char then
      return nil
    end
    local closing = find_unescaped_sequence(text, char, pos + 1)
    while closing do
      if text:sub(closing - 1, closing - 1) ~= char and text:sub(closing + 1, closing + 1) ~= char then
        if valid_overlay_emphasis_inner(text:sub(pos + 1, closing - 1)) then
          return closing, OVERLAY_EMPHASIS_HL
        end
      end
      closing = find_unescaped_sequence(text, char, closing + 1)
    end
    return nil
  end

  if char == '`' then
    local closing = find_unescaped_sequence(text, char, pos + 1)
    if closing then
      return closing, OVERLAY_CODE_HL
    end
    return nil
  end

  if char == '"' then
    local closing = find_unescaped_sequence(text, char, pos + 1)
    if closing then
      return closing, OVERLAY_QUOTED_HL
    end
    return nil
  end

  if char == "'" then
    local prev = text:sub(pos - 1, pos - 1)
    local next_char = text:sub(pos + 1, pos + 1)
    if prev:match('[%w_]') and next_char:match('[%w_]') then
      return nil
    end
    local closing = find_unescaped_sequence(text, char, pos + 1)
    if closing then
      return closing, OVERLAY_QUOTED_HL
    end
  end

  return nil
end

function M.overlay_markup_chunks(text, base_highlight)
  text = type(text) == 'string' and text or ''
  if text == '' then
    return {}
  end

  local chunks = {}
  local plain_start = 1
  local cursor = 1
  while cursor <= #text do
    local span_end, highlight = overlay_markup_span_at(text, cursor)
    if span_end then
      if plain_start < cursor then
        append_overlay_chunk(chunks, text:sub(plain_start, cursor - 1), base_highlight)
      end
      append_overlay_chunk(chunks, text:sub(cursor, span_end), highlight)
      cursor = span_end + 1
      plain_start = cursor
    else
      cursor = cursor + 1
    end
  end
  if plain_start <= #text then
    append_overlay_chunk(chunks, text:sub(plain_start), base_highlight)
  end
  return chunks
end

function M.append_overlay_section(virt_lines, lines, first_prefix, other_prefix, highlight, right_align)
  local display_lines = {}
  for idx, line in ipairs(lines) do
    local prefix = idx == 1 and first_prefix or other_prefix
    display_lines[#display_lines + 1] = prefix .. line
  end
  lines = display_lines

  local win_width
  if right_align then
    local win = state.chat_winid
    win_width = (win and vim.api.nvim_win_is_valid(win)) and vim.api.nvim_win_get_width(win) or 80
  end
  for _, display_text in ipairs(lines) do
    local line_chunks = M.overlay_markup_chunks(display_text, highlight)
    if right_align and win_width then
      local text_width = vim.fn.strdisplaywidth(display_text)
      local pad = math.max(0, win_width - text_width - 1)
      local padded_chunks = { { string.rep(' ', pad), '' } }
      for _, chunk in ipairs(line_chunks) do
        padded_chunks[#padded_chunks + 1] = chunk
      end
      virt_lines[#virt_lines + 1] = padded_chunks
    else
      virt_lines[#virt_lines + 1] = line_chunks
    end
  end
end

local function reasoning_overlay_lines(lines)
  local display_lines = {}
  local first_prefix = '  Reasoning: '
  local other_prefix = '             '
  local prefix_width = vim.fn.strdisplaywidth(first_prefix)
  local max_width = math.max(1, M.activity_overlay_width() - prefix_width)

  for _, line in ipairs(lines or {}) do
    local wrapped = M.wrap_overlay_text(line, max_width, {
      collapse_whitespace = false,
      min_width = 1,
      trim_chunks = true,
    })
    if #wrapped == 0 then
      wrapped = { '' }
    end
    for _, chunk in ipairs(wrapped) do
      local prefix = #display_lines == 0 and first_prefix or other_prefix
      display_lines[#display_lines + 1] = prefix .. chunk
    end
  end

  return display_lines
end

function M.reasoning_virtual_lines(task_lines, reasoning_lines_arg)
  local virt_lines = {}
  M.append_overlay_section(virt_lines, task_lines, '  Activity: ', '            ', 'CopilotAgentActivity', false)
  M.append_overlay_section(virt_lines, reasoning_overlay_lines(reasoning_lines_arg), '', '', 'CopilotAgentReasoning', false)
  return virt_lines
end

-- ── Overlay rendering and lifecycle ───────────────────────────────────────────

function M.overlay_tail_spacer_lines()
  return math.max(0, math.floor(tonumber(state.chat_tail_spacer_lines) or 0))
end

function M.overlay_bottom_padding(task_line_count, reasoning_line_count)
  local total = math.max(0, math.floor(tonumber(task_line_count) or 0) + math.floor(tonumber(reasoning_line_count) or 0))
  if total <= 0 then
    return 0
  end
  local winid = state.chat_winid
  local win_height = (winid and vim.api.nvim_win_is_valid(winid)) and vim.api.nvim_win_get_height(winid) or 0
  if win_height <= 1 then
    return total
  end
  return math.max(1, math.min(win_height - 1, math.max(OVERLAY_BOTTOM_GUTTER_MIN_LINES, total)))
end

function M.overlay_bottom_topline(line_count, win_height, padding)
  line_count = math.max(1, math.floor(tonumber(line_count) or 1))
  win_height = math.max(1, math.floor(tonumber(win_height) or 1))
  padding = math.max(0, math.floor(tonumber(padding) or 0))
  if padding <= 0 then
    return math.max(1, line_count - win_height + 1)
  end
  local visible_height = math.max(1, win_height - padding)
  return math.max(1, line_count - visible_height + 1)
end

local function overlay_now_ms()
  if uv and uv.hrtime then
    return math.floor(uv.hrtime() / 1e6)
  end
  return math.floor(vim.loop.hrtime() / 1e6)
end

local function overlay_anchor(bufnr)
  local line_count = vim.api.nvim_buf_line_count(bufnr)
  return math.max(line_count - 1, 0), false
end

local function clear_reasoning_overlay()
  local bufnr = state.chat_bufnr
  if bufnr and vim.api.nvim_buf_is_valid(bufnr) then
    vim.api.nvim_buf_clear_namespace(bufnr, REASONING_NS, 0, -1)
  end
end

local function sync_chat_tail_spacer_lines(bufnr, desired_count, rendered_content_line_count_fn, chat_view_log_summary_fn)
  local previous_count = M.overlay_tail_spacer_lines()
  desired_count = math.max(0, math.floor(tonumber(desired_count) or 0))
  state.chat_tail_spacer_lines = desired_count
  if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
    return
  end

  local content_end = rendered_content_line_count_fn(bufnr)
  local spacer_lines = {}
  for _ = 1, desired_count do
    spacer_lines[#spacer_lines + 1] = ''
  end

  vim.bo[bufnr].modifiable = true
  vim.bo[bufnr].readonly = false
  vim.api.nvim_buf_set_lines(bufnr, content_end, -1, false, spacer_lines)
  vim.bo[bufnr].modifiable = false
  vim.bo[bufnr].readonly = true
  vim.bo[bufnr].modified = false
  if previous_count ~= desired_count then
    log(
      string.format(
        'reasoning overlay tail spacers updated previous=%d current=%d content_end=%d line_count=%d %s',
        previous_count,
        desired_count,
        content_end,
        vim.api.nvim_buf_line_count(bufnr),
        chat_view_log_summary_fn()
      ),
      vim.log.levels.TRACE
    )
  end
end

-- Timer state for debounced overlay refresh
local reasoning_timer = uv.new_timer()
local reasoning_refresh_pending = false
local reasoning_last_refresh_ms = 0

--- Setup the overlay refresh system. Called by render/init.lua with callbacks.
--- @param render_M table The main render module table
--- @param deps table { rendered_content_line_count, chat_view_log_summary, refresh_statuslines }
function M.setup(render_M, deps)
  local rendered_content_line_count_fn = deps.rendered_content_line_count
  local chat_view_log_summary_fn = deps.chat_view_log_summary
  local refresh_statuslines = deps.refresh_statuslines

  local function update_reasoning_overlay_now()
    local bufnr = state.chat_bufnr
    if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
      log(
        string.format(
          'reasoning overlay skipped chat buffer unavailable bufnr=%s winid=%s text_len=%d stored_lines=%d',
          tostring(bufnr),
          tostring(state.chat_winid),
          #(state.reasoning_text or ''),
          #(state.reasoning_lines or {})
        ),
        vim.log.levels.TRACE
      )
      return
    end

    local had_overlay = #vim.api.nvim_buf_get_extmarks(bufnr, REASONING_NS, 0, -1, { limit = 1 }) > 0
    clear_reasoning_overlay()

    local enabled, max_lines = M.reasoning_config()
    if state.history_loading then
      state.overlay_tool_schedule_token = (tonumber(state.overlay_tool_schedule_token) or 0) + 1
      state.overlay_tool_display = nil
      log(
        string.format(
          'reasoning overlay skipped enabled=%s history_loading=%s text_len=%d stored_lines=%d %s',
          tostring(enabled),
          tostring(state.history_loading),
          #(state.reasoning_text or ''),
          #(state.reasoning_lines or {}),
          chat_view_log_summary_fn()
        ),
        vim.log.levels.TRACE
      )
      return
    end

    local task_lines = M.activity_overlay_lines(max_lines)
    local r_lines = enabled and M.reasoning_lines(max_lines) or {}
    local rendered_reasoning_lines = reasoning_overlay_lines(r_lines)
    if #task_lines == 0 and #rendered_reasoning_lines == 0 then
      sync_chat_tail_spacer_lines(bufnr, 0, rendered_content_line_count_fn, chat_view_log_summary_fn)
      if had_overlay then
        render_M.release_overlay_gutter()
      end
      log(
        string.format(
          'reasoning overlay skipped no lines enabled=%s had_overlay=%s text_len=%d stored_lines=%d activity=%d rendered_reasoning=%d %s',
          tostring(enabled),
          tostring(had_overlay),
          #(state.reasoning_text or ''),
          #(state.reasoning_lines or {}),
          #task_lines,
          #rendered_reasoning_lines,
          chat_view_log_summary_fn()
        ),
        vim.log.levels.TRACE
      )
      return
    end

    sync_chat_tail_spacer_lines(bufnr, state.chat_busy and OVERLAY_TAIL_SPACER_LINES or 0, rendered_content_line_count_fn, chat_view_log_summary_fn)
    local padding = M.overlay_bottom_padding(#task_lines, #rendered_reasoning_lines)
    render_M.reserve_overlay_gutter(#task_lines, #rendered_reasoning_lines)
    local anchor_row, anchor_above = overlay_anchor(bufnr)
    local extmark_id = vim.api.nvim_buf_set_extmark(bufnr, REASONING_NS, anchor_row, 0, {
      virt_lines = M.reasoning_virtual_lines(task_lines, r_lines),
      virt_lines_leftcol = true,
      virt_lines_above = anchor_above,
    })
    log(
      string.format(
        'reasoning overlay updated extmark=%s activity=%d reasoning=%d padding=%d anchor_row=%d above=%s text_len=%d stored_lines=%d %s',
        tostring(extmark_id),
        #task_lines,
        #rendered_reasoning_lines,
        padding,
        anchor_row,
        tostring(anchor_above),
        #(state.reasoning_text or ''),
        #(state.reasoning_lines or {}),
        chat_view_log_summary_fn()
      ),
      vim.log.levels.DEBUG
    )
  end

  function render_M.refresh_reasoning_overlay(immediate)
    if immediate then
      log(
        string.format(
          'reasoning overlay refresh immediate text_len=%d stored_lines=%d pending=%s %s',
          #(state.reasoning_text or ''),
          #(state.reasoning_lines or {}),
          tostring(reasoning_refresh_pending),
          chat_view_log_summary_fn()
        ),
        vim.log.levels.DEBUG
      )
      reasoning_timer:stop()
      reasoning_refresh_pending = false
      reasoning_last_refresh_ms = overlay_now_ms()
      update_reasoning_overlay_now()
      return
    end

    local now = overlay_now_ms()
    local elapsed = now - (reasoning_last_refresh_ms or 0)
    if not reasoning_refresh_pending and (reasoning_last_refresh_ms == 0 or elapsed >= REASONING_DEBOUNCE_MS) then
      log(
        string.format(
          'reasoning overlay refresh scheduled now elapsed=%d text_len=%d stored_lines=%d %s',
          elapsed,
          #(state.reasoning_text or ''),
          #(state.reasoning_lines or {}),
          chat_view_log_summary_fn()
        ),
        vim.log.levels.DEBUG
      )
      reasoning_last_refresh_ms = now
      vim.schedule(update_reasoning_overlay_now)
      return
    end

    local delay = math.max(1, REASONING_DEBOUNCE_MS - elapsed)
    log(
      string.format(
        'reasoning overlay refresh delayed delay=%d elapsed=%d pending=%s text_len=%d stored_lines=%d %s',
        delay,
        elapsed,
        tostring(reasoning_refresh_pending),
        #(state.reasoning_text or ''),
        #(state.reasoning_lines or {}),
        chat_view_log_summary_fn()
      ),
      vim.log.levels.DEBUG
    )
    reasoning_refresh_pending = true
    reasoning_timer:stop()
    reasoning_timer:start(
      delay,
      0,
      vim.schedule_wrap(function()
        reasoning_refresh_pending = false
        reasoning_last_refresh_ms = overlay_now_ms()
        update_reasoning_overlay_now()
      end)
    )
  end

  function render_M.clear_reasoning_preview(reason)
    local had_reasoning = (state.reasoning_text or '') ~= '' or #(state.reasoning_lines or {}) > 0
    local previous_key = state.reasoning_entry_key
    local previous_text_len = #(state.reasoning_text or '')
    local previous_line_count = #(state.reasoning_lines or {})
    state.reasoning_entry_key = nil
    state.reasoning_text = ''
    state.reasoning_lines = {}
    refresh_statuslines()
    render_M.refresh_reasoning_overlay(true)
    if had_reasoning then
      log(
        string.format(
          'reasoning preview cleared (%s) key=%s text_len=%d lines=%d %s',
          tostring(reason or 'unspecified'),
          tostring(previous_key or '<none>'),
          previous_text_len,
          previous_line_count,
          chat_view_log_summary_fn()
        ),
        vim.log.levels.DEBUG
      )
    end
  end

  function render_M.append_reasoning_delta(entry_key, delta)
    if type(delta) ~= 'string' or delta == '' then
      log('reasoning delta ignored because it was empty', vim.log.levels.DEBUG)
      return
    end
    if type(entry_key) == 'string' and entry_key ~= '' and state.reasoning_entry_key ~= entry_key then
      state.reasoning_text = ''
    end
    state.reasoning_entry_key = entry_key or state.reasoning_entry_key
    state.reasoning_text = (state.reasoning_text or '') .. delta
    state.reasoning_lines = M.normalize_reasoning_lines(state.reasoning_text)
    log(
      string.format(
        'reasoning delta appended key=%s chunk_len=%d total_len=%d lines=%d refresh_pending=%s',
        tostring(state.reasoning_entry_key or '<none>'),
        #delta,
        #(state.reasoning_text or ''),
        #(state.reasoning_lines or {}),
        tostring(reasoning_refresh_pending)
      ),
      vim.log.levels.DEBUG
    )
    render_M.refresh_reasoning_overlay()
    refresh_statuslines()
  end

  function render_M.reasoning_lines(max_lines_arg)
    return M.reasoning_lines(max_lines_arg)
  end

  function render_M.overlay_bottom_padding(task_line_count, reasoning_line_count)
    return M.overlay_bottom_padding(task_line_count, reasoning_line_count)
  end

  function render_M.overlay_bottom_topline(line_count, win_height, padding)
    return M.overlay_bottom_topline(line_count, win_height, padding)
  end
end

return M
