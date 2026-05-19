-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.
--
-- render/scroll.lua – Chat window scroll/follow/jump helpers.

local cfg = require('copilot_agent.config')
local state = cfg.state
local log = cfg.log

local M = {}

local CHAT_SCROLL_GUARD_MS = 80

-- ── Internal helpers ──────────────────────────────────────────────────────────

local function chat_window_matches_buffer(winid, bufnr)
  if not winid or not vim.api.nvim_win_is_valid(winid) then
    return false
  end
  if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
    return false
  end
  return vim.api.nvim_win_get_buf(winid) == bufnr
end

function M.resolve_chat_window_and_buffer()
  local bufnr = state.chat_bufnr
  if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
    state.chat_winid = nil
    return nil, nil
  end

  local winid = state.chat_winid
  if chat_window_matches_buffer(winid, bufnr) then
    return winid, bufnr
  end

  local current_tab = vim.api.nvim_get_current_tabpage()
  local windows = vim.fn.win_findbuf(bufnr)
  for _, candidate in ipairs(windows) do
    if chat_window_matches_buffer(candidate, bufnr) and vim.api.nvim_win_get_tabpage(candidate) == current_tab then
      state.chat_winid = candidate
      return candidate, bufnr
    end
  end
  for _, candidate in ipairs(windows) do
    if chat_window_matches_buffer(candidate, bufnr) then
      state.chat_winid = candidate
      return candidate, bufnr
    end
  end

  state.chat_winid = nil
  return nil, bufnr
end

function M.current_chat_view()
  local winid = select(1, M.resolve_chat_window_and_buffer())
  if not winid then
    return nil
  end
  local info = vim.fn.getwininfo(winid)
  return info and info[1] or nil
end

function M.chat_text_height_from_topline(winid, bufnr, topline)
  if not bufnr or not vim.api.nvim_buf_is_valid(bufnr) then
    return 0
  end

  local line_count = math.max(1, vim.api.nvim_buf_line_count(bufnr))
  local start_row = math.max(0, math.min(line_count - 1, math.floor(tonumber(topline) or 1) - 1))
  if not chat_window_matches_buffer(winid, bufnr) then
    return math.max(0, line_count - start_row)
  end

  local ok, info = pcall(vim.api.nvim_win_text_height, winid, {
    start_row = start_row,
    end_row = line_count - 1,
  })
  if not ok then
    log(string.format('chat text height fallback win=%s buf=%s start_row=%d line_count=%d error=%s', tostring(winid), tostring(bufnr), start_row, line_count, tostring(info)), vim.log.levels.DEBUG)
    return math.max(0, line_count - start_row)
  end
  if type(info) ~= 'table' or type(info.all) ~= 'number' then
    return math.max(0, line_count - start_row)
  end
  return math.max(0, math.floor(info.all + (tonumber(info.fill) or 0)))
end

function M.chat_view_metrics(winid, bufnr, topline, padding)
  local line_count = math.max(1, vim.api.nvim_buf_line_count(bufnr))
  local current_topline = math.max(1, math.min(math.floor(tonumber(topline) or 1), line_count))
  local win_height = math.max(1, vim.api.nvim_win_get_height(winid))
  padding = math.max(0, math.floor(tonumber(padding) or 0))
  local visible_height = math.max(1, win_height - padding)
  local buffer_rows = math.max(0, line_count - current_topline + 1)
  local content_rows = M.chat_text_height_from_topline(winid, bufnr, current_topline)
  return {
    topline = current_topline,
    line_count = line_count,
    win_height = win_height,
    padding = padding,
    visible_height = visible_height,
    content_rows = content_rows,
    buffer_rows = buffer_rows,
    spare_rows = win_height - content_rows,
    wrapped_rows = math.max(0, content_rows - buffer_rows),
  }
end

function M.chat_view_metrics_summary(metrics)
  if type(metrics) ~= 'table' then
    return 'metrics=<nil>'
  end
  return string.format(
    'content_rows=%d visible_rows=%d spare_rows=%d buffer_rows=%d wrapped_rows=%d',
    math.floor(tonumber(metrics.content_rows) or 0),
    math.floor(tonumber(metrics.visible_height) or 0),
    math.floor(tonumber(metrics.spare_rows) or 0),
    math.floor(tonumber(metrics.buffer_rows) or 0),
    math.floor(tonumber(metrics.wrapped_rows) or 0)
  )
end

function M.chat_view_log_summary()
  local winid = state.chat_winid
  if not winid or not vim.api.nvim_win_is_valid(winid) then
    return 'view=<invalid>'
  end
  local info = vim.fn.getwininfo(winid)
  local view = info and info[1] or nil
  if not view then
    return 'view=<unknown>'
  end
  return string.format('top=%s bot=%s height=%s', tostring(view.topline), tostring(view.botline), tostring(view.height))
end

function M.target_topline_for_padding(winid, bufnr, padding, min_topline)
  local line_count = math.max(1, vim.api.nvim_buf_line_count(bufnr))
  local low = math.max(1, math.min(math.floor(tonumber(min_topline) or 1), line_count))
  local low_metrics = M.chat_view_metrics(winid, bufnr, low, padding)
  if low_metrics.content_rows <= low_metrics.visible_height then
    return low, low_metrics
  end

  local best = line_count
  local best_metrics = M.chat_view_metrics(winid, bufnr, best, padding)
  local high = line_count
  while low <= high do
    local mid = math.floor((low + high) / 2)
    local metrics = M.chat_view_metrics(winid, bufnr, mid, padding)
    if metrics.content_rows <= metrics.visible_height then
      best = mid
      best_metrics = metrics
      high = mid - 1
    else
      low = mid + 1
    end
  end
  return best, best_metrics
end

local function with_programmatic_chat_scroll(callback)
  state.chat_scroll_guard = (tonumber(state.chat_scroll_guard) or 0) + 1
  local ok, result = pcall(callback)
  vim.defer_fn(function()
    state.chat_scroll_guard = math.max((tonumber(state.chat_scroll_guard) or 1) - 1, 0)
  end, CHAT_SCROLL_GUARD_MS)
  if not ok then
    error(result)
  end
  return result
end

function M.set_chat_view(topline, cursor_line)
  local winid, bufnr = M.resolve_chat_window_and_buffer()
  if not winid or not bufnr then
    return
  end

  local last_line = math.max(1, vim.api.nvim_buf_line_count(bufnr))
  topline = math.max(1, math.min(math.floor(tonumber(topline) or 1), last_line))
  cursor_line = math.max(1, math.min(math.floor(tonumber(cursor_line) or topline), last_line))

  with_programmatic_chat_scroll(function()
    vim.api.nvim_win_set_cursor(winid, { cursor_line, 0 })
    vim.api.nvim_win_call(winid, function()
      vim.fn.winrestview({ topline = topline })
    end)
  end)
end

--- Setup scroll functions on the main render module.
--- @param render_M table The main render module
--- @param deps table { overlay }
function M.setup(render_M, deps)
  local overlay = deps.overlay

  function render_M.chat_at_bottom()
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return false
    end
    local info = vim.fn.getwininfo(winid)
    if not info or not info[1] then
      return false
    end
    local metrics = M.chat_view_metrics(winid, bufnr, info[1].topline, 0)
    return metrics.content_rows <= metrics.visible_height
  end

  local function sorted_entry_starts(predicate)
    local starts = {}
    for row, idx in pairs(state.entry_row_index or {}) do
      if type(row) == 'number' and type(idx) == 'number' then
        local entry = state.entries[idx]
        if entry and predicate(entry, idx) then
          starts[#starts + 1] = {
            row = row + 1,
            idx = idx,
          }
        end
      end
    end
    table.sort(starts, function(a, b)
      if a.row == b.row then
        return a.idx < b.idx
      end
      return a.row < b.row
    end)
    return starts
  end

  local function jump_to_transcript_entry(direction, predicate)
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return false
    end

    local starts = sorted_entry_starts(predicate)
    if #starts == 0 then
      return false
    end

    local cursor = vim.api.nvim_win_get_cursor(winid)
    local cursor_row = cursor and cursor[1] or 1
    local step = (tonumber(direction) or 1) >= 0 and 1 or -1
    local target
    if step > 0 then
      for _, candidate in ipairs(starts) do
        if candidate.row > cursor_row then
          target = candidate
          break
        end
      end
    else
      for i = #starts, 1, -1 do
        local candidate = starts[i]
        if candidate.row < cursor_row then
          target = candidate
          break
        end
      end
    end

    if not target then
      return false
    end

    vim.api.nvim_set_current_win(winid)
    vim.api.nvim_win_set_cursor(winid, { target.row, 0 })
    state.chat_auto_scroll_enabled = render_M.chat_at_bottom()
    return true
  end

  function render_M.jump_conversation(direction)
    return jump_to_transcript_entry(direction, function(entry)
      return entry.kind == 'user'
    end)
  end

  function render_M.jump_assistant_activity(direction)
    return jump_to_transcript_entry(direction, function(entry)
      return entry.kind == 'assistant' or entry.kind == 'activity'
    end)
  end

  function render_M.scroll_to_bottom()
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return
    end
    local lc = vim.api.nvim_buf_line_count(bufnr)
    local topline, target_metrics = M.target_topline_for_padding(winid, bufnr, 0, 1)
    local raw_topline = overlay.overlay_bottom_topline(lc, vim.api.nvim_win_get_height(winid), 0)
    if target_metrics and (target_metrics.wrapped_rows > 0 or raw_topline ~= topline) then
      log(string.format('chat scroll_to_bottom target=%d raw_target=%d %s %s', topline, raw_topline, M.chat_view_metrics_summary(target_metrics), M.chat_view_log_summary()), vim.log.levels.DEBUG)
    end
    M.set_chat_view(topline, lc)
  end

  local function live_turn_render_allowed()
    return state.history_loading and state.chat_busy
  end

  local function auto_follow_active_conversation()
    if state.history_loading and not live_turn_render_allowed() then
      return false
    end
    if not state.active_conversation_entry_index then
      return false
    end
    return state.chat_auto_scroll_enabled ~= false
  end

  local function active_conversation_topline()
    local target_idx = state.active_conversation_entry_index
    if not target_idx then
      return nil
    end
    for row, idx in pairs(state.entry_row_index or {}) do
      if idx == target_idx then
        return row + 1
      end
    end
    return nil
  end

  local function current_overlay_follow_state()
    local overlay_tool = state.overlay_tool_display
    if type(overlay_tool) ~= 'table' then
      local items = state.recent_activity_items or {}
      for i = #items, 1, -1 do
        local item = items[i]
        if type(item) == 'table' and item.kind == 'tool' and overlay.tool_is_displayable_in_overlay(item.tool_name) then
          overlay_tool = item
          break
        end
      end
    end
    local overlay_active = state.chat_busy == true or type(overlay_tool) == 'table'
    if not overlay_active then
      return {
        task_count = 0,
        reasoning_count = 0,
        padding = 0,
        tail_spacers = 0,
      }
    end

    local enabled, max_lines = overlay.reasoning_config()
    local task_lines = overlay.activity_overlay_lines(max_lines)
    local reasoning_lines_list = enabled and overlay.reasoning_lines(max_lines) or {}
    local first_prefix = '  Reasoning: '
    local other_prefix = '             '
    -- Count rendered reasoning lines (with wrapping)
    local rendered_reasoning_count = 0
    local prefix_width = vim.fn.strdisplaywidth(first_prefix)
    local max_width = math.max(1, overlay.activity_overlay_width() - prefix_width)
    for _, line in ipairs(reasoning_lines_list) do
      local wrapped = overlay.wrap_overlay_text(line, max_width, { collapse_whitespace = false, min_width = 1, trim_chunks = true })
      rendered_reasoning_count = rendered_reasoning_count + math.max(1, #wrapped)
    end
    return {
      task_count = #task_lines,
      reasoning_count = rendered_reasoning_count,
      padding = overlay.overlay_bottom_padding(#task_lines, rendered_reasoning_count),
      tail_spacers = overlay.overlay_tail_spacer_lines(),
    }
  end

  function render_M.reserve_overlay_gutter(task_line_count, reasoning_line_count)
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return
    end

    local view = M.current_chat_view()
    if not view then
      return
    end

    local padding = overlay.overlay_bottom_padding(task_line_count, reasoning_line_count)
    if padding <= 0 then
      return
    end

    local current_topline = math.max(1, math.floor(tonumber(view.topline) or 1))
    local current_metrics = M.chat_view_metrics(winid, bufnr, current_topline, padding)
    local raw_target_topline = overlay.overlay_bottom_topline(current_metrics.line_count, current_metrics.win_height, padding)
    if current_metrics.content_rows <= current_metrics.visible_height then
      log(
        string.format(
          'reasoning overlay gutter unchanged current=%d target=%d raw_target=%d padding=%d line_count=%d %s %s',
          current_topline, current_topline, raw_target_topline, padding,
          current_metrics.line_count, M.chat_view_metrics_summary(current_metrics), M.chat_view_log_summary()
        ),
        vim.log.levels.DEBUG
      )
      return
    end

    local target_topline, target_metrics = M.target_topline_for_padding(winid, bufnr, padding, current_topline)
    if target_topline <= current_topline and current_metrics.content_rows > current_metrics.visible_height then
      log(
        string.format(
          'reasoning overlay gutter constrained current=%d target=%d raw_target=%d padding=%d line_count=%d current_%s target_%s %s',
          current_topline, target_topline, raw_target_topline, padding,
          current_metrics.line_count, M.chat_view_metrics_summary(current_metrics),
          M.chat_view_metrics_summary(target_metrics), M.chat_view_log_summary()
        ),
        vim.log.levels.DEBUG
      )
      return
    end

    local at_bottom = render_M.chat_at_bottom()
    if state.chat_auto_scroll_enabled == false and not at_bottom then
      if state.overlay_gutter_restore_view then
        state.overlay_gutter_restore_view = nil
      end
      log(
        string.format(
          'reasoning overlay gutter skipped while browsing history current=%d target=%d raw_target=%d padding=%d line_count=%d current_%s target_%s %s',
          current_topline, target_topline, raw_target_topline, padding,
          current_metrics.line_count, M.chat_view_metrics_summary(current_metrics),
          M.chat_view_metrics_summary(target_metrics), M.chat_view_log_summary()
        ),
        vim.log.levels.DEBUG
      )
      return
    end

    if state.overlay_gutter_restore_view and (state.overlay_gutter_restore_view.winid ~= winid or state.overlay_gutter_restore_view.bufnr ~= bufnr) then
      state.overlay_gutter_restore_view = nil
    end

    local step = math.max(1, math.floor(current_metrics.win_height / 2))
    local next_topline = math.min(target_topline, current_topline + step)
    local cursor_line = next_topline
    log(
      string.format(
        'reasoning overlay gutter advanced current=%d target=%d next=%d raw_target=%d step=%d padding=%d line_count=%d current_%s target_%s %s',
        current_topline, target_topline, next_topline, raw_target_topline, step, padding,
        current_metrics.line_count, M.chat_view_metrics_summary(current_metrics),
        M.chat_view_metrics_summary(target_metrics), M.chat_view_log_summary()
      ),
      vim.log.levels.DEBUG
    )
    M.set_chat_view(next_topline, cursor_line)
  end

  function render_M.release_overlay_gutter()
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return
    end

    if auto_follow_active_conversation() then
      if state.overlay_gutter_restore_view then
        log('reasoning overlay gutter dropped saved view because active conversation follow resumed', vim.log.levels.DEBUG)
        state.overlay_gutter_restore_view = nil
      end
      log('reasoning overlay gutter released via active conversation follow', vim.log.levels.DEBUG)
      render_M.follow_active_conversation(false)
      return
    end

    local restore_view = state.overlay_gutter_restore_view
    if restore_view then
      state.overlay_gutter_restore_view = nil
      if restore_view.winid == winid and restore_view.bufnr == bufnr then
        log(
          string.format('reasoning overlay gutter restored saved view top=%d cursor=%d %s', restore_view.topline, restore_view.cursor_line or restore_view.topline, M.chat_view_log_summary()),
          vim.log.levels.DEBUG
        )
        M.set_chat_view(restore_view.topline, restore_view.cursor_line or restore_view.topline)
        return
      end
      log('reasoning overlay gutter dropped stale saved view during release', vim.log.levels.DEBUG)
    end

    if render_M.chat_at_bottom() then
      log('reasoning overlay gutter released via scroll_to_bottom', vim.log.levels.DEBUG)
      render_M.scroll_to_bottom()
    end
  end

  function render_M.follow_active_conversation(force)
    local winid, bufnr = M.resolve_chat_window_and_buffer()
    if not winid or not bufnr then
      return false
    end

    local anchor_topline = active_conversation_topline()
    if not anchor_topline then
      return false
    end

    if force then
      state.chat_auto_scroll_enabled = true
    elseif state.chat_auto_scroll_enabled == false then
      return false
    end

    local view = M.current_chat_view()
    if not view then
      return false
    end

    local overlay_state = current_overlay_follow_state()
    local topline = force and anchor_topline or math.max(anchor_topline, state.chat_follow_topline or anchor_topline)
    local win_height = math.max(1, vim.api.nvim_win_get_height(winid))
    local step = math.max(1, math.floor(win_height / 2))
    local last_line = vim.api.nvim_buf_line_count(bufnr)
    if overlay_state.padding > 0 or overlay_state.tail_spacers > 0 then
      local current_metrics = M.chat_view_metrics(winid, bufnr, topline, overlay_state.padding)
      step = math.max(1, math.floor(current_metrics.win_height / 2))
      last_line = current_metrics.line_count
      if current_metrics.content_rows > current_metrics.visible_height then
        local target_topline, target_metrics = M.target_topline_for_padding(winid, bufnr, overlay_state.padding, topline)
        local next_topline = math.min(target_topline, topline + step)
        if next_topline > topline then
          log(
            string.format(
              'conversation follow advanced current=%d target=%d next=%d step=%d padding=%d tail_spacers=%d activity=%d reasoning=%d current_%s target_%s %s',
              topline, target_topline, next_topline, step,
              overlay_state.padding, overlay_state.tail_spacers,
              overlay_state.task_count, overlay_state.reasoning_count,
              M.chat_view_metrics_summary(current_metrics),
              M.chat_view_metrics_summary(target_metrics), M.chat_view_log_summary()
            ),
            vim.log.levels.DEBUG
          )
          topline = next_topline
        end
      else
        log(
          string.format(
            'conversation follow unchanged current=%d padding=%d tail_spacers=%d activity=%d reasoning=%d %s %s',
            topline, overlay_state.padding, overlay_state.tail_spacers,
            overlay_state.task_count, overlay_state.reasoning_count,
            M.chat_view_metrics_summary(current_metrics), M.chat_view_log_summary()
          ),
          vim.log.levels.DEBUG
        )
      end
    elseif last_line > (topline + win_height - 1) then
      topline = math.min(last_line, topline + step)
    end

    state.chat_follow_topline = topline
    M.set_chat_view(topline, math.min(topline, last_line))
    return true
  end

  function render_M.handle_chat_window_scrolled(winid)
    winid = tonumber(winid)
    local chat_winid = select(1, M.resolve_chat_window_and_buffer())
    if not winid or not chat_winid or winid ~= chat_winid then
      return
    end
    if not vim.api.nvim_win_is_valid(winid) or state.history_loading then
      return
    end
    if (tonumber(state.chat_scroll_guard) or 0) > 0 then
      return
    end

    local view = M.current_chat_view()
    if not view then
      return
    end

    if state.overlay_gutter_restore_view then
      log(string.format('reasoning overlay gutter discarded saved view due to manual scroll new_top=%s new_bot=%s', tostring(view.topline), tostring(view.botline)), vim.log.levels.DEBUG)
      state.overlay_gutter_restore_view = nil
    end

    if render_M.chat_at_bottom() then
      state.chat_auto_scroll_enabled = true
      state.chat_follow_topline = view.topline or state.chat_follow_topline
      return
    end

    if state.active_conversation_entry_index then
      state.chat_auto_scroll_enabled = false
    end
  end

  -- Expose helpers needed by render_chat and stream_update
  render_M._auto_follow_active_conversation = auto_follow_active_conversation
end

return M
