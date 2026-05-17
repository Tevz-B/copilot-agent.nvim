-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local activity_diff = require('copilot_agent.activity_diff')
local notify = cfg.notify
local state = cfg.state
local window = require('copilot_agent.window')

local M = {}

local SAMPLE_DIFF = table.concat({
  'diff --git a/sample.lua b/sample.lua',
  '--- a/sample.lua',
  '+++ b/sample.lua',
  '@@ -1,2 +1,3 @@',
  ' local value = 1',
  '+local next_value = value + 1',
  ' return value',
}, '\n')

local menu_bufnr
local menu_winid

local function close_menu()
  if menu_winid and vim.api.nvim_win_is_valid(menu_winid) then
    pcall(vim.api.nvim_win_close, menu_winid, true)
  end
  if menu_bufnr and vim.api.nvim_buf_is_valid(menu_bufnr) then
    pcall(vim.api.nvim_buf_delete, menu_bufnr, { force = true })
  end
  menu_bufnr = nil
  menu_winid = nil
end

local function open_menu()
  if state.input_mode ~= 'test' then
    return false
  end
  if menu_winid and vim.api.nvim_win_is_valid(menu_winid) then
    vim.api.nvim_set_current_win(menu_winid)
    return true
  end

  local lines = {
    '# Copilot Agent Test Mode',
    '',
    'Run a sample UI flow:',
    '',
    '1. Text input prompt',
    '2. Choice selection prompt',
    '3. Permission review prompt',
    '4. Diff preview',
    '5. Editable buffer',
    '',
    'Press <CR> on a line or use 1-5.',
    'q / <Esc> close',
  }

  menu_bufnr = vim.api.nvim_create_buf(false, true)
  vim.api.nvim_buf_set_lines(menu_bufnr, 0, -1, false, lines)
  vim.bo[menu_bufnr].buftype = 'nofile'
  vim.bo[menu_bufnr].bufhidden = 'wipe'
  vim.bo[menu_bufnr].swapfile = false
  vim.bo[menu_bufnr].modifiable = false
  vim.bo[menu_bufnr].filetype = 'markdown'
  vim.b[menu_bufnr].copilot_agent_test_mode = true

  local width = math.min(math.max(52, math.floor(vim.o.columns * 0.5)), 96)
  local height = math.min(math.max(#lines + 2, 10), math.floor(vim.o.lines * 0.7))
  menu_winid = vim.api.nvim_open_win(menu_bufnr, true, {
    relative = 'editor',
    width = width,
    height = height,
    row = math.floor((vim.o.lines - height) / 2),
    col = math.floor((vim.o.columns - width) / 2),
    style = 'minimal',
    border = 'rounded',
    title = ' UI test launcher ',
    title_pos = 'center',
  })

  window.protect_markdown_buffer(menu_bufnr, menu_winid)
  window.set_window_syntax(menu_winid, 'markdown')
  vim.wo[menu_winid].wrap = true
  vim.wo[menu_winid].linebreak = false

  local function reopen_menu_after_modal()
    vim.schedule(function()
      if state.input_mode == 'test' then
        open_menu()
      end
    end)
  end

  local function open_edit_buffer()
    close_menu()
    local buf = vim.api.nvim_create_buf(true, false)
    vim.api.nvim_buf_set_lines(buf, 0, -1, false, {
      '-- Editable test buffer',
      '-- Modify this buffer to test editing UI.',
      '',
      'local value = 1',
      'local next_value = value + 1',
      'return next_value',
    })
    vim.bo[buf].buftype = ''
    vim.bo[buf].bufhidden = 'wipe'
    vim.bo[buf].swapfile = false
    vim.bo[buf].modifiable = true
    vim.bo[buf].filetype = 'lua'
    vim.b[buf].copilot_agent_test_edit = true
    local win = vim.api.nvim_open_win(buf, true, {
      relative = 'editor',
      width = math.min(math.max(70, math.floor(vim.o.columns * 0.7)), 120),
      height = math.min(math.max(12, math.floor(vim.o.lines * 0.6)), 24),
      row = math.floor((vim.o.lines - math.min(math.max(12, math.floor(vim.o.lines * 0.6)), 24)) / 2),
      col = math.floor((vim.o.columns - math.min(math.max(70, math.floor(vim.o.columns * 0.7)), 120)) / 2),
      style = 'minimal',
      border = 'rounded',
      title = ' UI test: edit buffer ',
      title_pos = 'center',
    })
    window.disable_folds(win)
    local function close_edit()
      if vim.api.nvim_win_is_valid(win) then
        vim.api.nvim_win_close(win, true)
      end
      reopen_menu_after_modal()
    end
    vim.keymap.set('n', 'q', close_edit, { buffer = buf, nowait = true })
    vim.keymap.set('n', '<Esc>', close_edit, { buffer = buf, nowait = true })
    vim.keymap.set({ 'n', 'i' }, '<C-c>', close_edit, { buffer = buf, nowait = true })
    vim.api.nvim_create_autocmd('WinClosed', {
      pattern = tostring(win),
      once = true,
      callback = reopen_menu_after_modal,
    })
  end

  local function run_text_prompt()
    close_menu()
    vim.ui.input({ prompt = 'UI test: enter freeform text: ' }, function(input)
      if input and input ~= '' then
        notify('UI test input: ' .. input, vim.log.levels.INFO)
      else
        notify('UI test input dismissed', vim.log.levels.WARN)
      end
      reopen_menu_after_modal()
    end)
  end

  local function run_choice_prompt()
    close_menu()
    vim.ui.select({ 'Choice A', 'Choice B', 'Choice C' }, { prompt = 'UI test: choose one option' }, function(choice)
      if choice then
        notify('UI test choice: ' .. choice, vim.log.levels.INFO)
      else
        notify('UI test choice dismissed', vim.log.levels.WARN)
      end
      reopen_menu_after_modal()
    end)
  end

  local function run_diff_preview()
    close_menu()
    activity_diff.open_preview_patch_text(SAMPLE_DIFF, {
      enter = true,
      after_close = reopen_menu_after_modal,
    })
  end

  local function run_permission_prompt()
    close_menu()
    local options = { 'Allow', 'Deny', 'Show diff' }
    vim.ui.select(options, { prompt = 'UI test: permission review' }, function(choice)
      if choice == 'Show diff' then
        activity_diff.open_preview_patch_text(SAMPLE_DIFF, {
          enter = true,
          after_close = reopen_menu_after_modal,
        })
        return
      end
      if choice then
        notify('UI test permission: ' .. choice, vim.log.levels.INFO)
      else
        notify('UI test permission dismissed', vim.log.levels.WARN)
      end
      reopen_menu_after_modal()
    end)
  end

  local actions = {
    [1] = run_text_prompt,
    [2] = run_choice_prompt,
    [3] = run_permission_prompt,
    [4] = run_diff_preview,
    [5] = open_edit_buffer,
  }

  local function run_current()
    local row = (vim.api.nvim_win_get_cursor(menu_winid)[1] or 1) - 4
    local action = actions[row]
    if action then
      action()
    end
  end

  vim.keymap.set('n', 'q', close_menu, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '<Esc>', close_menu, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '<Esc><Esc>', close_menu, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set({ 'n', 'i' }, '<C-c>', close_menu, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '<CR>', run_current, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '1', run_text_prompt, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '2', run_choice_prompt, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '3', run_permission_prompt, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '4', run_diff_preview, { buffer = menu_bufnr, nowait = true })
  vim.keymap.set('n', '5', open_edit_buffer, { buffer = menu_bufnr, nowait = true })

  vim.api.nvim_create_autocmd('WinClosed', {
    pattern = tostring(menu_winid),
    once = true,
    callback = function()
      menu_winid = nil
      menu_bufnr = nil
    end,
  })

  return true
end

function M.open()
  return open_menu()
end

function M.close()
  close_menu()
end

return M
