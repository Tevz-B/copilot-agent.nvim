-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.
--
-- render/content.lua – Content formatting: table alignment, line normalization,
-- entry_lines formatting.

local utils = require('copilot_agent.utils')
local split_lines = utils.split_lines
local sanitize_display_text = function(text)
  return utils.tilde_home_path(utils.normalize_display_text(text))
end

local M = {}

-- ── Table alignment ───────────────────────────────────────────────────────────

local ALIGN_MAX_LINE_LEN = 500
local ALIGN_MAX_COLS = 30
local ALIGN_MAX_ROWS = 200

function M.align_tables(lines)
  local out = {}
  local i = 1
  local n = #lines

  local function is_table_row(line)
    return line:match('^%s*|') ~= nil
  end

  local function parse_cells(line)
    if #line > ALIGN_MAX_LINE_LEN then
      return nil
    end
    local inner = line:match('^%s*|(.+)|%s*$')
    if not inner then
      return nil
    end
    local raw = vim.split(inner, '|', { plain = true })
    if #raw > ALIGN_MAX_COLS then
      return nil
    end
    local cells = {}
    for _, cell in ipairs(raw) do
      cells[#cells + 1] = vim.trim(cell)
    end
    return cells
  end

  local function is_separator_row(cells)
    for _, c in ipairs(cells) do
      if not c:match('^:?%-+:?$') then
        return false
      end
    end
    return true
  end

  local strdisplaywidth = vim.fn.strdisplaywidth
  local function cell_width(text)
    local w = strdisplaywidth(text)
    local _, backtick_count = text:gsub('`', '')
    return w - backtick_count
  end

  while i <= n do
    if is_table_row(lines[i]) then
      local block_start = i
      local rows = {}
      while i <= n and is_table_row(lines[i]) and #rows < ALIGN_MAX_ROWS do
        local indent = lines[i]:match('^(%s*)') or ''
        local cells = parse_cells(lines[i])
        if not cells then
          break
        end
        local sep = is_separator_row(cells)
        rows[#rows + 1] = { cells = cells, is_sep = sep, indent = indent }
        i = i + 1
      end

      if #rows < 2 then
        for j = block_start, i - 1 do
          out[#out + 1] = lines[j]
        end
        if i == block_start then
          out[#out + 1] = lines[i]
          i = i + 1
        end
      else
        local max_cols = 0
        for _, row in ipairs(rows) do
          if #row.cells > max_cols then
            max_cols = #row.cells
          end
        end

        local col_widths = {}
        for c = 1, max_cols do
          col_widths[c] = 0
        end
        for _, row in ipairs(rows) do
          if not row.is_sep then
            for c = 1, max_cols do
              local cell = row.cells[c] or ''
              local w = cell_width(cell)
              if w > col_widths[c] then
                col_widths[c] = w
              end
            end
          end
        end

        for _, row in ipairs(rows) do
          local parts = {}
          for c = 1, max_cols do
            local cell = row.cells[c] or ''
            if row.is_sep then
              local prefix = cell:sub(1, 1) == ':' and ':' or ''
              local suffix = cell:sub(-1) == ':' and ':' or ''
              local dash_count = math.max(3, col_widths[c] - #prefix - #suffix)
              parts[#parts + 1] = prefix .. string.rep('-', dash_count) .. suffix
            else
              local pad = col_widths[c] - cell_width(cell)
              parts[#parts + 1] = cell .. string.rep(' ', math.max(0, pad))
            end
          end
          out[#out + 1] = row.indent .. '| ' .. table.concat(parts, ' | ') .. ' |'
        end
      end
    else
      out[#out + 1] = lines[i]
      i = i + 1
    end
  end
  return out
end

-- ── Content line classification and normalization ─────────────────────────────

local function trim_text(text)
  return vim.trim(type(text) == 'string' and text or '')
end

local function classify_content_line(line)
  if type(line) ~= 'string' then
    return 'text'
  end
  if line == '' then
    return 'blank'
  end
  if line:match('^%s+$') then
    return 'soft_blank'
  end

  local trimmed = trim_text(line)
  if trimmed:match('^[-*+]%s+') or trimmed:match('^%d+[.)]%s+') then
    return 'list'
  end
  if trimmed:match('^Done%.$') or trimmed:match('^Status:?') then
    return 'status'
  end
  if trimmed:match('^#+%s+') then
    return 'block'
  end
  if trimmed:match('^```') or trimmed:match('^~~~') then
    return 'block'
  end
  if trimmed:match('^>') then
    return 'block'
  end
  if trimmed:match('^|') then
    return 'table'
  end
  return 'text'
end

local function needs_blank_before(kind, previous_kind)
  if kind == 'list' or kind == 'table' or kind == 'block' then
    return previous_kind == 'text' or previous_kind == 'status'
  end
  if kind == 'status' then
    return previous_kind ~= nil and previous_kind ~= 'status'
  end
  return false
end

local function needs_blank_after(previous_kind, next_kind)
  if previous_kind == 'list' or previous_kind == 'table' or previous_kind == 'block' then
    return next_kind == 'text' or next_kind == 'status'
  end
  if previous_kind == 'status' then
    return next_kind == 'text' or next_kind == 'list'
  end
  return false
end

function M.normalize_content_lines(lines)
  local normalized = {}
  local previous_kind
  local in_fence = false

  local function append_blank()
    if #normalized > 0 and normalized[#normalized].line ~= '' then
      normalized[#normalized + 1] = { line = '', kind = 'blank', in_fence = false }
      previous_kind = 'blank'
    end
  end

  for _, line in ipairs(lines or {}) do
    local trimmed = trim_text(line)
    if trimmed:match('^```') or trimmed:match('^~~~') then
      if not in_fence and needs_blank_before('block', previous_kind) then
        append_blank()
      end
      normalized[#normalized + 1] = {
        line = line,
        kind = 'block',
        in_fence = true,
        fence_role = in_fence and 'close' or 'open',
      }
      previous_kind = 'block'
      in_fence = not in_fence
    elseif in_fence then
      normalized[#normalized + 1] = { line = line, kind = 'block', in_fence = true, fence_role = 'body' }
      previous_kind = 'block'
    else
      local kind = classify_content_line(line)
      if kind ~= 'blank' and kind ~= 'soft_blank' then
        if needs_blank_before(kind, previous_kind) then
          append_blank()
        end
        normalized[#normalized + 1] = { line = line, kind = kind, in_fence = false }
        previous_kind = kind
      end
    end
  end

  local with_transitions = {}
  for idx, item in ipairs(normalized) do
    with_transitions[#with_transitions + 1] = item.line
    local next_item = normalized[idx + 1]
    local current_blocks_following = item.in_fence and item.fence_role ~= 'close'
    local next_blocks_spacing = next_item and next_item.in_fence and next_item.fence_role ~= 'close'
    if next_item and not current_blocks_following and not next_blocks_spacing and next_item.kind ~= 'blank' and needs_blank_after(item.kind, next_item.kind) then
      with_transitions[#with_transitions + 1] = ''
    end
  end

  while #with_transitions > 0 and with_transitions[#with_transitions] == '' do
    table.remove(with_transitions)
  end

  return with_transitions
end

function M.verbatim_content_lines(lines)
  local preserved = {}
  for _, line in ipairs(lines or {}) do
    if type(line) == 'string' and line:match('^%s+$') then
      preserved[#preserved + 1] = ''
    else
      preserved[#preserved + 1] = line
    end
  end
  return preserved
end

-- ── Entry formatting ──────────────────────────────────────────────────────────

--- Format one transcript entry into display lines.
--- @param entry table The entry to format
--- @param _idx number Entry index
--- @param align boolean|nil When true (default), apply align_tables
--- @param opts table Context: { activity_entries_visible, collapsed_activity_line, should_merge_assistant }
function M.entry_lines(entry, _idx, align, opts)
  if align == nil then
    align = true
  end
  opts = opts or {}
  local out = {}
  local content = sanitize_display_text(entry.content or '')
  if entry.kind == 'activity' then
    if not opts.activity_entries_visible then
      out[#out + 1] = opts.collapsed_activity_line(entry)
    else
      out[#out + 1] = 'Activity:'
      for _, l in ipairs(M.normalize_content_lines(split_lines(content))) do
        out[#out + 1] = '  ' .. l
      end
    end
    out[#out + 1] = ''
  elseif entry.kind == 'system' or entry.kind == 'error' then
    out[#out + 1] = (entry.kind == 'error' and 'Error' or 'System') .. ':'
    for _, l in ipairs(M.normalize_content_lines(split_lines(content))) do
      out[#out + 1] = '  ' .. l
    end
    out[#out + 1] = ''
  elseif entry.kind == 'assistant' then
    local trimmed = vim.trim(content)
    if trimmed ~= '' then
      out[#out + 1] = 'Response:'
      for _, l in ipairs(M.verbatim_content_lines(split_lines(content))) do
        out[#out + 1] = '  ' .. l
      end
      out[#out + 1] = ''
    end
  else
    out[#out + 1] = 'Prompt:'
    for _, l in ipairs(M.normalize_content_lines(split_lines(content))) do
      out[#out + 1] = '  ' .. l
    end
    if entry.attachments and #entry.attachments > 0 then
      for _, a in ipairs(entry.attachments) do
        out[#out + 1] = '  📎 ' .. sanitize_display_text(a.display or a.path or a.type or '')
      end
    end
    out[#out + 1] = ''
  end
  return align and M.align_tables(out) or out
end

return M
