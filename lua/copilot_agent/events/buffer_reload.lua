-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Buffer reload: detect and reconcile external file changes.

local cfg = require('copilot_agent.config')
local notify = cfg.notify

local M = {}

local external_reload_prompt = 'The open buffer has been updated externally. Do you want to reload it? (yes/no)'
local buffer_disk_state = {}
local open_buffer_refresh_pending = false

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

local function confirm_external_buffer_reload()
  return vim.fn.confirm(external_reload_prompt, '&yes\n&no', 2) == 1
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

M.confirm_external_buffer_reload = confirm_external_buffer_reload
M.remember_buffer_disk_state = remember_buffer_disk_state
M.forget_buffer_disk_state = forget_buffer_disk_state
M.remember_open_buffer_disk_state = remember_open_buffer_disk_state
M.check_open_buffers_for_external_changes = check_open_buffers_for_external_changes
M.schedule_open_buffer_refresh = schedule_open_buffer_refresh

return M
