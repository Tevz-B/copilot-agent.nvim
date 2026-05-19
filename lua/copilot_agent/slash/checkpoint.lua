-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local http = require('copilot_agent.http')
local render = require('copilot_agent.render')
local sl = require('copilot_agent.statusline')

local state = cfg.state
local notify = cfg.notify
local append_entry = render.append_entry
local refresh_statuslines = sl.refresh_statuslines
local request = http.request

local M = {}

local function compact_result_message(result)
  if type(result) ~= 'table' then
    return 'History compaction finished'
  end

  local parts = {}
  if result.success == false then
    local err = vim.trim(result.error or '')
    return err ~= '' and ('History compaction failed: ' .. err) or 'History compaction failed'
  end
  if tonumber(result.messagesRemoved) then
    parts[#parts + 1] = string.format('%d messages removed', tonumber(result.messagesRemoved))
  end
  if tonumber(result.tokensRemoved) then
    parts[#parts + 1] = string.format('%d tokens freed', tonumber(result.tokensRemoved))
  end

  local context = result.contextWindow
  if type(context) == 'table' and tonumber(context.currentTokens) and tonumber(context.tokenLimit) then
    parts[#parts + 1] = string.format('%d/%d tokens', tonumber(context.currentTokens), tonumber(context.tokenLimit))
  end

  if #parts == 0 then
    return 'History compacted successfully'
  end
  return 'History compacted: ' .. table.concat(parts, ', ')
end

function M.compact_history()
  local session = require('copilot_agent.session')
  local events = require('copilot_agent.events')
  session.with_session(function(session_id, err)
    if err then
      append_entry('error', err)
      return
    end

    request('POST', string.format('/sessions/%s/compact', session_id), {}, function(response, request_err)
      if request_err then
        append_entry('error', 'Compaction failed: ' .. request_err)
        return
      end

      local result = response and response.result or nil
      if type(result) == 'table' and result.success == false then
        append_entry('error', compact_result_message(result))
        return
      end

      if type(result) == 'table' and type(result.contextWindow) == 'table' then
        state.context_tokens = tonumber(result.contextWindow.currentTokens) or state.context_tokens
        state.context_limit = tonumber(result.contextWindow.tokenLimit) or state.context_limit
        refresh_statuslines()
      end

      events.reload_session_history(session_id, function(reload_err)
        if reload_err then
          append_entry('error', 'Compaction succeeded but history reload failed: ' .. reload_err)
          return
        end
        append_entry('system', compact_result_message(result))
      end)
    end)
  end)
  return true
end

local function short_hash(commit)
  local text = vim.trim(commit or '')
  if text == '' then
    return 'unknown'
  end
  return #text > 12 and text:sub(1, 12) or text
end

local function restore_command_label(command_name, requested_checkpoint, result)
  if command_name == 'undo' then
    return '/undo'
  end
  local target_id = type(result) == 'table' and type(result.target) == 'table' and result.target.id or nil
  local checkpoint = vim.trim(requested_checkpoint or '')
  if checkpoint ~= '' then
    return '/rewind ' .. checkpoint
  end
  if target_id and target_id ~= '' then
    return '/rewind ' .. target_id
  end
  return '/rewind'
end

local function restore_context_text(command_label, result)
  local target = type(result) == 'table' and type(result.target) == 'table' and result.target or nil
  if not target or type(target.id) ~= 'string' or target.id == '' then
    return nil
  end

  local lines = {
    'Checkpoint restore context for the next Copilot turn:',
    '- Command: ' .. command_label,
    '- Target checkpoint: ' .. target.id,
    '- Target checkpoint git hash: ' .. tostring(target.commit or 'unknown'),
  }

  if type(result.previous_head) == 'string' and result.previous_head ~= '' and result.previous_head ~= target.commit then
    lines[#lines + 1] = '- Previous checkpoint git hash: ' .. result.previous_head
  end

  if type(result.reverted) == 'table' and not vim.tbl_isempty(result.reverted) then
    lines[#lines + 1] = '- Reverted checkpoints:'
    for _, item in ipairs(result.reverted) do
      local detail = string.format('  - %s (%s): User asked %s', tostring(item.id or '?'), short_hash(item.commit), tostring(item.prompt_summary or 'checkpoint'))
      if type(item.assistant_summary) == 'string' and item.assistant_summary ~= '' then
        detail = detail .. '; Copilot updated ' .. item.assistant_summary
      end
      lines[#lines + 1] = detail
    end
  else
    lines[#lines + 1] = '- Reverted checkpoints: none; the workspace was restored to the latest saved checkpoint state.'
  end

  lines[#lines + 1] = '- This restore context will be included automatically with the next prompt sent to Copilot.'
  lines[#lines + 1] = '- Copilot may run `git diff` in the workspace to inspect the reverted code before making more changes.'
  return table.concat(lines, '\n')
end

local function queue_restore_context(command_name, requested_checkpoint, result)
  local context_text = restore_context_text(restore_command_label(command_name, requested_checkpoint, result), result)
  if not context_text then
    return nil
  end
  state.pending_session_context = {
    session_id = state.session_id,
    text = context_text,
  }
  return context_text
end

function M.undo_checkpoint()
  local checkpoints = require('copilot_agent.checkpoints')
  if not state.session_id then
    notify('No active session to undo', vim.log.levels.WARN)
    return true
  end

  checkpoints.undo(state.session_id, function(err, result)
    if err and err ~= '' then
      if err == 'No checkpoints available' then
        notify(err, vim.log.levels.INFO)
        return
      end
      append_entry('error', 'Undo failed: ' .. err)
      return
    end
    append_entry('system', queue_restore_context('undo', nil, result) or 'Restored latest checkpoint')
  end)
  return true
end

function M.rewind_checkpoint(args)
  local checkpoints = require('copilot_agent.checkpoints')
  if not state.session_id then
    notify('No active session to rewind', vim.log.levels.WARN)
    return true
  end

  args = vim.trim(args or '')
  checkpoints.rewind(state.session_id, args ~= '' and args or nil, function(err, result)
    if err and err ~= '' then
      if err == 'No checkpoints available' then
        notify(err, vim.log.levels.INFO)
        return
      end
      append_entry('error', 'Rewind failed: ' .. err)
      return
    end
    local context_text = queue_restore_context('rewind', args, result)
    if context_text then
      append_entry('system', context_text)
    end
  end)
  return true
end

return M
