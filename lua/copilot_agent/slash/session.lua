-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local render = require('copilot_agent.render')
local utils = require('copilot_agent.utils')

local state = cfg.state
local notify = cfg.notify
local append_entry = render.append_entry

-- These modules are accessed via require() each time because tests may
-- reload them between test cases.
local function get_http()
  return require('copilot_agent.http')
end
local function get_checkpoints()
  return require('copilot_agent.checkpoints')
end
local function get_session()
  return require('copilot_agent.session')
end
local function get_session_names()
  return require('copilot_agent.session_names')
end

local M = {}

--- Simple utility: return first non-empty string argument.
local function first_non_empty_string(...)
  for i = 1, select('#', ...) do
    local value = select(i, ...)
    if type(value) == 'string' and value ~= '' then
      return value
    end
  end
  return nil
end

local function merge_session_catalog(response)
  local merged = {}
  local order = {}

  local function upsert(item, live_source)
    local session_id = item and item.sessionId or nil
    if type(session_id) ~= 'string' or session_id == '' then
      return
    end

    local normalized = vim.deepcopy(item)
    normalized.live = live_source == true or normalized.live == true
    if not merged[session_id] then
      merged[session_id] = normalized
      order[#order + 1] = session_id
      return
    end

    local existing = merged[session_id]
    if normalized.live then
      normalized.summary = normalized.summary or existing.summary
      normalized.workingDirectory = normalized.workingDirectory or existing.workingDirectory
      normalized.workspacePath = normalized.workspacePath or existing.workspacePath
      normalized.modifiedTime = normalized.modifiedTime or existing.modifiedTime
      normalized.startTime = normalized.startTime or existing.startTime
      normalized.createdAt = normalized.createdAt or existing.createdAt
      merged[session_id] = normalized
      return
    end

    existing.summary = existing.summary or normalized.summary
    existing.workingDirectory = existing.workingDirectory or normalized.workingDirectory
    existing.workspacePath = existing.workspacePath or normalized.workspacePath
    existing.modifiedTime = existing.modifiedTime or normalized.modifiedTime
    existing.startTime = existing.startTime or normalized.startTime
    existing.createdAt = existing.createdAt or normalized.createdAt
  end

  for _, item in ipairs((response and response.persisted) or {}) do
    upsert(item, false)
  end
  for _, item in ipairs((response and response.live) or {}) do
    upsert(item, true)
  end

  local sessions = {}
  for _, session_id in ipairs(order) do
    sessions[#sessions + 1] = merged[session_id]
  end
  table.sort(sessions, function(left, right)
    local left_key = first_non_empty_string(left.modifiedTime, left.startTime, left.createdAt) or ''
    local right_key = first_non_empty_string(right.modifiedTime, right.startTime, right.createdAt) or ''
    return left_key > right_key
  end)
  return sessions
end

local function fetch_session_catalog(callback)
  get_http().request('GET', '/sessions', nil, function(response, err)
    if err then
      callback(nil, err)
      return
    end
    callback(merge_session_catalog(response), nil)
  end)
end

local function session_label(session_id, summary)
  local resolved = get_session_names().resolve(summary, session_id)
  local formatted_id = utils.format_session_id(session_id)
  if type(resolved) == 'string' and resolved ~= '' then
    return resolved .. ' [' .. formatted_id .. ']'
  end
  return formatted_id
end

local function find_session_record(sessions, token)
  token = vim.trim(token or '')
  if token == '' then
    return nil
  end

  local lowered = token:lower()
  for _, item in ipairs(sessions or {}) do
    local session_id = item.sessionId or ''
    if session_id:lower() == lowered then
      return item
    end
  end

  for _, item in ipairs(sessions or {}) do
    local session_id = item.sessionId or ''
    if utils.format_session_id(session_id):lower() == lowered then
      return item
    end
  end

  for _, item in ipairs(sessions or {}) do
    local resolved = get_session_names().resolve(item.summary, item.sessionId)
    if type(resolved) == 'string' and resolved:lower() == lowered then
      return item
    end
  end

  local prefix_matches = {}
  for _, item in ipairs(sessions or {}) do
    local session_id = (item.sessionId or ''):lower()
    if lowered ~= '' and vim.startswith(session_id, lowered) then
      prefix_matches[#prefix_matches + 1] = item
    end
  end
  if #prefix_matches == 1 then
    return prefix_matches[1]
  end

  return nil
end

local function resolve_session_target(raw, opts, callback)
  opts = opts or {}
  raw = vim.trim(raw or '')
  if raw == '' then
    if state.session_id and state.session_id ~= '' then
      callback({
        session_id = state.session_id,
        session = nil,
        checkpoint_info = get_checkpoints().session_info(state.session_id),
      }, nil)
      return
    end
    callback(nil, opts.missing_message or 'No active session')
    return
  end

  fetch_session_catalog(function(sessions, err)
    if err then
      callback(nil, err)
      return
    end

    local record = find_session_record(sessions, raw)
    local session_id = record and record.sessionId or raw
    local checkpoint_info = get_checkpoints().session_info(session_id)
    if not record and not checkpoint_info and not opts.allow_raw_id then
      callback(nil, 'Session not found: ' .. raw)
      return
    end
    callback({
      session_id = session_id,
      session = record,
      checkpoint_info = checkpoint_info,
    }, nil)
  end)
end

local function parse_session_timestamp(value)
  value = vim.trim(value or '')
  if value == '' then
    return nil
  end

  local normalized = value:gsub('%.%d+', '')
  local zulu = normalized:match('^(%d%d%d%d%-%d%d%-%d%dT%d%d:%d%d:%d%d)Z$')
  if zulu then
    local parsed = tonumber(vim.fn.strptime('%Y-%m-%dT%H:%M:%S', zulu))
    if parsed and parsed >= 0 then
      return parsed
    end
    return nil
  end

  local base, sign, hours, minutes = normalized:match('^(%d%d%d%d%-%d%d%-%d%dT%d%d:%d%d:%d%d)([+-])(%d%d):?(%d%d)$')
  if base and sign and hours and minutes then
    local parsed = tonumber(vim.fn.strptime('%Y-%m-%dT%H:%M:%S', base))
    if parsed then
      local offset = tonumber(hours) * 3600 + tonumber(minutes) * 60
      if sign == '+' then
        return parsed - offset
      end
      return parsed + offset
    end
  end

  local parsed = tonumber(vim.fn.strptime('%Y-%m-%dT%H:%M:%S', normalized))
  if parsed and parsed >= 0 then
    return parsed
  end
  return nil
end

local function render_session_info(target, live_summary)
  local session_id = target.session_id
  local summary = (live_summary and live_summary.summary) or (target.session and target.session.summary) or nil
  local custom_name = get_session_names().get(session_id)
  local checkpoint_info = target.checkpoint_info or get_checkpoints().session_info(session_id)
  local lines = {
    'Session info:',
    '  Label: ' .. session_label(session_id, summary),
    '  Session ID: ' .. session_id,
  }

  if custom_name and custom_name ~= '' then
    lines[#lines + 1] = '  Saved name: ' .. custom_name
  end
  if type(summary) == 'string' and summary ~= '' and summary ~= custom_name then
    lines[#lines + 1] = '  Summary: ' .. summary
  end

  local session_working_directory = first_non_empty_string(
    live_summary and live_summary.workingDirectory,
    target.session and target.session.workingDirectory,
    checkpoint_info and checkpoint_info.deleted_working_directory,
    session_id == state.session_id and state.session_working_directory or nil
  )
  if session_working_directory then
    lines[#lines + 1] = '  Working directory: ' .. vim.fn.fnamemodify(session_working_directory, ':~')
  end

  local workspace_path = first_non_empty_string(live_summary and live_summary.workspacePath, target.session and target.session.workspacePath)
  if workspace_path then
    lines[#lines + 1] = '  Workspace path: ' .. workspace_path
  end

  local created_at = first_non_empty_string(live_summary and live_summary.createdAt, target.session and target.session.createdAt)
  if created_at then
    lines[#lines + 1] = '  Created: ' .. created_at
  end
  local modified_at = first_non_empty_string(target.session and target.session.modifiedTime, target.session and target.session.startTime)
  if modified_at then
    lines[#lines + 1] = '  Last activity: ' .. modified_at
  end

  lines[#lines + 1] = '  Attached here: ' .. ((session_id == state.session_id) and 'yes' or 'no')
  lines[#lines + 1] = '  Live: ' .. (((live_summary and live_summary.live) or (target.session and target.session.live)) and 'yes' or 'no')

  local model_name = first_non_empty_string(live_summary and live_summary.model, session_id == state.session_id and cfg.active_session_model(state.session_id) or nil)
  if model_name then
    lines[#lines + 1] = '  Model: ' .. model_name
  end
  if live_summary and live_summary.agentMode then
    lines[#lines + 1] = '  Mode: ' .. live_summary.agentMode
  elseif session_id == state.session_id and state.input_mode then
    lines[#lines + 1] = '  Mode: ' .. state.input_mode
  end
  if live_summary and live_summary.permissionMode then
    lines[#lines + 1] = '  Permission: ' .. live_summary.permissionMode
  elseif session_id == state.session_id and state.permission_mode then
    lines[#lines + 1] = '  Permission: ' .. state.permission_mode
  end
  if live_summary and live_summary.agent then
    lines[#lines + 1] = '  Agent: ' .. live_summary.agent
  end

  if checkpoint_info then
    lines[#lines + 1] = '  Checkpoints: ' .. tostring(checkpoint_info.checkpoint_count or 0)
    if checkpoint_info.deleted_at then
      lines[#lines + 1] = '  Soft-deleted: ' .. checkpoint_info.deleted_at
    end
    if checkpoint_info.purge_after then
      lines[#lines + 1] = '  Purge after: ' .. checkpoint_info.purge_after
    end
  end

  if live_summary and live_summary.live then
    lines[#lines + 1] = string.format(
      '  Discovery: %d instructions, %d agents, %d skills, %d MCP servers',
      tonumber(live_summary.instructionCount) or 0,
      tonumber(live_summary.agentCount) or 0,
      tonumber(live_summary.skillCount) or 0,
      tonumber(live_summary.mcpCount) or 0
    )
  end

  append_entry('system', table.concat(lines, '\n'))
end

local function session_info_command(args)
  resolve_session_target(args, { missing_message = 'No active session to inspect' }, function(target, err)
    if err then
      append_entry('error', err)
      return
    end

    if target.session_id == state.session_id then
      get_http().request('GET', '/sessions/' .. target.session_id, nil, function(response, request_err)
        if request_err then
          render_session_info(target, target.session)
          return
        end
        render_session_info(target, response)
      end)
      return
    end

    render_session_info(target, target.session)
  end)
  return true
end

local function session_checkpoints_command(args)
  resolve_session_target(args, { missing_message = 'No active session to inspect checkpoints for' }, function(target, err)
    if err then
      append_entry('error', err)
      return
    end

    local details, checkpoint_info = get_checkpoints().list_details(target.session_id)
    local lines = {
      'Session checkpoints:',
      '  Session: ' .. session_label(target.session_id, target.session and target.session.summary),
    }
    if checkpoint_info and checkpoint_info.deleted_at then
      lines[#lines + 1] = '  Soft-deleted: ' .. checkpoint_info.deleted_at
    end
    if checkpoint_info and checkpoint_info.purge_after then
      lines[#lines + 1] = '  Purge after: ' .. checkpoint_info.purge_after
    end

    if vim.tbl_isempty(details) then
      lines[#lines + 1] = '  No checkpoints recorded.'
      append_entry('system', table.concat(lines, '\n'))
      return
    end

    for _, item in ipairs(details) do
      local header = '  - ' .. tostring(item.id or '<unknown>')
      if item.created_at then
        header = header .. '  ' .. item.created_at
      end
      lines[#lines + 1] = header
      if item.prompt_summary then
        lines[#lines + 1] = '    prompt: ' .. item.prompt_summary
      end
      if item.assistant_summary then
        lines[#lines + 1] = '    reply: ' .. item.assistant_summary
      end
    end

    append_entry('system', table.concat(lines, '\n'))
  end)
  return true
end

local function session_files_command(args)
  resolve_session_target(args, { missing_message = 'No active session to inspect files for' }, function(target, err)
    if err then
      append_entry('error', err)
      return
    end

    local files, file_err = get_checkpoints().list_files(target.session_id)
    if file_err then
      append_entry('error', 'Failed to list session files: ' .. file_err)
      return
    end

    local lines = {
      'Session files:',
      '  Session: ' .. session_label(target.session_id, target.session and target.session.summary),
    }
    if not files or vim.tbl_isempty(files) then
      lines[#lines + 1] = '  No checkpoint snapshot files recorded.'
      append_entry('system', table.concat(lines, '\n'))
      return
    end

    local max_items = 200
    lines[#lines + 1] = '  Files: ' .. tostring(#files)
    for idx = 1, math.min(#files, max_items) do
      lines[#lines + 1] = '  - ' .. files[idx]
    end
    if #files > max_items then
      lines[#lines + 1] = string.format('  … %d more files', #files - max_items)
    end
    append_entry('system', table.concat(lines, '\n'))
  end)
  return true
end

local function session_cleanup_command(args)
  if vim.trim(args or '') ~= '' then
    append_entry('error', 'Usage: /session cleanup')
    return true
  end

  local removed, errors = get_checkpoints().prune_deleted()
  local lines = {
    'Session cleanup:',
    string.format('  Pruned %d deleted checkpoint repo(s)', removed),
  }
  for _, message in ipairs(errors or {}) do
    lines[#lines + 1] = '  - ' .. message
  end
  append_entry('system', table.concat(lines, '\n'))
  return true
end

local function parse_session_prune_args(args)
  local usage = 'Usage: /session prune (--older-than <days> [--include-named] | --keep-last <count> [--session <id>]) [--dry-run]'
  local tokens = vim.split(vim.trim(args or ''), '%s+', { trimempty = true })
  local parsed = {
    dry_run = false,
    include_named = false,
    older_than_days = nil,
    keep_last = nil,
    session_id = nil,
    mode = nil,
  }

  local idx = 1
  while idx <= #tokens do
    local token = tokens[idx]
    if token == '--dry-run' then
      parsed.dry_run = true
    elseif token == '--include-named' then
      parsed.include_named = true
    elseif token == '--older-than' then
      idx = idx + 1
      local value = tonumber(tokens[idx] or '')
      if not value or value < 0 then
        return nil, usage
      end
      parsed.older_than_days = value
    elseif token == '--keep-last' then
      idx = idx + 1
      local value = tonumber(tokens[idx] or '')
      if not value or value < 1 then
        return nil, usage
      end
      parsed.keep_last = math.floor(value)
    elseif token == '--session' then
      idx = idx + 1
      local value = vim.trim(tokens[idx] or '')
      if value == '' then
        return nil, usage
      end
      parsed.session_id = value
    else
      local inline = token:match('^%-%-older%-than=(.+)$')
      if inline then
        local value = tonumber(inline)
        if not value or value < 0 then
          return nil, usage
        end
        parsed.older_than_days = value
      else
        inline = token:match('^%-%-keep%-last=(.+)$')
        if inline then
          local value = tonumber(inline)
          if not value or value < 1 then
            return nil, usage
          end
          parsed.keep_last = math.floor(value)
        else
          inline = token:match('^%-%-session=(.+)$')
          if inline then
            inline = vim.trim(inline)
            if inline == '' then
              return nil, usage
            end
            parsed.session_id = inline
          elseif vim.startswith(token, '--') then
            return nil, usage
          elseif not parsed.session_id then
            parsed.session_id = token
          else
            return nil, usage
          end
        end
      end
    end
    idx = idx + 1
  end

  if parsed.older_than_days ~= nil and parsed.keep_last ~= nil then
    return nil, usage
  end

  if parsed.older_than_days ~= nil then
    parsed.mode = 'sessions'
  elseif parsed.keep_last ~= nil then
    parsed.mode = 'checkpoints'
  else
    return nil, usage
  end

  if parsed.mode == 'sessions' and parsed.session_id then
    return nil, usage
  end
  if parsed.mode == 'checkpoints' and parsed.include_named then
    return nil, usage
  end
  return parsed, nil
end

local function append_session_prune_report(title, opts, candidates, skipped, failures)
  local lines = {
    title,
    string.format('  Older than: %d day(s)', opts.older_than_days),
    '  Include named: ' .. (opts.include_named and 'yes' or 'no'),
    '  Candidates: ' .. tostring(#candidates),
  }

  if skipped.live > 0 then
    lines[#lines + 1] = '  Skipped live sessions: ' .. tostring(skipped.live)
  end
  if skipped.active > 0 then
    lines[#lines + 1] = '  Skipped active sessions: ' .. tostring(skipped.active)
  end
  if skipped.named > 0 then
    lines[#lines + 1] = '  Skipped named sessions: ' .. tostring(skipped.named)
  end
  if skipped.untimed > 0 then
    lines[#lines + 1] = '  Skipped untimed sessions: ' .. tostring(skipped.untimed)
  end

  if vim.tbl_isempty(candidates) then
    lines[#lines + 1] = '  No sessions matched.'
  else
    for _, candidate in ipairs(candidates) do
      local line = '  - ' .. candidate.label
      if candidate.timestamp then
        line = line .. '  ' .. candidate.timestamp
      end
      if candidate.named then
        line = line .. '  [named]'
      end
      lines[#lines + 1] = line
    end
  end

  for _, failure in ipairs(failures or {}) do
    lines[#lines + 1] = '  ! ' .. failure.label .. ' — ' .. failure.error
  end

  append_entry('system', table.concat(lines, '\n'))
end

local function append_checkpoint_prune_report(title, target, parsed, total, result, dry_run)
  local summary = target and target.session and target.session.summary or nil
  local session_id = target and target.session_id or state.session_id or '<unknown>'
  local removed = result and (tonumber(result.removed) or 0) or math.max(total - parsed.keep_last, 0)
  local kept = result and (tonumber(result.kept) or 0) or math.min(total, parsed.keep_last)
  local lines = {
    title,
    '  Mode: checkpoints',
    '  Session: ' .. session_label(session_id, summary),
    '  Keep last: ' .. tostring(parsed.keep_last),
    '  Current checkpoints: ' .. tostring(total),
  }

  if dry_run then
    lines[#lines + 1] = '  Would remove: ' .. tostring(removed)
  else
    lines[#lines + 1] = '  Removed: ' .. tostring(removed)
    lines[#lines + 1] = '  Kept: ' .. tostring(kept)
  end

  if result and result.first_kept then
    lines[#lines + 1] = '  First kept: ' .. tostring(result.first_kept)
  end
  if result and result.last_kept then
    lines[#lines + 1] = '  Last kept: ' .. tostring(result.last_kept)
  end
  if removed == 0 then
    lines[#lines + 1] = '  No checkpoint pruning needed.'
  end

  append_entry('system', table.concat(lines, '\n'))
end

local function session_prune_command(args)
  local parsed, parse_err = parse_session_prune_args(args)
  if parse_err then
    append_entry('error', parse_err)
    return true
  end

  if parsed.mode == 'checkpoints' then
    resolve_session_target(parsed.session_id or '', {
      missing_message = 'No active session to prune checkpoints for',
      allow_raw_id = true,
    }, function(target, err)
      if err then
        append_entry('error', err)
        return
      end

      local checkpoint_info = target.checkpoint_info or get_checkpoints().session_info(target.session_id)
      local total = tonumber(checkpoint_info and checkpoint_info.checkpoint_count) or #(get_checkpoints().list(target.session_id) or {})
      if parsed.dry_run then
        append_checkpoint_prune_report('Session prune preview:', target, parsed, total, nil, true)
        return
      end

      local result, prune_err = get_checkpoints().prune_history(target.session_id, parsed.keep_last)
      if prune_err then
        append_entry('error', 'Failed to prune checkpoints: ' .. tostring(prune_err))
        return
      end

      append_checkpoint_prune_report('Session prune:', target, parsed, total, result, false)
    end)
    return true
  end

  if parsed.mode ~= 'sessions' then
    append_entry('error', 'Unsupported prune mode')
    return true
  end

  fetch_session_catalog(function(sessions, err)
    if err then
      append_entry('error', 'Failed to list sessions: ' .. err)
      return
    end

    local cutoff = os.time() - math.floor(parsed.older_than_days * 24 * 60 * 60)
    local candidates = {}
    local skipped = {
      live = 0,
      active = 0,
      named = 0,
      untimed = 0,
    }

    for _, item in ipairs(sessions) do
      if item.live then
        skipped.live = skipped.live + 1
      elseif state.session_id and item.sessionId == state.session_id then
        skipped.active = skipped.active + 1
      else
        local timestamp_text = first_non_empty_string(item.modifiedTime, item.startTime, item.createdAt)
        local timestamp_unix = parse_session_timestamp(timestamp_text)
        local named = type(get_session_names().get(item.sessionId)) == 'string' and get_session_names().get(item.sessionId) ~= ''
        if named and not parsed.include_named then
          skipped.named = skipped.named + 1
        elseif not timestamp_unix then
          skipped.untimed = skipped.untimed + 1
        elseif timestamp_unix <= cutoff then
          candidates[#candidates + 1] = {
            id = item.sessionId,
            label = session_label(item.sessionId, item.summary),
            timestamp = timestamp_text,
            named = named,
            session = item,
          }
        end
      end
    end

    if parsed.dry_run then
      append_session_prune_report('Session prune preview:', parsed, candidates, skipped, {})
      return
    end
    if vim.tbl_isempty(candidates) then
      append_session_prune_report('Session prune:', parsed, candidates, skipped, {})
      return
    end

    local failures = {}
    local deleted = {}
    local function prune_next(index)
      if index > #candidates then
        append_session_prune_report('Session prune:', parsed, deleted, skipped, failures)
        return
      end

      local candidate = candidates[index]
      get_session().delete_session_by_id(candidate.id, candidate.session, function(delete_err)
        if delete_err then
          failures[#failures + 1] = {
            label = candidate.label,
            error = delete_err,
          }
        else
          deleted[#deleted + 1] = candidate
        end
        prune_next(index + 1)
      end)
    end

    prune_next(1)
  end)
  return true
end

local function session_delete_command(args)
  args = vim.trim(args or '')
  if args == '' then
    get_session().delete_session()
    return true
  end

  fetch_session_catalog(function(sessions, err)
    local picked = nil
    if not err then
      picked = find_session_record(sessions, args)
    end
    local session_id = picked and picked.sessionId or args
    get_session().delete_session_by_id(session_id, picked, function(delete_err)
      if delete_err then
        local message = 'Failed to delete session ' .. utils.format_session_id(session_id) .. ': ' .. delete_err
        append_entry('error', message)
        notify(message, vim.log.levels.ERROR)
      end
    end)
  end)
  return true
end

--- Main /session command dispatcher.
--- @param args string
--- @param delegates table  Functions from the parent module: { rename_session, plan_mode_command, set_input_mode }
local function session_command(args, delegates)
  local action, rest = vim.trim(args or ''):match('^(%S+)%s*(.*)$')
  action = action and action:lower() or ''
  rest = rest or ''

  if action == '' then
    get_session().switch_session()
    return true
  end
  if action == 'new' then
    get_session().new_session()
    return true
  end
  if action == 'clear' then
    get_session().clear_and_new_session()
    return true
  end
  if action == 'info' then
    return session_info_command(rest)
  end
  if action == 'checkpoints' then
    return session_checkpoints_command(rest)
  end
  if action == 'files' then
    return session_files_command(rest)
  end
  if action == 'plan' then
    return delegates.plan_mode_command(rest)
  end
  if action == 'mode' then
    return delegates.set_input_mode(rest)
  end
  if action == 'rename' then
    return delegates.rename_session(rest)
  end
  if action == 'cleanup' then
    return session_cleanup_command(rest)
  end
  if action == 'prune' then
    return session_prune_command(rest)
  end
  if action == 'delete' then
    return session_delete_command(rest)
  end
  get_session().switch_to_session_id(vim.trim(args or ''))
  return true
end

M.merge_session_catalog = merge_session_catalog
M.fetch_session_catalog = fetch_session_catalog
M.session_label = session_label
M.find_session_record = find_session_record
M.resolve_session_target = resolve_session_target
M.parse_session_timestamp = parse_session_timestamp
M.render_session_info = render_session_info
M.session_info_command = session_info_command
M.session_checkpoints_command = session_checkpoints_command
M.session_files_command = session_files_command
M.session_cleanup_command = session_cleanup_command
M.parse_session_prune_args = parse_session_prune_args
M.append_session_prune_report = append_session_prune_report
M.append_checkpoint_prune_report = append_checkpoint_prune_report
M.session_prune_command = session_prune_command
M.session_delete_command = session_delete_command
M.session_command = session_command

return M
