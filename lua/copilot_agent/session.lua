-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Session lifecycle: create, resume, disconnect, pick-or-create.

local cfg = require('copilot_agent.config')
local http = require('copilot_agent.http')
local service = require('copilot_agent.service')
local sl = require('copilot_agent.statusline')
local render = require('copilot_agent.render')
local events = require('copilot_agent.events')
local model = require('copilot_agent.model')
local approvals = require('copilot_agent.approvals')
local checkpoints = require('copilot_agent.checkpoints')
local session_names = require('copilot_agent.session_names')
local utils = require('copilot_agent.utils')
local state = cfg.state
local notify = cfg.notify
local log = cfg.log

local request = http.request
local sync_request = http.sync_request

local working_directory = service.working_directory

local refresh_statuslines = sl.refresh_statuslines

local append_entry = render.append_entry
local clear_transcript = render.clear_transcript

local stop_event_stream = events.stop_event_stream
local start_event_stream = events.start_event_stream

local stale_service_hint = model.stale_service_hint
local prompt_supported_model_selection = model.prompt_supported_model_selection
local active_provider = cfg.active_provider
local provider_key = cfg.provider_key
local session_model_key = cfg.session_model_key
local session_provider = cfg.session_provider
local bind_session_provider = cfg.bind_session_provider

local format_session_id = utils.format_session_id
local truncate_session_summary = utils.truncate_session_summary
local unavailable_model_from_error = utils.unavailable_model_from_error

local M = {}
local create_session
local PROVIDER_HANDOFF_MAX_ENTRIES = 8
local PROVIDER_HANDOFF_MAX_CHARS = 240

local function current_provider()
  return type(active_provider) == 'function' and active_provider() or 'copilot'
end

local function set_active_provider(provider)
  local normalized = provider_key(provider)
  if normalized then
    if state.active_provider ~= normalized then
      state.active_provider = normalized
      refresh_statuslines()
    end
  end
  return current_provider()
end

local function session_provider_for(session_id, fallback_provider)
  return type(session_provider) == 'function' and session_provider(session_id, fallback_provider) or current_provider()
end

local function remember_session_provider(session_id, provider)
  local resolved_provider = set_active_provider(provider)
  if type(bind_session_provider) == 'function' then
    bind_session_provider(session_id, resolved_provider)
  end
  return resolved_provider
end

local function cache_session_model(session_id, provider, model_name)
  if type(session_id) ~= 'string' or session_id == '' then
    return
  end
  if type(model_name) ~= 'string' or model_name == '' then
    return
  end
  local key = session_model_key(session_id, session_provider_for(session_id, provider))
  if key then
    state.session_models[key] = model_name
  end
end

local function with_provider_opts(opts, provider)
  local next_opts = vim.tbl_extend('force', {}, opts or {})
  next_opts.provider = provider or current_provider()
  return next_opts
end

local function formatted_session_summary(summary)
  return truncate_session_summary(summary, 32)
end

local function formatted_session_label(summary, session_id)
  local formatted_id = format_session_id(session_id)
  local formatted_summary = formatted_session_summary(session_names.resolve(summary, session_id))
  if formatted_summary ~= '' then
    return formatted_summary .. ' [' .. formatted_id .. ']'
  end
  return formatted_id
end

local function project_picker_label(path)
  if type(path) ~= 'string' or path == '' then
    return 'current project'
  end

  local display = vim.fn.fnamemodify(path, ':~')
  local name = vim.fn.fnamemodify(path, ':t')
  if name == '' or name == '.' or name == display then
    return display
  end
  return string.format('%s (%s)', name, display)
end

local function session_id_of(session)
  return session and session.sessionId or nil
end

local function session_cwd_of(session)
  if not session then
    return nil
  end
  return (session.context and session.context.cwd) or session.workingDirectory or nil
end

local function session_sort_key(session)
  return (session and (session.modifiedTime or session.startTime or session.createdAt)) or ''
end

local function log_session_catalog(context, sessions, target_cwd)
  for _, session in ipairs(sessions or {}) do
    local session_cwd = session_cwd_of(session) or '<none>'
    local summary = formatted_session_summary(session_names.resolve(session.summary, session.sessionId))
    log(
      string.format(
        '%s candidate id=%s live=%s cwd=%s target=%s match=%s summary=%s',
        context,
        format_session_id(session.sessionId),
        tostring(session.live == true),
        session_cwd,
        target_cwd or '<none>',
        tostring(session_cwd == target_cwd),
        summary ~= '' and summary or '<none>'
      ),
      vim.log.levels.DEBUG
    )
  end
end

local function merge_sessions(response)
  local merged = {}
  local order = {}
  local response_provider = provider_key(response and response.provider)

  local function upsert(session, live_source)
    local id = session_id_of(session)
    if not id or id == '' then
      return
    end
    local normalized = vim.deepcopy(session)
    normalized.live = live_source == true
    normalized.provider = provider_key(normalized.provider) or response_provider or current_provider()
    if not merged[id] then
      order[#order + 1] = id
      merged[id] = normalized
      return
    end

    local existing = merged[id]
    if live_source then
      local combined = normalized
      combined.context = combined.context or existing.context
      combined.workingDirectory = combined.workingDirectory or existing.workingDirectory
      combined.summary = combined.summary or existing.summary
      combined.provider = combined.provider or existing.provider
      combined.modifiedTime = combined.modifiedTime or existing.modifiedTime
      combined.startTime = combined.startTime or existing.startTime
      combined.createdAt = combined.createdAt or existing.createdAt
      merged[id] = combined
      return
    end

    existing.provider = existing.provider or normalized.provider
    existing.context = existing.context or normalized.context
    existing.workingDirectory = existing.workingDirectory or normalized.workingDirectory
    existing.summary = existing.summary or normalized.summary
    existing.modifiedTime = existing.modifiedTime or normalized.modifiedTime
    existing.startTime = existing.startTime or normalized.startTime
    existing.createdAt = existing.createdAt or normalized.createdAt
  end

  for _, session in ipairs((response and response.persisted) or {}) do
    upsert(session, false)
  end
  for _, session in ipairs((response and response.live) or {}) do
    upsert(session, true)
  end

  local items = {}
  for _, id in ipairs(order) do
    items[#items + 1] = merged[id]
  end
  return items
end

local request_with_managed_base_url

local function fetch_sorted_sessions(context, callback, opts)
  opts = opts or {}
  local provider = opts.provider or current_provider()
  local request_fn = opts.strict_discovery == true and request_with_managed_base_url or request
  request_fn('GET', '/sessions', nil, function(response, err)
    if err then
      callback(nil, err, response)
      return
    end

    local sessions = merge_sessions(response)
    for _, session in ipairs(sessions) do
      session.provider = provider_key(session.provider) or provider
    end
    log_session_catalog(context, sessions)
    log(string.format('%s persisted=%d live=%d merged=%d', context, #((response and response.persisted) or {}), #((response and response.live) or {}), #sessions), vim.log.levels.INFO)
    table.sort(sessions, function(a, b)
      local ta = session_sort_key(a)
      local tb = session_sort_key(b)
      return ta > tb
    end)
    callback(sessions, nil, response)
  end, with_provider_opts({ auto_start = false }, provider))
end

local function latest_matching_session(sessions, target_cwd)
  local matching = {}
  for _, session in ipairs(sessions or {}) do
    if session_cwd_of(session) == target_cwd then
      matching[#matching + 1] = session
    end
  end

  table.sort(matching, function(a, b)
    return session_sort_key(a) > session_sort_key(b)
  end)

  return matching[1]
end

local function is_missing_session_error(err)
  if type(err) ~= 'string' then
    return false
  end
  return err:lower():find('session not found', 1, true) ~= nil
end

local function is_corrupted_session_error(err)
  if type(err) ~= 'string' then
    return false
  end
  local lowered = err:lower()
  return lowered:find('session file is corrupted', 1, true) ~= nil or lowered:find('session is corrupted', 1, true) ~= nil or lowered:find('corrupted session', 1, true) ~= nil
end

local function is_stale_service_error(err)
  if type(err) ~= 'string' then
    return false
  end
  local lowered = err:lower()
  return lowered:find('cli process exited', 1, true) ~= nil
    or lowered:find('process exited unexpectedly', 1, true) ~= nil
    or lowered:find('client not connected', 1, true) ~= nil
    or lowered:find('client stopped', 1, true) ~= nil
end

-- Delete any temp files (clipboard PNGs) still waiting in pending_attachments.
function M.discard_pending_attachments()
  for _, a in ipairs(state.pending_attachments) do
    if a.temp and a.path then
      pcall(os.remove, a.path)
    end
  end
  state.pending_attachments = {}
  local ok, input_mod = pcall(require, 'copilot_agent.input')
  if ok and type(input_mod) == 'table' and type(input_mod.refresh_attachment_badge) == 'function' then
    input_mod.refresh_attachment_badge()
  end
  refresh_statuslines()
end

local function focus_input_for_active_chat()
  if not (state.chat_winid and vim.api.nvim_win_is_valid(state.chat_winid)) then
    return
  end

  vim.schedule(function()
    if not (state.chat_winid and vim.api.nvim_win_is_valid(state.chat_winid)) then
      return
    end
    require('copilot_agent')._open_input_window()
  end)
end

local function active_session_working_directory()
  return state.session_working_directory or working_directory()
end

local function delete_session_request(session_id, delete_state, callback, opts)
  opts = with_provider_opts(opts)
  request('DELETE', string.format('/sessions/%s%s', session_id, delete_state and '?delete=true' or ''), nil, function(_, err)
    if callback then
      callback(err)
    end
  end, vim.tbl_extend('force', opts, { auto_start = false }))
end

request_with_managed_base_url = function(method, path, body, callback, opts)
  opts = with_provider_opts(opts)
  service.ensure_managed_base_url(function(err, base_url)
    if err then
      callback(nil, err)
      return
    end
    request(
      method,
      path,
      body,
      callback,
      vim.tbl_extend('force', opts, {
        base_url = base_url,
        auto_start = false,
      })
    )
  end)
end

function M.disconnect_session(session_id, delete_state, callback, opts)
  if type(delete_state) == 'table' and opts == nil then
    opts = delete_state
    delete_state = opts.delete_state == true
  end
  stop_event_stream()
  approvals.reset()
  if not session_id then
    if callback then
      callback(nil)
    end
    return
  end

  delete_session_request(session_id, delete_state, callback, opts)
end

local function on_session_ready(session_id, err)
  local should_open_input = session_id and not err and state.open_input_on_session_ready
  state.open_input_on_session_ready = false
  for _, callback in ipairs(state.pending_session_callbacks) do
    callback(session_id, err)
  end
  state.pending_session_callbacks = {}
  if should_open_input then
    focus_input_for_active_chat()
  end
end

local function cancel_attach_attempt(message, callback, opts)
  opts = opts or {}
  state.creating_session = false
  append_entry('system', message)
  if opts.resolve_pending then
    on_session_ready(nil, message)
  end
  if callback then
    callback(nil, message)
  end
end

local function confirm_takeover_if_live(session, callback)
  if not (session and session.live == true) then
    callback('resume')
    return
  end

  local session_label = formatted_session_label(session.summary, session.sessionId)
  vim.ui.select({
    'Keep older instance attached',
    'Kick older instance out',
    'New Session',
  }, {
    prompt = 'Session ' .. session_label .. ' is already attached in another Neovim instance. Kick the older instance out?',
  }, function(choice)
    if choice == 'Kick older instance out' then
      log('resume takeover confirmed for ' .. format_session_id(session.sessionId), vim.log.levels.INFO)
      callback('resume')
      return
    end

    if choice == 'New Session' then
      log('resume takeover creating replacement session instead of resuming ' .. format_session_id(session.sessionId), vim.log.levels.INFO)
      callback('new')
      return
    end

    local message = 'Kept older instance attached; did not connect to session ' .. format_session_id(session.sessionId)
    log('resume takeover declined for ' .. format_session_id(session.sessionId), vim.log.levels.INFO)
    callback('cancel', message)
  end)
end

local function resume_known_session(session, callback, opts)
  opts = opts or {}
  confirm_takeover_if_live(session, function(decision, message)
    if decision == 'new' then
      append_entry('system', 'Creating a new session instead of resuming ' .. format_session_id(session.sessionId))
      create_session(callback, {
        provider = (opts.resume_opts and opts.resume_opts.provider) or session.provider or current_provider(),
      })
      return
    end

    if decision ~= 'resume' then
      cancel_attach_attempt(message, callback, { resolve_pending = opts.resolve_pending })
      return
    end

    if opts.append_message then
      append_entry('system', opts.append_message)
    end
    if opts.log_message then
      log(opts.log_message, vim.log.levels.INFO)
    end
    local resume_opts = vim.tbl_extend('force', {}, opts.resume_opts or {})
    resume_opts.provider = session.provider or resume_opts.provider or current_provider()
    M.resume_session(session.sessionId, callback, resume_opts)
  end)
end

function M.resume_session(session_id, callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or session_provider_for(session_id))
  local requested_wd = working_directory()
  log(string.format('resume_session request id=%s provider=%s cwd=%s', format_session_id(session_id), tostring(provider), requested_wd), vim.log.levels.DEBUG)
  local request_fn = opts.strict_discovery == true and request_with_managed_base_url or request
  request_fn('POST', '/sessions', {
    sessionId = session_id,
    resume = true,
    provider = provider,
    clientId = service.client_id(),
    clientName = state.config.client_name,
    permissionMode = state.permission_mode or state.config.permission_mode,
    workingDirectory = requested_wd,
    streaming = state.config.session.streaming,
    enableConfigDiscovery = state.config.session.enable_config_discovery,
    model = cfg.active_session_model(session_id, provider) or state.config.session.model,
    agent = state.config.session.agent,
  }, function(response, err)
    state.startup_session_discovery = false
    state.creating_session = false
    if opts.guard_current_session_id and state.session_id ~= nil and state.session_id ~= opts.guard_current_session_id then
      local message = 'resume cancelled: active session changed'
      log(
        string.format(
          'resume_session ignored stale response id=%s current=%s expected=%s',
          format_session_id(response and response.sessionId or session_id),
          format_session_id(state.session_id),
          format_session_id(opts.guard_current_session_id)
        ),
        vim.log.levels.DEBUG
      )
      if callback then
        callback(nil, message)
      end
      return
    end
    if err then
      if is_corrupted_session_error(err) and opts.corrupt_recovery_attempted ~= true then
        M.recover_after_corrupted_session(session_id, function(new_session_id, recovery_err)
          if callback then
            callback(new_session_id, recovery_err)
          end
        end)
        return
      end
      if opts.suppress_error_ui ~= true then
        notify('Failed to resume session: ' .. err, vim.log.levels.ERROR)
        append_entry('error', 'Failed to resume session: ' .. err)
      end
      on_session_ready(nil, err)
      if callback then
        callback(nil, err)
      end
      return
    end
    local resumed_session_id = response and response.sessionId or nil
    state.session_id = resumed_session_id
    state.session_working_directory = (response and response.workingDirectory) or requested_wd
    local resumed_provider = remember_session_provider(resumed_session_id, response and response.provider or provider)
    if resumed_session_id and resumed_provider then
      state.provider_sessions[resumed_provider] = resumed_session_id
    end
    if type(response and response.model) == 'string' and response.model ~= '' and resumed_session_id then
      cache_session_model(resumed_session_id, resumed_provider, response.model)
      state.current_model = response.model
    end

    -- Ensure an initial checkpoint exists for the session so diffs have a baseline.
    -- Create or initialize checkpoint repo asynchronously but don't block resume.
    pcall(function()
      if checkpoints and type(checkpoints.ensure_initial_checkpoint) == 'function' then
        pcall(checkpoints.ensure_initial_checkpoint, state.session_id, state.session_working_directory)
      end
    end)

    if not resumed_session_id then
      local message = 'Server did not return a sessionId'
      append_entry('error', message)
      on_session_ready(nil, message)
      if callback then
        callback(nil, message)
      end
      return
    end
    log(
      string.format(
        'resume_session attached id=%s provider=%s requested_cwd=%s response_wd=%s summary=%s',
        format_session_id(state.session_id),
        tostring(resumed_provider),
        requested_wd,
        tostring(response and response.workingDirectory or '<none>'),
        tostring(response and response.summary or '<none>')
      ),
      vim.log.levels.DEBUG
    )
    approvals.reset()
    if type(response and response.model) == 'string' and response.model ~= '' then
      cache_session_model(state.session_id, resumed_provider, response.model)
      state.current_model = response.model
    end
    start_event_stream(state.session_id)
    on_session_ready(state.session_id)
    if callback then
      callback(state.session_id, nil)
    end
  end, with_provider_opts({ auto_start = false }, provider))
end

function M.latest_project_session_sync()
  local wd = working_directory()
  local response, err = sync_request('GET', '/sessions', nil, with_provider_opts())
  if err or type(response) ~= 'table' then
    return nil, err
  end

  local sessions = merge_sessions(response)
  local provider = provider_key(response and response.provider) or current_provider()
  for _, session in ipairs(sessions) do
    session.provider = provider_key(session.provider) or provider
  end
  table.sort(sessions, function(a, b)
    return session_sort_key(a) > session_sort_key(b)
  end)
  log_session_catalog('latest_project_session_sync', sessions, wd)
  return latest_matching_session(sessions, wd), nil
end

local function reset_for_session_switch()
  state.session_id = nil
  state.session_name = nil
  state.session_working_directory = nil
  state.current_model = nil
  state.reasoning_effort = nil
  state.creating_session = true
  M.discard_pending_attachments()
  clear_transcript()
  require('copilot_agent')._ensure_chat_window()
end

local function disconnect_current_session_for_project_attach(callback)
  local previous_session_id = state.session_id
  if not previous_session_id then
    callback(nil)
    return
  end

  reset_for_session_switch()
  M.disconnect_session(previous_session_id, false, function(disconnect_err)
    if disconnect_err then
      append_entry('error', 'Failed to disconnect previous session: ' .. disconnect_err)
      log('attach_latest_project_session_or_create disconnect failed: ' .. tostring(disconnect_err), vim.log.levels.ERROR)
      callback(disconnect_err)
      return
    end
    callback(nil)
  end, { provider = session_provider_for(previous_session_id) })
end

function M.attach_latest_project_session_or_create(callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or current_provider())
  callback = callback or function() end

  local wd = working_directory()
  local active_wd = state.session_working_directory
  if state.session_id and active_wd == wd and session_provider_for(state.session_id, provider) == provider then
    callback(state.session_id, nil)
    return
  end

  log('attach_latest_project_session_or_create provider=' .. tostring(provider) .. ' cwd=' .. tostring(wd), vim.log.levels.INFO)
  fetch_sorted_sessions('attach_latest_project_session_or_create', function(sessions, err)
    if err then
      log('attach_latest_project_session_or_create list failed: ' .. tostring(err), vim.log.levels.WARN)
      disconnect_current_session_for_project_attach(function(disconnect_err)
        if disconnect_err then
          callback(nil, disconnect_err)
          return
        end
        create_session(callback, { provider = provider })
      end)
      return
    end

    local latest = latest_matching_session(sessions, wd)
    if latest then
      disconnect_current_session_for_project_attach(function(disconnect_err)
        if disconnect_err then
          callback(nil, disconnect_err)
          return
        end
        resume_known_session(latest, callback, {
          resolve_pending = true,
          append_message = 'Resuming most recent session ' .. formatted_session_label(latest.summary, latest.sessionId),
          log_message = 'attach_latest_project_session_or_create resume ' .. formatted_session_label(latest.summary, latest.sessionId),
          resume_opts = {
            provider = provider,
          },
        })
      end)
      return
    end

    log('attach_latest_project_session_or_create no matching session; creating new session', vim.log.levels.INFO)
    disconnect_current_session_for_project_attach(function(disconnect_err)
      if disconnect_err then
        callback(nil, disconnect_err)
        return
      end
      create_session(callback, { provider = provider })
    end)
  end, { provider = provider })
end

function M.is_missing_session_error(err)
  return is_missing_session_error(err)
end

function M.is_corrupted_session_error(err)
  return is_corrupted_session_error(err)
end

function M.recover_after_service_restart(unavailable_session_id, callback)
  callback = callback or function() end
  log(string.format('recover_after_service_restart unavailable=%s cwd=%s', format_session_id(unavailable_session_id), tostring(working_directory())), vim.log.levels.WARN)

  local provider = session_provider_for(unavailable_session_id)
  reset_for_session_switch()
  append_entry('system', 'Previous session is unavailable after service restart. Recreating it...')
  create_session(callback, {
    session_id = unavailable_session_id,
    provider = provider,
  })
end

function M.recover_after_corrupted_session(corrupted_session_id, callback)
  callback = callback or function() end
  log(string.format('recover_after_corrupted_session id=%s cwd=%s', format_session_id(corrupted_session_id), tostring(working_directory())), vim.log.levels.WARN)

  local provider = session_provider_for(corrupted_session_id)
  reset_for_session_switch()
  append_entry('system', 'Saved session ' .. format_session_id(corrupted_session_id) .. ' is corrupted. Creating a new session...')
  delete_session_request(corrupted_session_id, true, function(delete_err)
    if delete_err then
      append_entry('system', 'Could not delete corrupted saved session ' .. format_session_id(corrupted_session_id) .. ': ' .. delete_err .. '. Creating a new session anyway.')
      log('recover_after_corrupted_session delete failed for ' .. format_session_id(corrupted_session_id) .. ': ' .. tostring(delete_err), vim.log.levels.WARN)
    end
    create_session(callback, { provider = provider })
  end, { provider = provider })
end

function M.pick_or_create_session(callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or current_provider())
  local wd = working_directory()
  local strict_startup_discovery = state.startup_session_discovery == true
  -- Show a connecting indicator immediately while the async fetch runs.
  append_entry('system', 'Connecting…')
  log('pick_or_create_session provider=' .. tostring(provider) .. ' cwd=' .. tostring(wd), vim.log.levels.INFO)
  fetch_sorted_sessions('pick_or_create_session', function(sessions, err, raw_response)
    if err then
      log('pick_or_create_session list failed: ' .. tostring(err), vim.log.levels.ERROR)
      create_session(callback, {
        strict_discovery = strict_startup_discovery,
        provider = provider,
      })
      return
    end

    strict_startup_discovery = strict_startup_discovery and #sessions == 0
    state.startup_session_discovery = false
    log_session_catalog('pick_or_create_session', sessions, wd)
    local matching = {}
    for _, s in ipairs(sessions) do
      local s_cwd = session_cwd_of(s)
      if s_cwd == wd then
        table.insert(matching, s)
      end
    end
    log(
      string.format(
        'pick_or_create_session cwd=%s persisted=%d live=%d merged=%d matching=%d',
        tostring(wd),
        #((raw_response and raw_response.persisted) or {}),
        #((raw_response and raw_response.live) or {}),
        #sessions,
        #matching
      ),
      vim.log.levels.INFO
    )

    if #matching == 0 then
      log('pick_or_create_session no matching session; creating new session', vim.log.levels.WARN)
      create_session(callback, {
        strict_discovery = strict_startup_discovery,
        provider = provider,
      })
      return
    end

    -- Auto-resume the single match silently — no need to prompt.
    if #matching == 1 then
      local s = matching[1]
      resume_known_session(s, callback, {
        resolve_pending = true,
        append_message = 'Resuming session ' .. formatted_session_label(s.summary, s.sessionId),
        log_message = 'pick_or_create_session resuming single match ' .. formatted_session_label(s.summary, s.sessionId),
      })
      return
    end

    -- Multiple matches: sort newest-first.
    table.sort(matching, function(a, b)
      local ta = session_sort_key(a)
      local tb = session_sort_key(b)
      return ta > tb
    end)

    -- auto_resume='auto': silently resume the most recent without prompting.
    if state.config.session.auto_resume == 'auto' then
      local s = matching[1]
      resume_known_session(s, callback, {
        resolve_pending = true,
        append_message = 'Resuming most recent session ' .. formatted_session_label(s.summary, s.sessionId),
        log_message = 'pick_or_create_session auto-resume ' .. formatted_session_label(s.summary, s.sessionId),
      })
      return
    end

    -- Show a picker; most recent session is listed first (default selection).
    local choices = {}
    for _, s in ipairs(matching) do
      local label = formatted_session_label(s.summary, s.sessionId)
      if label == (s.sessionId or '') then
        local ts = s.modifiedTime or s.startTime or ''
        if ts ~= '' then
          label = label .. ' (' .. ts .. ')'
        end
      end
      table.insert(choices, { label = label, id = s.sessionId, session = s })
    end
    table.insert(choices, { label = 'Create new session', id = nil })
    -- Offer access to sessions from other directories.
    local other_count = #sessions - #matching
    if other_count > 0 then
      table.insert(choices, { label = 'Show all sessions (' .. other_count .. ' from other dirs)…', id = '__all__' })
    end

    local display = vim.tbl_map(function(c)
      return c.label
    end, choices)

    vim.ui.select(display, { prompt = 'Select session for project: ' .. project_picker_label(wd) }, function(_, idx)
      if not idx then
        -- <Esc> dismissed the picker — default to the most recent session.
        local default = choices[1]
        if default.id then
          resume_known_session(default.session, callback, {
            resolve_pending = true,
            append_message = 'Resumed most recent session ' .. format_session_id(default.id) .. ' (picker cancelled)',
            log_message = 'pick_or_create_session picker cancelled; resuming ' .. format_session_id(default.id),
          })
        else
          create_session(callback, {
            strict_discovery = strict_startup_discovery,
            provider = provider,
          })
        end
        return
      end
      local picked = choices[idx]
      if picked.id == '__all__' then
        -- Re-open picker with the full unfiltered list.
        -- Deferred so the first picker fully closes before the second opens
        -- (some picker backends need time to tear down their window).
        vim.defer_fn(function()
          local all_choices = {}
          table.sort(sessions, function(a, b)
            return session_sort_key(a) > session_sort_key(b)
          end)
          for _, s in ipairs(sessions) do
            local label = formatted_session_label(s.summary, s.sessionId)
            local cwd = session_cwd_of(s) or ''
            if cwd ~= '' then
              label = label .. '  ' .. vim.fn.fnamemodify(cwd, ':~')
            end
            table.insert(all_choices, { label = label, id = s.sessionId, session = s })
          end
          table.insert(all_choices, { label = 'Create new session', id = nil })
          local all_display = vim.tbl_map(function(c)
            return c.label
          end, all_choices)
          vim.ui.select(all_display, { prompt = 'All sessions (current project: ' .. project_picker_label(wd) .. ')' }, function(_, idx2)
            if not idx2 then
              local def = all_choices[1]
              if def and def.id then
                resume_known_session(def.session, callback, {
                  resolve_pending = true,
                })
              else
                create_session(callback, {
                  strict_discovery = state.startup_session_discovery == true,
                  provider = provider,
                })
              end
              return
            end
            local p = all_choices[idx2]
            if p.id then
              resume_known_session(p.session, callback, {
                resolve_pending = true,
                append_message = 'Resuming session ' .. format_session_id(p.id),
                log_message = 'pick_or_create_session resumed from all-sessions picker ' .. format_session_id(p.id),
              })
            else
              create_session(callback, {
                strict_discovery = strict_startup_discovery,
                provider = provider,
              })
            end
          end)
        end, 100)
      elseif picked.id then
        resume_known_session(picked.session, callback, {
          resolve_pending = true,
          append_message = 'Resuming session ' .. format_session_id(picked.id),
          log_message = 'pick_or_create_session resumed from matching picker ' .. format_session_id(picked.id),
        })
      else
        create_session(callback, {
          strict_discovery = strict_startup_discovery,
          provider = provider,
        })
      end
    end)
  end, {
    strict_discovery = strict_startup_discovery,
    provider = provider,
  })
end

create_session = function(callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or current_provider())
  local requested_wd = working_directory()
  log(
    string.format(
      'create_session request provider=%s cwd=%s model=%s agent=%s permission=%s',
      tostring(provider),
      requested_wd,
      tostring(state.config.session.model or '<default>'),
      tostring(state.config.session.agent or '<default>'),
      tostring(state.permission_mode or state.config.permission_mode)
    ),
    vim.log.levels.DEBUG
  )
  local request_fn = opts.strict_discovery == true and request_with_managed_base_url or request
  request_fn('POST', '/sessions', {
    sessionId = opts.session_id,
    provider = provider,
    clientId = service.client_id(),
    clientName = state.config.client_name,
    permissionMode = state.permission_mode or state.config.permission_mode,
    workingDirectory = requested_wd,
    streaming = state.config.session.streaming,
    enableConfigDiscovery = state.config.session.enable_config_discovery,
    model = cfg.active_session_model(opts.session_id, provider) or state.config.session.model,
    agent = state.config.session.agent,
  }, function(response, err)
    state.startup_session_discovery = false
    state.creating_session = false
    if err then
      if is_stale_service_error(err) and opts.service_restart_attempted ~= true then
        append_entry('system', 'Copilot service lost its embedded CLI; reconnecting to the shared service and retrying.')
        service.ensure_service_live(function(start_err)
          if start_err then
            local message = 'Failed to reconnect service: ' .. start_err
            notify(message, vim.log.levels.ERROR)
            append_entry('error', message)
            on_session_ready(nil, start_err)
            return
          end
          state.creating_session = true
          local retry_opts = vim.tbl_extend('force', {}, opts, { service_restart_attempted = true })
          create_session(callback, retry_opts)
        end)
        return
      end
      local um = unavailable_model_from_error(err)
      local hint = stale_service_hint(um)
      if hint then
        notify(hint, vim.log.levels.ERROR)
        append_entry('error', hint)
        append_entry('error', 'Failed to create session: ' .. err)
        on_session_ready(nil, err)
        return
      end
      if um and state.config.session.model == um and opts.model_selection_attempts ~= false then
        append_entry('system', string.format('Model "%s" is unavailable; choose a supported model.', um))
        prompt_supported_model_selection(um, 'Select a supported model', function(reselected_model, prompt_err)
          if prompt_err then
            notify('Failed to create session: ' .. prompt_err, vim.log.levels.ERROR)
            append_entry('error', 'Failed to create session: ' .. prompt_err)
            on_session_ready(nil, prompt_err)
            return
          end
          state.config.session.model = reselected_model
          state.current_model = reselected_model
          append_entry('system', 'Retrying session creation with model ' .. reselected_model)
          state.creating_session = true
          create_session(callback, {
            provider = provider,
            model_selection_attempts = false,
          })
        end)
        return
      end
      notify('Failed to create session: ' .. err, vim.log.levels.ERROR)
      append_entry('error', 'Failed to create session: ' .. err)
      log('create_session failed: ' .. tostring(err), vim.log.levels.ERROR)
      on_session_ready(nil, err)
      return
    end

    state.session_id = response and response.sessionId or nil
    state.session_working_directory = (response and response.workingDirectory) or requested_wd
    if not state.session_id then
      local message = 'Server did not return a sessionId'
      append_entry('error', message)
      log(message, vim.log.levels.ERROR)
      on_session_ready(nil, message)
      return
    end

    local created_provider = remember_session_provider(state.session_id, response and response.provider or provider)
    if created_provider and state.session_id then
      state.provider_sessions[created_provider] = state.session_id
    end

    if type(response and response.model) == 'string' and response.model ~= '' then
      cache_session_model(state.session_id, created_provider, response.model)
      state.current_model = response.model
    end

    approvals.reset()
    start_event_stream(state.session_id)

    -- Announce the new session.
    local wd = (response.workingDirectory and response.workingDirectory ~= '') and vim.fn.fnamemodify(response.workingDirectory, ':~') or vim.fn.fnamemodify(working_directory(), ':~')
    local name = formatted_session_summary(session_names.resolve(response.summary, state.session_id))
    local formatted_id = format_session_id(state.session_id)
    local msg = 'New session created' .. '  id:' .. formatted_id .. (name ~= '' and ('  name:' .. name) or '') .. '  dir:' .. wd
    append_entry('system', msg)
    log(msg, vim.log.levels.INFO)
    log(
      string.format(
        'create_session attached id=%s provider=%s requested_cwd=%s response_wd=%s workspace=%s summary=%s',
        formatted_id,
        tostring(created_provider),
        requested_wd,
        tostring(response and response.workingDirectory or '<none>'),
        tostring(response and response.workspacePath or '<none>'),
        name ~= '' and name or '<none>'
      ),
      vim.log.levels.DEBUG
    )

    -- Sync the agent mode with the server if the user already picked one.
    if state.input_mode and state.input_mode ~= 'agent' then
      require('copilot_agent')._set_agent_mode(state.input_mode)
    end
    on_session_ready(state.session_id)
    if callback then
      callback(state.session_id)
    end
  end, with_provider_opts({ auto_start = false }, provider))
end

-- Force-create a brand-new session, bypassing any pick/resume logic.
function M.create_new_session(callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or current_provider())
  table.insert(state.pending_session_callbacks, callback or function() end)
  if state.creating_session then
    return
  end
  state.creating_session = true
  create_session(callback, {
    strict_discovery = false,
    provider = provider,
  })
end

function M.with_session(callback, opts)
  opts = opts or {}
  local provider = set_active_provider(opts.provider or current_provider())
  if state.session_id and session_provider_for(state.session_id, provider) == provider then
    if opts.open_input_on_session_ready then
      focus_input_for_active_chat()
    end
    callback(state.session_id)
    return
  end

  table.insert(state.pending_session_callbacks, callback)
  if opts.open_input_on_session_ready then
    state.open_input_on_session_ready = true
    state.startup_session_discovery = true
  end
  if state.creating_session then
    return
  end

  state.creating_session = true
  M.pick_or_create_session(nil, { provider = provider })
end

local function provider_descriptor_list()
  local providers = {}
  for _, item in pairs(state.provider_cache or {}) do
    providers[#providers + 1] = item
  end
  table.sort(providers, function(left, right)
    local ln = tostring(left.name or '')
    local rn = tostring(right.name or '')
    return ln < rn
  end)
  return providers
end

local function normalize_provider_item(item)
  if type(item) ~= 'table' then
    return nil
  end
  local name = provider_key(item.name or item.provider or item.id)
  if not name then
    return nil
  end
  return {
    name = name,
    type = vim.trim(tostring(item.type or name)),
    default_model = vim.trim(tostring(item.defaultModel or item.default_model or '')),
  }
end

function M.fetch_providers(callback)
  callback = callback or function() end
  request('GET', '/providers', nil, function(response, err)
    if err then
      callback(nil, err, response)
      return
    end

    local cache = {}
    for _, item in ipairs((response and response.providers) or {}) do
      local normalized = normalize_provider_item(item)
      if normalized then
        cache[normalized.name] = normalized
      end
    end

    if vim.tbl_isempty(cache) then
      local fallback = current_provider()
      cache[fallback] = {
        name = fallback,
        type = fallback,
        default_model = '',
      }
    end

    state.provider_cache = cache
    local configured = provider_key(state.config.providers and state.config.providers.active or nil)
    local default_provider = provider_key(response and response.default)
    local selected = provider_key(state.active_provider) or configured or default_provider
    if not selected or not cache[selected] then
      selected = default_provider
    end
    if not selected or not cache[selected] then
      selected = next(cache)
    end
    set_active_provider(selected)

    callback(provider_descriptor_list(), nil, response)
  end, { auto_start = false })
end

local function recent_handoff_entries()
  local collected = {}
  for idx = #state.entries, 1, -1 do
    local entry = state.entries[idx]
    if type(entry) == 'table' and (entry.kind == 'user' or entry.kind == 'assistant') then
      local text = vim.trim(tostring(entry.content or '')):gsub('%s+', ' ')
      if text ~= '' then
        if #text > PROVIDER_HANDOFF_MAX_CHARS then
          text = text:sub(1, PROVIDER_HANDOFF_MAX_CHARS - 3) .. '...'
        end
        table.insert(collected, 1, string.format('- %s: %s', entry.kind == 'user' and 'User' or 'Assistant', text))
        if #collected >= PROVIDER_HANDOFF_MAX_ENTRIES then
          break
        end
      end
    end
  end
  return collected
end

local function build_provider_handoff_text(source_provider, target_provider, source_session_id)
  local providers_cfg = (state.config and state.config.providers) or {}
  if providers_cfg.handoff_on_switch == false then
    return nil
  end
  if not source_session_id then
    return nil
  end
  if provider_key(source_provider) == provider_key(target_provider) then
    return nil
  end

  local lines = {
    string.format('Provider handoff context from "%s" to "%s":', tostring(source_provider), tostring(target_provider)),
    '- Previous session: ' .. format_session_id(source_session_id),
    '- Working directory: ' .. vim.fn.fnamemodify(state.session_working_directory or working_directory(), ':~'),
    '- Recent transcript summary:',
  }

  local transcript_lines = recent_handoff_entries()
  if vim.tbl_isempty(transcript_lines) then
    lines[#lines + 1] = '- (No recent transcript entries available.)'
  else
    vim.list_extend(lines, transcript_lines)
  end
  lines[#lines + 1] = '- Use this context as background for the next user request in the new provider session.'
  return table.concat(lines, '\n')
end

local function switch_to_provider_name(target_provider)
  target_provider = provider_key(target_provider)
  if not target_provider then
    notify('Provider name is required', vim.log.levels.WARN)
    return
  end

  local previous_session_id = state.session_id
  local previous_provider = session_provider_for(previous_session_id, current_provider())
  local already_attached = previous_session_id and provider_key(previous_provider) == target_provider
  if already_attached then
    notify('Already on provider ' .. target_provider, vim.log.levels.INFO)
    return
  end

  local remembered_target_session = state.provider_sessions[target_provider]
  local handoff_text = build_provider_handoff_text(previous_provider, target_provider, previous_session_id)
  if previous_session_id and previous_provider then
    state.provider_sessions[previous_provider] = previous_session_id
  end

  local function continue_switch()
    if handoff_text then
      state.pending_session_context = {
        provider = target_provider,
        session_id = nil,
        text = handoff_text,
      }
      append_entry('system', 'Queued context handoff for provider "' .. target_provider .. '".')
    else
      state.pending_session_context = nil
    end

    if type(remembered_target_session) == 'string' and remembered_target_session ~= '' then
      append_entry('system', 'Switching provider to "' .. target_provider .. '" and resuming session ' .. format_session_id(remembered_target_session) .. '…')
      M.resume_session(remembered_target_session, function(_, resume_err)
        if not resume_err then
          return
        end
        if is_missing_session_error(resume_err) then
          append_entry('system', 'Remembered session ' .. format_session_id(remembered_target_session) .. ' is unavailable; creating a new session.')
          state.creating_session = true
          M.pick_or_create_session(nil, { provider = target_provider })
          return
        end
        append_entry('error', 'Failed to resume provider session: ' .. resume_err)
      end, {
        provider = target_provider,
        suppress_error_ui = true,
      })
      return
    end

    append_entry('system', 'Switching provider to "' .. target_provider .. '"…')
    M.pick_or_create_session(nil, { provider = target_provider })
  end

  reset_for_session_switch()
  set_active_provider(target_provider)
  if not previous_session_id then
    continue_switch()
    return
  end

  M.disconnect_session(previous_session_id, false, function(disconnect_err)
    if disconnect_err then
      append_entry('error', 'Failed to disconnect previous session: ' .. disconnect_err)
      log('switch_provider disconnect failed: ' .. tostring(disconnect_err), vim.log.levels.ERROR)
    end
    continue_switch()
  end, { provider = previous_provider })
end

function M.switch_provider(provider_name)
  local requested = provider_key(provider_name)
  M.fetch_providers(function(providers, err)
    if err then
      notify('Failed to list providers: ' .. err, vim.log.levels.ERROR)
      return
    end
    if vim.tbl_isempty(providers or {}) then
      notify('No providers configured.', vim.log.levels.WARN)
      return
    end

    if requested then
      for _, provider in ipairs(providers) do
        if provider.name == requested then
          switch_to_provider_name(provider.name)
          return
        end
      end
      notify('Unknown provider: ' .. requested, vim.log.levels.ERROR)
      return
    end

    vim.ui.select(providers, {
      prompt = 'Switch provider',
      format_item = function(item)
        local marker = item.name == current_provider() and ' ●' or ''
        local model_hint = item.default_model ~= '' and (' model=' .. item.default_model) or ''
        return string.format('%s (%s)%s%s', item.name, item.type, model_hint, marker)
      end,
    }, function(choice)
      if choice then
        switch_to_provider_name(choice.name)
      end
    end)
  end)
end

function M.complete_provider(arglead)
  local leading = provider_key(arglead)
  local names = {}
  for name in pairs(state.provider_cache or {}) do
    if not leading or leading == '' or vim.startswith(name, leading) then
      names[#names + 1] = name
    end
  end
  table.sort(names)
  return names
end

-- ── High-level session operations (moved from init.lua) ──────────────────────

--- Create a new session, disconnecting the previous one.
function M.new_session(opts)
  opts = opts or {}
  local previous_session_id = state.session_id
  local provider = set_active_provider(opts.provider or current_provider())
  local previous_provider = session_provider_for(previous_session_id, provider)
  state.session_id = nil
  state.session_name = nil
  state.session_working_directory = nil
  M.discard_pending_attachments()
  clear_transcript()
  -- Ensure chat window via lazy require to avoid circular dependency.
  require('copilot_agent')._ensure_chat_window()
  M.disconnect_session(previous_session_id, false, function(err)
    if err then
      append_entry('error', 'Failed to disconnect previous session: ' .. err)
      return
    end
    M.create_new_session(function(session_id, create_err)
      if create_err then
        append_entry('error', create_err)
        return
      end
      append_entry('system', 'Created session ' .. session_id)
    end, { provider = provider })
  end, { provider = previous_provider })
end

function M.clear_and_new_session()
  local previous_session_id = state.session_id
  local previous_session_name = state.session_name
  local previous_working_directory = state.session_working_directory
  M.discard_pending_attachments()
  clear_transcript()
  require('copilot_agent')._ensure_chat_window()

  local function create_replacement()
    M.create_new_session(function(session_id, create_err)
      if create_err then
        append_entry('error', create_err)
        return
      end
      append_entry('system', 'Created session ' .. session_id)
    end, { provider = current_provider() })
  end

  if not previous_session_id then
    create_replacement()
    return
  end

  M.delete_session_by_id(previous_session_id, {
    sessionId = previous_session_id,
    summary = previous_session_name,
    workingDirectory = previous_working_directory or working_directory(),
  }, function(err)
    if err then
      append_entry('error', 'Failed to clear previous session: ' .. err)
      return
    end
    create_replacement()
  end)
end

function M.switch_to_session_id(target_session_id, target_session)
  target_session_id = vim.trim(target_session_id or '')
  if target_session_id == '' then
    notify('Session ID is required', vim.log.levels.WARN)
    return
  end
  local target_provider = provider_key(target_session and target_session.provider) or session_provider_for(target_session_id, current_provider())
  if state.session_id and target_session_id == state.session_id and session_provider_for(state.session_id, current_provider()) == target_provider then
    notify('Already on this session', vim.log.levels.INFO)
    return
  end

  local function perform_switch()
    local previous_session_id = state.session_id
    local previous_provider = session_provider_for(previous_session_id, current_provider())
    state.session_id = nil
    state.session_name = nil
    state.session_working_directory = nil
    state.creating_session = true
    M.discard_pending_attachments()
    clear_transcript()
    require('copilot_agent')._ensure_chat_window()
    M.disconnect_session(previous_session_id, false, function(disconnect_err)
      if disconnect_err then
        append_entry('error', 'Failed to disconnect previous session: ' .. disconnect_err)
        log('switch_to_session_id disconnect failed: ' .. tostring(disconnect_err), vim.log.levels.ERROR)
      end
      set_active_provider(target_provider)
      append_entry('system', 'Switching to session ' .. format_session_id(target_session_id) .. ' on provider "' .. target_provider .. '"…')
      log('switch_to_session_id switching to ' .. format_session_id(target_session_id) .. ' provider=' .. tostring(target_provider), vim.log.levels.INFO)
      M.resume_session(target_session_id, nil, { provider = target_provider })
    end, { provider = previous_provider })
  end

  if target_session then
    confirm_takeover_if_live(target_session, function(decision, message)
      if decision == 'new' then
        M.new_session({ provider = target_provider })
        return
      end

      if decision ~= 'resume' then
        append_entry('system', message)
        return
      end
      perform_switch()
    end)
    return
  end

  fetch_sorted_sessions('switch_to_session_id', function(sessions, err)
    if err then
      perform_switch()
      return
    end

    local found_session = nil
    for _, session in ipairs(sessions) do
      if session.sessionId == target_session_id then
        found_session = session
        break
      end
    end

    confirm_takeover_if_live(found_session, function(decision, message)
      if decision == 'new' then
        M.new_session({ provider = target_provider })
        return
      end

      if decision ~= 'resume' then
        append_entry('system', message)
        return
      end
      perform_switch()
    end)
  end, { provider = target_provider })
end

--- Show a picker of all persisted sessions and switch to the selected one.
function M.switch_session()
  fetch_sorted_sessions('switch_session', function(sessions, err)
    if err then
      notify('Failed to list sessions: ' .. err, vim.log.levels.ERROR)
      return
    end
    if #sessions == 0 then
      notify('No sessions found. Use :CopilotAgentNewSession to create one.', vim.log.levels.INFO)
      return
    end

    local choices = {}
    for _, s in ipairs(sessions) do
      local label = formatted_session_label(s.summary, s.sessionId)
      if label == (s.sessionId or '') then
        local ts = session_sort_key(s)
        if ts ~= '' then
          label = label .. ' (' .. ts .. ')'
        end
      end
      local cwd_label = ''
      local session_cwd = session_cwd_of(s)
      if session_cwd then
        cwd_label = '  ' .. vim.fn.fnamemodify(session_cwd, ':~')
      end
      -- Mark the currently active session.
      local active = (state.session_id and s.sessionId == state.session_id) and ' ●' or ''
      table.insert(choices, { label = label .. cwd_label .. active, id = s.sessionId, session = s })
    end
    table.insert(choices, { label = '+ New session', id = nil })

    local display = vim.tbl_map(function(c)
      return c.label
    end, choices)

    vim.ui.select(display, { prompt = 'Switch session' }, function(_, idx)
      if not idx then
        return
      end
      local picked = choices[idx]
      if not picked.id then
        M.new_session()
        return
      end
      M.switch_to_session_id(picked.id, picked.session)
    end)
  end, { provider = current_provider() })
end

local function delete_session_label(session)
  local parts = {}
  local summary = formatted_session_summary(session_names.resolve(session.summary, session.sessionId))
  if summary ~= '' then
    parts[#parts + 1] = summary
  end
  parts[#parts + 1] = '[' .. (session.sessionId or '') .. ']'

  local session_cwd = session_cwd_of(session)
  if session_cwd then
    parts[#parts + 1] = vim.fn.fnamemodify(session_cwd, ':~')
  end

  local label = table.concat(parts, '  ')
  local entry_provider = provider_key(session and session.provider) or session_provider_for(session and session.sessionId, current_provider())
  if state.session_id and session.sessionId == state.session_id and session_provider_for(state.session_id, current_provider()) == entry_provider then
    label = label .. ' ●'
  end
  return label
end

local function soft_delete_checkpoint_repo(session_id, opts, callback)
  checkpoints.soft_delete_session(session_id, opts, function(checkpoint_err)
    if checkpoint_err then
      local message = 'Deleted session ' .. format_session_id(session_id) .. ', but failed to retain its checkpoint repo: ' .. checkpoint_err
      append_entry('error', message)
      notify(message, vim.log.levels.WARN)
    end
    if callback then
      callback(checkpoint_err)
    end
  end)
end

function M.delete_session_by_id(target_session_id, session_record, callback)
  callback = callback or function() end
  target_session_id = vim.trim(target_session_id or '')
  if target_session_id == '' then
    callback('Session ID is required')
    return
  end

  local target_provider = provider_key(session_record and session_record.provider) or session_provider_for(target_session_id, current_provider())
  local active_session = state.session_id and target_session_id == state.session_id and session_provider_for(state.session_id, current_provider()) == target_provider
  local current_session_name = active_session and state.session_name or nil
  local current_working_directory = active_session and active_session_working_directory() or nil
  local checkpoint_opts = {
    session_name = current_session_name or session_names.resolve(session_record and session_record.summary, target_session_id),
    working_directory = current_working_directory or session_cwd_of(session_record),
  }
  local formatted_id = format_session_id(target_session_id)

  local function finish_delete()
    append_entry('system', 'Deleted session ' .. formatted_id)
    notify('Deleted session ' .. formatted_id, vim.log.levels.INFO)
    soft_delete_checkpoint_repo(target_session_id, checkpoint_opts, function(checkpoint_err)
      callback(nil, checkpoint_err)
    end)
  end

  if active_session then
    state.session_id = nil
    state.session_name = nil
    state.session_working_directory = nil
    M.discard_pending_attachments()
    clear_transcript()
    M.disconnect_session(target_session_id, true, function(err)
      if err then
        callback(err)
        return
      end
      finish_delete()
    end, { provider = target_provider })
    return
  end

  delete_session_request(target_session_id, true, function(err)
    if err then
      callback(err)
      return
    end
    finish_delete()
  end, { provider = target_provider })
end

function M.delete_session()
  fetch_sorted_sessions('delete_session', function(sessions, err)
    if err then
      notify('Failed to list sessions: ' .. err, vim.log.levels.ERROR)
      return
    end
    if #sessions == 0 then
      notify('No sessions found to delete.', vim.log.levels.INFO)
      return
    end

    local choices = {}
    for _, session in ipairs(sessions) do
      table.insert(choices, {
        id = session.sessionId,
        label = delete_session_label(session),
        session = session,
      })
    end
    local display = vim.tbl_map(function(choice)
      return choice.label
    end, choices)

    vim.ui.select(display, { prompt = 'Delete session' }, function(_, idx)
      if not idx then
        return
      end

      local picked = choices[idx]
      M.delete_session_by_id(picked.id, picked.session, function(delete_err)
        if delete_err then
          local message = 'Failed to delete session ' .. format_session_id(picked.id) .. ': ' .. delete_err
          append_entry('error', message)
          notify(message, vim.log.levels.ERROR)
        end
      end)
    end)
  end, { provider = current_provider() })
end

--- Disconnect the active session.
function M.stop(delete_state)
  if not state.session_id then
    append_entry('system', 'No active session')
    return
  end

  if delete_state then
    local session_id = state.session_id
    local session_summary = state.session_name
    local provider = session_provider_for(session_id, current_provider())
    M.delete_session_by_id(session_id, {
      sessionId = session_id,
      summary = session_summary,
      provider = provider,
      workingDirectory = active_session_working_directory(),
    }, function(err)
      if err then
        append_entry('error', 'Failed to delete session: ' .. err)
      end
    end)
    return
  end

  local session_id = state.session_id
  local provider = session_provider_for(session_id, current_provider())
  state.session_id = nil
  state.session_working_directory = nil
  M.discard_pending_attachments()
  clear_transcript()
  M.disconnect_session(session_id, delete_state, function(err)
    if err then
      append_entry('error', 'Failed to disconnect session: ' .. err)
      return
    end
    append_entry('system', 'Disconnected session ' .. session_id)
  end, { provider = provider })
end

--- Cancel the current in-progress turn.
function M.cancel()
  if not state.session_id then
    notify('No active session to cancel', vim.log.levels.WARN)
    return
  end
  local session_id = state.session_id
  local provider = session_provider_for(session_id, current_provider())
  request('POST', '/sessions/' .. session_id .. '/abort', {}, function(_, err)
    if err then
      append_entry('error', 'Cancel failed: ' .. err)
      return
    end
    log('cancel request acknowledged for session ' .. tostring(session_id), vim.log.levels.DEBUG)
  end, with_provider_opts({ auto_start = false }, provider))
end

M._on_session_ready = on_session_ready
M._focus_input_for_active_chat = focus_input_for_active_chat

return M
