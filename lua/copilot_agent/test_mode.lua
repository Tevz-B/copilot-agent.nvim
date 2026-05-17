-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local activity_diff = require('copilot_agent.activity_diff')
local utils = require('copilot_agent.utils')
local notify = cfg.notify
local state = cfg.state
local resolve_select_choice = utils.resolve_select_choice

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

local function reopen_menu()
  vim.defer_fn(function()
    if state.input_mode == 'test' then
      M.open()
    end
  end, 20)
end

local function queue_step(step)
  if type(step) ~= 'function' then
    return
  end
  vim.defer_fn(function()
    if state.input_mode == 'test' then
      step()
    end
  end, 20)
end

local function item_label(item)
  if type(item) == 'string' then
    return item
  end

  local code = vim.trim(item.code or item.id or item.short or '')
  local title = vim.trim(item.label or item.title or item.name or '')
  local description = vim.trim(item.description or item.detail or '')

  local header = title
  if code ~= '' then
    header = header ~= '' and string.format('%s — %s', code, header) or code
  end
  if description ~= '' then
    return header ~= '' and (header .. ' · ' .. description) or description
  end
  return header
end

local function run_text_prompt(next_step)
  notify('UI test: opening text input prompt', vim.log.levels.INFO)
  vim.ui.input({
    prompt = 'UI test: paste a detailed request, e.g. "Summarize this change set and mention the file names": ',
  }, function(input)
    if input and input ~= '' then
      notify('UI test input: ' .. input, vim.log.levels.INFO)
    else
      notify('UI test input dismissed', vim.log.levels.WARN)
    end
    if next_step then
      queue_step(next_step)
    else
      reopen_menu()
    end
  end)
end

local function run_choice_prompt(next_step)
  notify('UI test: opening selection prompt', vim.log.levels.INFO)
  local items = {
    {
      code = 'A12',
      label = 'Insert markdown summary',
      description = 'Use a long description to exercise picker wrapping and show the code + label together.',
    },
    {
      code = 'B07',
      label = 'Open recent workspace session',
      description = 'Display a more realistic action name alongside the stable code that would appear in a picker.',
    },
    {
      code = 'C42',
      label = 'Switch model for this session',
      description = 'Simulate a menu item with an id-like code, a readable title, and a longer explanatory tail.',
    },
  }
  vim.ui.select(items, {
    prompt = 'UI test: choose one item with code and long description',
    format_item = item_label,
  }, function(choice)
    choice = resolve_select_choice(items, choice, item_label)
    if choice then
      notify('UI test choice: ' .. item_label(choice), vim.log.levels.INFO)
    else
      notify('UI test choice dismissed', vim.log.levels.WARN)
    end
    if next_step then
      queue_step(next_step)
    else
      reopen_menu()
    end
  end)
end

local function run_permission_prompt(next_step)
  notify('UI test: opening permission review prompt', vim.log.levels.INFO)
  local items = {
    {
      code = 'ALLOW',
      label = 'Allow patch application',
      description = 'Approve a sample edit that would touch multiple files and look like a normal review prompt.',
    },
    {
      code = 'DENY',
      label = 'Reject the change',
      description = 'Exercise the same choice list with a negative outcome and a descriptive code.',
    },
    {
      code = 'DIFF',
      label = 'Show diff before deciding',
      description = 'Open the diff preview flow from a picker option that carries both text and a code.',
    },
  }
  vim.ui.select(items, {
    prompt = 'UI test: permission review with coded options',
    format_item = item_label,
  }, function(choice)
    choice = resolve_select_choice(items, choice, item_label)
    if choice and choice.code == 'DIFF' then
      activity_diff.open_preview_patch_text(SAMPLE_DIFF, {
        enter = true,
        after_close = next_step or reopen_menu,
      })
      return
    end
    if choice then
      notify('UI test permission: ' .. item_label(choice), vim.log.levels.INFO)
    else
      notify('UI test permission dismissed', vim.log.levels.WARN)
    end
    if next_step then
      queue_step(next_step)
    else
      reopen_menu()
    end
  end)
end

local function run_diff_preview(next_step)
  notify('UI test: opening diff preview', vim.log.levels.INFO)
  activity_diff.open_preview_patch_text(SAMPLE_DIFF, {
    enter = true,
    after_close = next_step or reopen_menu,
  })
end

local function run_edit_buffer(next_step)
  notify('UI test: opening editable buffer', vim.log.levels.INFO)
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

  local height = math.min(math.max(12, math.floor(vim.o.lines * 0.6)), 24)
  local width = math.min(math.max(70, math.floor(vim.o.columns * 0.7)), 120)
  local win = vim.api.nvim_open_win(buf, true, {
    relative = 'editor',
    width = width,
    height = height,
    row = math.floor((vim.o.lines - height) / 2),
    col = math.floor((vim.o.columns - width) / 2),
    style = 'minimal',
    border = 'rounded',
    title = ' UI test: edit buffer ',
    title_pos = 'center',
  })
  vim.wo[win].wrap = false
  vim.wo[win].linebreak = false

  local closed = false
  local function close_edit()
    if closed then
      return
    end
    closed = true
    if vim.api.nvim_win_is_valid(win) then
      vim.api.nvim_win_close(win, true)
    end
    if next_step then
      queue_step(next_step)
    else
      reopen_menu()
    end
  end

  vim.keymap.set('n', 'q', close_edit, { buffer = buf, nowait = true })
  vim.keymap.set('n', '<Esc>', close_edit, { buffer = buf, nowait = true })
  vim.keymap.set({ 'n', 'i' }, '<C-c>', close_edit, { buffer = buf, nowait = true })
  vim.api.nvim_create_autocmd('WinClosed', {
    pattern = tostring(win),
    once = true,
    callback = function()
      close_edit()
    end,
  })
end

local function run_all()
  local steps = {
    run_text_prompt,
    run_choice_prompt,
    run_permission_prompt,
    run_diff_preview,
    run_edit_buffer,
  }

  local index = 1
  local function next_step()
    index = index + 1
    local step = steps[index]
    if step then
      step(next_step)
    else
      reopen_menu()
    end
  end

  local first = steps[index]
  if first then
    queue_step(function()
      first(next_step)
    end)
  end
end

function M.open()
  if state.input_mode ~= 'test' then
    return false
  end

  local items = {
    {
      code = 'ALL',
      label = 'Run all workflows',
      description = 'Text input, coded select, permission review, diff preview, and editable buffer in sequence.',
    },
    {
      code = 'TEXT',
      label = 'Text input prompt',
      description = 'Ask for a long freeform request the same way a real prompt would.',
    },
    {
      code = 'SELECT',
      label = 'Selection with long descriptions',
      description = 'Show a picker with codes, labels, and long explanatory text.',
    },
    {
      code = 'REVIEW',
      label = 'Permission review prompt',
      description = 'Simulate approve / reject / diff choices with coded options.',
    },
    {
      code = 'DIFF',
      label = 'Diff preview',
      description = 'Open the diff viewer from the launcher.',
    },
    {
      code = 'EDIT',
      label = 'Editable buffer',
      description = 'Open a normal buffer that can be edited like a draft response.',
    },
  }

  vim.ui.select(items, {
    prompt = 'Copilot Agent test mode',
    format_item = item_label,
  }, function(choice)
    choice = resolve_select_choice(items, choice, item_label)
    if choice == nil then
      return
    end
    vim.schedule(function()
      if state.input_mode ~= 'test' then
        return
      end
      notify('UI test: selected ' .. item_label(choice), vim.log.levels.INFO)
      if choice.code == 'ALL' then
        run_all()
      elseif choice.code == 'TEXT' then
        run_text_prompt()
      elseif choice.code == 'SELECT' then
        run_choice_prompt()
      elseif choice.code == 'REVIEW' then
        run_permission_prompt()
      elseif choice.code == 'DIFF' then
        run_diff_preview()
      elseif choice.code == 'EDIT' then
        run_edit_buffer()
      end
    end)
  end)

  return true
end

function M.close()
  return true
end

return M
