-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Optional nvim-cmp source bridging the plugin's native slash-command /
-- attachment completion (lua/copilot_agent/input.lua) into nvim-cmp, so
-- users who prefer cmp's popup/matching/mappings over Vim's built-in
-- ins-completion menu get a consistent experience inside the chat input
-- buffer. This is purely additive: the native completefunc path keeps
-- working unchanged for everyone else. See `setup_cmp_buffer()` in
-- input.lua for how/when this gets attached.

local M = {}

function M.new()
  return setmetatable({}, { __index = M })
end

-- Only offer completions while the cursor sits inside a recognized
-- slash-command / attachment token (mirrors the native completefunc gate).
function M:is_available()
  local ok, input = pcall(require, 'copilot_agent.input')
  if not ok or type(input._input_completion_context) ~= 'function' then
    return false
  end
  local line = vim.api.nvim_get_current_line()
  local col = vim.api.nvim_win_get_cursor(0)[2]
  return input._input_completion_context(line:sub(1, col)) ~= nil
end

function M:get_trigger_characters()
  return { '/', '@' }
end

-- The default cmp keyword pattern doesn't include '/' or '@', which would
-- misalign cmp's match offset with our tokens (e.g. "/model"). Override it
-- so cmp anchors matching at the trigger character itself.
function M:get_keyword_pattern()
  return [[\%(/\|@\)\S*]]
end

function M:complete(_, callback)
  local ok, input = pcall(require, 'copilot_agent.input')
  if not ok or type(input._input_completion_context) ~= 'function' then
    callback({ items = {}, isIncomplete = false })
    return
  end

  local line = vim.api.nvim_get_current_line()
  local cursor = vim.api.nvim_win_get_cursor(0)
  local row, col = cursor[1] - 1, cursor[2]
  local completion_request = input._input_completion_context(line:sub(1, col))
  if not completion_request then
    callback({ items = {}, isIncomplete = false })
    return
  end

  local ok_items, raw_items = pcall(input._input_completefunc, 0, completion_request.token or '')
  if not ok_items or type(raw_items) ~= 'table' then
    callback({ items = {}, isIncomplete = false })
    return
  end

  local cmp_ok, cmp = pcall(require, 'cmp')
  local kind_text = cmp_ok and cmp.lsp.CompletionItemKind.Text or 1
  local start_char = completion_request.start - 1

  local items = {}
  for _, raw in ipairs(raw_items) do
    if type(raw) == 'table' and type(raw.word) == 'string' and raw.word ~= '' then
      local label = (type(raw.abbr) == 'string' and raw.abbr ~= '') and raw.abbr or raw.word
      items[#items + 1] = {
        label = label,
        filterText = label,
        sortText = label,
        kind = kind_text,
        labelDetails = (type(raw.menu) == 'string' and raw.menu ~= '') and { description = raw.menu } or nil,
        documentation = (type(raw.info) == 'string' and raw.info ~= '') and {
          kind = 'plaintext',
          value = raw.info,
        } or nil,
        textEdit = {
          range = {
            start = { line = row, character = start_char },
            ['end'] = { line = row, character = col },
          },
          newText = raw.word,
        },
      }
    end
  end

  -- isIncomplete = true: re-invoke complete() on every keystroke so our
  -- own context-aware filtering (slash command query, file path segments,
  -- etc.) stays authoritative instead of cmp's generic fuzzy matcher alone.
  callback({ items = items, isIncomplete = true })
end

return M
