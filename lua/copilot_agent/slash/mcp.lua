-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local http = require('copilot_agent.http')
local render = require('copilot_agent.render')
local service = require('copilot_agent.service')
local session = require('copilot_agent.session')
local sl = require('copilot_agent.statusline')
local utils = require('copilot_agent.utils')
local window = require('copilot_agent.window')
local is_list = vim.islist

local state = cfg.state
local notify = cfg.notify
local append_entry = render.append_entry
local refresh_statuslines = sl.refresh_statuslines
local split_lines = utils.split_lines
local working_directory = service.working_directory

local function first_non_empty_string(...)
  for i = 1, select('#', ...) do
    local value = select(i, ...)
    if type(value) == 'string' and value ~= '' then
      return value
    end
  end
  return nil
end

local MCP_HEALTH_TIMEOUT_MS = 1500
local MCP_INITIALIZE_PROTOCOL_VERSION = '2024-11-05'

local function invalidate_mcp_completion_cache()
  local input_mod = package.loaded['copilot_agent.input']
  if input_mod and type(input_mod.invalidate_mcp_completion_cache) == 'function' then
    input_mod.invalidate_mcp_completion_cache()
  end
end

local function run_command_capture(args, opts)
  opts = opts or {}
  if vim.system then
    local command_opts = {
      text = true,
      cwd = opts.cwd,
      stdin = opts.stdin,
      timeout = opts.timeout,
      env = opts.env,
    }
    local result = vim.system(args, command_opts):wait()
    return {
      code = tonumber(result.code) or 1,
      stdout = result.stdout or '',
      stderr = result.stderr or '',
    }
  end

  if opts.stdin ~= nil then
    return {
      code = 1,
      stdout = '',
      stderr = 'stdin probes require Neovim vim.system support',
    }
  end

  local output = vim.fn.systemlist(args)
  local code = tonumber(vim.v.shell_error) or 0
  local joined = table.concat(output, '\n')
  if code == 0 then
    return {
      code = 0,
      stdout = joined,
      stderr = '',
    }
  end
  return {
    code = code,
    stdout = '',
    stderr = joined,
  }
end

local function run_command(args, cwd)
  local result = run_command_capture(args, { cwd = cwd })
  local output = vim.trim((result.stdout or '') ~= '' and result.stdout or (result.stderr or ''))
  if result.code ~= 0 then
    return nil, output ~= '' and output or table.concat(args, ' ')
  end
  return output, nil
end

local function open_path(path)
  if type(path) ~= 'string' or path == '' then
    return
  end
  local _, err = window.open_path_safely(path)
  if err then
    notify('Opened with a fallback buffer after :edit failed: ' .. tostring(err), vim.log.levels.WARN)
  end
end

local function mcp_config_paths()
  local wd = working_directory()
  return {
    root = wd .. '/.mcp.json',
    vscode = wd .. '/.vscode/mcp.json',
    global = vim.fn.expand('~/.copilot/mcp-config.json'),
  }
end

local function encode_json_pretty(value)
  if vim.json and type(vim.json.encode) == 'function' then
    return vim.json.encode(value, { indent = '  ' })
  end
  return http.encode_json(value)
end

local function read_mcp_config(path)
  if vim.fn.filereadable(path) ~= 1 then
    return nil, nil
  end

  local decoded, decode_err = http.decode_json(table.concat(vim.fn.readfile(path), '\n'), { log = false })
  if type(decoded) ~= 'table' then
    return nil, 'Failed to parse ' .. vim.fn.fnamemodify(path, ':~:.') .. ': ' .. tostring(decode_err)
  end
  return decoded, nil
end

local function write_mcp_config(path, payload)
  local parent = vim.fn.fnamemodify(path, ':h')
  if parent ~= '' then
    vim.fn.mkdir(parent, 'p')
  end

  local encoded = encode_json_pretty(payload)
  local ok, err = pcall(vim.fn.writefile, vim.split(encoded, '\n', { plain = true }), path)
  if not ok then
    return nil, err
  end
  return true
end

local function load_mcp_sources()
  local sources = {}
  local paths = mcp_config_paths()
  for _, path in ipairs({ paths.root, paths.vscode, paths.global }) do
    local payload, err = read_mcp_config(path)
    if err then
      return nil, err
    end
    if payload then
      sources[#sources + 1] = { path = path, payload = payload }
    end
  end
  return sources, nil
end

local function collect_mcp_entries(sources)
  local entries = {}
  for _, source in ipairs(sources or {}) do
    for _, container in ipairs({ 'mcpServers', 'servers' }) do
      local servers = source.payload[container]
      if type(servers) == 'table' then
        if is_list and is_list(servers) then
          for index, entry in ipairs(servers) do
            local name = nil
            local disabled = false
            local command = nil
            local args = nil
            local url = nil
            local cwd = nil
            local entry_type = nil
            local entry_source = nil
            local source_path = nil
            local env = nil
            if type(entry) == 'string' then
              name = entry
            elseif type(entry) == 'table' then
              name = entry.name or entry.id
              disabled = entry.disabled == true
              command = entry.command
              args = entry.args
              url = entry.url
              cwd = entry.cwd
              entry_type = entry.type
              entry_source = entry.source
              source_path = entry.sourcePath
              env = entry.env
            end
            if type(name) == 'string' and name ~= '' then
              entries[#entries + 1] = {
                name = name,
                path = source.path,
                container = container,
                list = true,
                index = index,
                disabled = disabled,
                command = command,
                args = args,
                url = url,
                cwd = cwd,
                type = entry_type,
                source = entry_source,
                sourcePath = source_path,
                env = env,
              }
            end
          end
        else
          for name, entry in pairs(servers) do
            if type(name) == 'string' and name ~= '' then
              local command = nil
              local args = nil
              local url = nil
              local cwd = nil
              local entry_type = nil
              local entry_source = nil
              local source_path = nil
              local env = nil
              if type(entry) == 'table' then
                command = entry.command
                args = entry.args
                url = entry.url
                cwd = entry.cwd
                entry_type = entry.type
                entry_source = entry.source
                source_path = entry.sourcePath
                env = entry.env
              end
              entries[#entries + 1] = {
                name = name,
                path = source.path,
                container = container,
                list = false,
                key = name,
                disabled = type(entry) == 'table' and entry.disabled == true,
                command = command,
                args = args,
                url = url,
                cwd = cwd,
                type = entry_type,
                source = entry_source,
                sourcePath = source_path,
                env = env,
              }
            end
          end
        end
      end
    end
  end

  table.sort(entries, function(left, right)
    if left.name ~= right.name then
      return left.name < right.name
    end
    if left.path ~= right.path then
      return left.path < right.path
    end
    if left.container ~= right.container then
      return left.container < right.container
    end
    return (left.index or 0) < (right.index or 0)
  end)
  return entries
end

local function find_mcp_entries(entries, target_name)
  local needle = vim.trim(target_name or '')
  if needle == '' then
    return entries
  end

  needle = needle:lower()
  local matches = {}
  for _, entry in ipairs(entries or {}) do
    if entry.name:lower() == needle then
      matches[#matches + 1] = entry
    end
  end
  return matches
end

local function mcp_entry_label(entry)
  local status = entry.disabled and 'disabled' or 'enabled'
  return string.format('%s (%s)  [%s]', entry.name, status, vim.fn.fnamemodify(entry.path, ':~:.'))
end

local function mcp_source_map(sources)
  local map = {}
  for _, source in ipairs(sources or {}) do
    map[source.path] = source
  end
  return map
end

local function ensure_root_mcp_config()
  local path = mcp_config_paths().root
  local payload, err = read_mcp_config(path)
  if err then
    return nil, nil, err
  end
  return path, payload or {}, nil
end

local function mcp_entry_disabled_from_config(name, path)
  if type(name) ~= 'string' or name == '' or type(path) ~= 'string' or path == '' then
    return false
  end

  local decoded = read_mcp_config(path)
  if type(decoded) ~= 'table' then
    return false
  end

  local function entry_disabled(value)
    return type(value) == 'table' and value.disabled == true
  end

  for _, container in ipairs({ 'mcpServers', 'servers' }) do
    local servers = decoded[container]
    if type(servers) == 'table' then
      if is_list and is_list(servers) then
        for _, entry in ipairs(servers) do
          if type(entry) == 'string' and entry == name then
            return false
          elseif type(entry) == 'table' then
            local entry_name = entry.name or entry.id
            if entry_name == name then
              return entry_disabled(entry)
            end
          end
        end
      else
        local entry = servers[name]
        if entry ~= nil then
          return entry_disabled(entry)
        end
      end
    end
  end

  return false
end

local function normalize_mcp_source_path(entry)
  if type(entry.sourcePath) == 'string' and entry.sourcePath ~= '' then
    return entry.sourcePath
  end
  if type(entry.source) == 'string' and entry.source:lower() == 'user' then
    return vim.fn.expand('~/.copilot/mcp-config.json')
  end
  return nil
end

local function mcp_entries_from_cli()
  local output, err = run_command({ 'copilot', 'mcp', 'list', '--json' }, working_directory())
  if err then
    return nil, err
  end

  local decoded, decode_err = http.decode_json(output or '', { log = false })
  if type(decoded) ~= 'table' then
    return nil, 'Failed to decode `copilot mcp list --json`: ' .. tostring(decode_err)
  end

  local servers = decoded.mcpServers
  if type(servers) ~= 'table' then
    return {}, nil
  end

  local entries = {}
  for name, entry in pairs(servers) do
    if type(name) == 'string' and name ~= '' and type(entry) == 'table' then
      local source_path = normalize_mcp_source_path(entry)
      entries[#entries + 1] = {
        name = name,
        type = entry.type,
        source = entry.source,
        sourcePath = source_path,
        command = entry.command,
        args = entry.args,
        cwd = entry.cwd,
        url = entry.url,
        env = entry.env,
        disabled = entry.disabled == true or mcp_entry_disabled_from_config(name, source_path),
      }
    end
  end

  table.sort(entries, function(left, right)
    if left.name ~= right.name then
      return left.name < right.name
    end
    local left_kind = first_non_empty_string(left.type, left.source) or ''
    local right_kind = first_non_empty_string(right.type, right.source) or ''
    if left_kind ~= right_kind then
      return left_kind < right_kind
    end
    local left_detail = first_non_empty_string(left.command, left.url, left.sourcePath, left.cwd) or ''
    local right_detail = first_non_empty_string(right.command, right.url, right.sourcePath, right.cwd) or ''
    return left_detail < right_detail
  end)

  return entries, nil
end

local function mcp_entries_from_sources()
  local sources, source_err = load_mcp_sources()
  if source_err then
    return nil, source_err
  end
  local entries = collect_mcp_entries(sources)
  for _, entry in ipairs(entries) do
    if type(entry.sourcePath) ~= 'string' or entry.sourcePath == '' then
      entry.sourcePath = entry.path
    end
    if type(entry.source) ~= 'string' or entry.source == '' then
      entry.source = 'workspace'
    end
  end
  return entries, nil
end

local function expand_mcp_template(value)
  if type(value) ~= 'string' then
    return value
  end
  local wd = working_directory()
  local home = vim.fn.expand('~')
  local basename = wd ~= '' and vim.fn.fnamemodify(wd, ':t') or ''
  return (value:gsub('%${workspaceFolder}', wd):gsub('%${workspaceRoot}', wd):gsub('%${workspaceFolderBasename}', basename):gsub('%${userHome}', home))
end

local function mcp_probe_line_detail(text)
  if type(text) ~= 'string' then
    return ''
  end
  local trimmed = vim.trim(text)
  if trimmed == '' then
    return ''
  end
  local lines = split_lines(trimmed)
  local first = vim.trim(lines[1] or '')
  return first
end

local function mcp_initialize_probe_payload()
  local payload = http.encode_json({
    jsonrpc = '2.0',
    id = 1,
    method = 'initialize',
    params = {
      protocolVersion = MCP_INITIALIZE_PROTOCOL_VERSION,
      capabilities = {},
      clientInfo = {
        name = 'copilot-agent.nvim',
        version = '0.0.0',
      },
    },
  })
  return string.format('Content-Length: %d\r\n\r\n%s', #payload, payload)
end

local function mcp_probe_stdio(entry)
  local command = expand_mcp_template(entry.command)
  if type(command) ~= 'string' or command == '' then
    return false, 'unhealthy', 'missing command'
  end

  local command_args = { command }
  if type(entry.args) == 'table' then
    for _, value in ipairs(entry.args) do
      command_args[#command_args + 1] = tostring(expand_mcp_template(value))
    end
  end

  local probe_cwd = expand_mcp_template(entry.cwd)
  if type(probe_cwd) ~= 'string' or probe_cwd == '' then
    probe_cwd = working_directory()
  end

  local result = run_command_capture(command_args, {
    cwd = probe_cwd,
    stdin = mcp_initialize_probe_payload(),
    timeout = MCP_HEALTH_TIMEOUT_MS,
    env = type(entry.env) == 'table' and entry.env or nil,
  })
  local output = (result.stdout or '') .. '\n' .. (result.stderr or '')
  if result.code == 124 then
    return false, 'timeout', 'probe timed out'
  end
  if output:find('"jsonrpc"%s*:%s*"2.0"') and output:find('"id"%s*:%s*1') and output:find('"result"%s*:') then
    return true, 'healthy', 'initialize responded'
  end
  if result.code ~= 0 then
    return false, 'unhealthy', mcp_probe_line_detail(output ~= '' and output or ('exit code ' .. tostring(result.code)))
  end
  return false, 'unhealthy', 'no initialize response'
end

local function mcp_probe_http(entry)
  local url = expand_mcp_template(entry.url)
  if type(url) ~= 'string' or url == '' then
    return false, 'unhealthy', 'missing url'
  end

  local timeout_seconds = string.format('%.1f', MCP_HEALTH_TIMEOUT_MS / 1000)
  local result = run_command_capture({
    'curl',
    '--silent',
    '--show-error',
    '--location',
    '--max-time',
    timeout_seconds,
    '-o',
    '/dev/null',
    '-w',
    '%{http_code}',
    url,
  }, {
    timeout = MCP_HEALTH_TIMEOUT_MS,
  })

  local http_code = vim.trim(result.stdout or '')
  if result.code == 124 then
    return false, 'timeout', 'probe timed out'
  end
  if http_code ~= '' and http_code ~= '000' then
    return true, 'healthy', 'http ' .. http_code
  end
  if result.code ~= 0 then
    local detail = mcp_probe_line_detail((result.stderr or '') ~= '' and result.stderr or ('exit code ' .. tostring(result.code)))
    if detail:lower():find('timed out', 1, true) then
      return false, 'timeout', detail
    end
    return false, 'unhealthy', detail
  end
  return false, 'unhealthy', 'connection failed'
end

local function probe_mcp_health(entry)
  if entry.disabled == true then
    return false, 'disabled', 'disabled in config'
  end

  local entry_type = type(entry.type) == 'string' and entry.type:lower() or ''
  local has_command = type(entry.command) == 'string' and entry.command ~= ''
  local has_url = type(entry.url) == 'string' and entry.url ~= ''
  if has_command or entry_type:find('stdio', 1, true) then
    return mcp_probe_stdio(entry)
  end
  if has_url or entry_type:find('http', 1, true) or entry_type:find('sse', 1, true) then
    return mcp_probe_http(entry)
  end
  return false, 'unknown', 'no probe method for type ' .. (entry.type or 'unknown')
end

local function mcp_entry_display_detail(entry)
  if type(entry.command) == 'string' and entry.command ~= '' then
    local detail = entry.command
    if type(entry.args) == 'table' and not vim.tbl_isempty(entry.args) then
      local args = {}
      for _, value in ipairs(entry.args) do
        args[#args + 1] = tostring(value)
      end
      detail = detail .. ' ' .. table.concat(args, ' ')
    end
    return detail
  end
  return first_non_empty_string(entry.url, entry.sourcePath, entry.cwd) or '-'
end

local function mcp_entry_display_kind(entry)
  return first_non_empty_string(entry.type, entry.source) or 'unknown'
end

local function filter_mcp_health_entries(entries, target_name)
  local needle = vim.trim(target_name or '')
  if needle == '' then
    return entries
  end

  needle = needle:lower()
  local matches = {}
  for _, entry in ipairs(entries or {}) do
    if type(entry.name) == 'string' and entry.name:lower() == needle then
      matches[#matches + 1] = entry
    end
  end
  return matches
end

local function mcp_show_command(target_name)
  local entries, err = mcp_entries_from_cli()
  if err then
    entries, err = mcp_entries_from_sources()
  end
  if err then
    append_entry('error', 'Unable to read MCP status: ' .. err)
    return true
  end
  if vim.tbl_isempty(entries) then
    append_entry('system', 'No MCP status available')
    return true
  end

  local selected = filter_mcp_health_entries(entries, target_name)
  if vim.tbl_isempty(selected) then
    append_entry('error', 'Unknown MCP server: ' .. target_name)
    return true
  end

  local lines = { 'MCP server check:' }
  for _, entry in ipairs(selected) do
    local _, _, health_detail = probe_mcp_health(entry)
    local check_detail = health_detail ~= '' and health_detail or 'no probe detail'
    lines[#lines + 1] = string.format('  %-18s %-12s %-30s check: %s', tostring(entry.name), mcp_entry_display_kind(entry), mcp_entry_display_detail(entry), check_detail)
  end

  lines[#lines + 1] = 'Double-check the same MCP server in Copilot CLI; this check is advisory only.'
  append_entry('system', table.concat(lines, '\n'))
  return true
end

local discovery = require('copilot_agent.discovery')

local function matching_item(items, query)
  query = vim.trim(query or ''):lower()
  if query == '' then
    return nil
  end
  for _, item in ipairs(items or {}) do
    local name = (item.name or ''):lower()
    local path = (item.path or ''):lower()
    if name == query or path == query or vim.endswith(path, query) then
      return item
    end
  end
  for _, item in ipairs(items or {}) do
    if (item.name or ''):lower():find(query, 1, true) then
      return item
    end
  end
  return nil
end

local function open_discovered_item(kind, items, args)
  if vim.tbl_isempty(items) then
    notify('No ' .. kind .. ' entries found', vim.log.levels.INFO)
    return true
  end

  args = vim.trim(args or '')
  if args ~= '' then
    local item = matching_item(items, args)
    if not item then
      append_entry('error', 'Unknown ' .. kind .. ': ' .. args)
      return true
    end
    open_path(item.path)
    return true
  end

  vim.ui.select(items, {
    prompt = 'Open ' .. kind,
    format_item = function(item)
      return string.format('%s  [%s]', item.name, vim.fn.fnamemodify(item.path, ':~:.'))
    end,
  }, function(choice)
    if choice then
      open_path(choice.path)
    end
  end)
  return true
end

local function mcp_edit_command(target_name)
  target_name = vim.trim(target_name or '')

  if target_name == '' then
    local items = discovery.mcp_items()
    if not vim.tbl_isempty(items) then
      return open_discovered_item('MCP config', items, '')
    end

    local path, payload, ensure_err = ensure_root_mcp_config()
    if ensure_err then
      append_entry('error', ensure_err)
      return true
    end

    if type(payload.mcpServers) ~= 'table' or (is_list and is_list(payload.mcpServers)) then
      payload.mcpServers = {}
    end
    if vim.fn.filereadable(path) ~= 1 then
      local ok, write_err = write_mcp_config(path, payload)
      if not ok then
        append_entry('error', 'Failed to write ' .. vim.fn.fnamemodify(path, ':~:.') .. ': ' .. tostring(write_err))
        return true
      end
      append_entry('system', 'Created ' .. vim.fn.fnamemodify(path, ':~:.') .. ' for MCP configuration')
    end
    open_path(path)
    return true
  end

  local sources, source_err = load_mcp_sources()
  if source_err then
    append_entry('error', source_err)
    return true
  end

  local entries = find_mcp_entries(collect_mcp_entries(sources), target_name)
  if vim.tbl_isempty(entries) then
    append_entry('error', 'Unknown MCP server: ' .. target_name)
    return true
  end
  if #entries == 1 then
    open_path(entries[1].path)
    return true
  end

  vim.ui.select(entries, {
    prompt = 'Select MCP config to edit',
    format_item = mcp_entry_label,
  }, function(choice)
    if choice then
      open_path(choice.path)
    end
  end)
  return true
end

local function parse_add_mcp_args(args)
  local name, rest = vim.trim(args or ''):match('^(%S+)%s*(.*)$')
  if not name then
    return nil, nil, {}
  end
  local parts = {}
  if vim.trim(rest or '') ~= '' then
    parts = vim.split(vim.trim(rest), '%s+', { trimempty = true })
  end

  local command = parts[1]
  local command_args = {}
  if #parts > 1 then
    for idx = 2, #parts do
      command_args[#command_args + 1] = parts[idx]
    end
  end
  return name, command, command_args
end

local function mcp_add_command(args)
  local name, command, command_args = parse_add_mcp_args(args)
  if not name then
    append_entry('error', 'Usage: /mcp add <name> [command [arg...]]')
    return true
  end

  local sources, source_err = load_mcp_sources()
  if source_err then
    append_entry('error', source_err)
    return true
  end
  local existing = find_mcp_entries(collect_mcp_entries(sources), name)
  if not vim.tbl_isempty(existing) then
    append_entry('error', 'MCP server "' .. name .. '" already exists')
    return true
  end

  local path, payload, ensure_err = ensure_root_mcp_config()
  if ensure_err then
    append_entry('error', ensure_err)
    return true
  end

  local container
  local list_container = false
  if type(payload.mcpServers) == 'table' and not (is_list and is_list(payload.mcpServers)) then
    container = payload.mcpServers
  elseif type(payload.servers) == 'table' then
    container = payload.servers
    list_container = is_list and is_list(container)
  else
    payload.mcpServers = {}
    container = payload.mcpServers
  end

  if list_container then
    local list_entry = { name = name }
    if command then
      list_entry.command = command
      list_entry.args = command_args
    end
    table.insert(container, list_entry)
  else
    local map_entry = {}
    if command then
      map_entry.command = command
      map_entry.args = command_args
    end
    container[name] = map_entry
  end

  local ok, write_err = write_mcp_config(path, payload)
  if not ok then
    append_entry('error', 'Failed to write ' .. vim.fn.fnamemodify(path, ':~:.') .. ': ' .. tostring(write_err))
    return true
  end

  open_path(path)
  append_entry('system', 'Added MCP server "' .. name .. '" to ' .. vim.fn.fnamemodify(path, ':~:.'))
  invalidate_mcp_completion_cache()
  return true
end

local function mcp_delete_command(target_name)
  target_name = vim.trim(target_name or '')
  if target_name == '' then
    append_entry('error', 'Usage: /mcp delete <name>')
    return true
  end

  local sources, source_err = load_mcp_sources()
  if source_err then
    append_entry('error', source_err)
    return true
  end

  local entries = find_mcp_entries(collect_mcp_entries(sources), target_name)
  if vim.tbl_isempty(entries) then
    append_entry('error', 'Unknown MCP server: ' .. target_name)
    return true
  end

  local source_map = mcp_source_map(sources)
  local list_removals = {}
  local changed_paths = {}
  for _, entry in ipairs(entries) do
    local source = source_map[entry.path]
    if source then
      local servers = source.payload[entry.container]
      if type(servers) == 'table' then
        if entry.list then
          local group_key = entry.path .. '\0' .. entry.container
          local group = list_removals[group_key]
          if not group then
            group = { source = source, container = entry.container, indices = {} }
            list_removals[group_key] = group
          end
          group.indices[#group.indices + 1] = entry.index
        else
          servers[entry.key] = nil
          changed_paths[entry.path] = true
        end
      end
    end
  end

  for _, removal in pairs(list_removals) do
    local servers = removal.source.payload[removal.container]
    table.sort(removal.indices, function(left, right)
      return left > right
    end)
    for _, index in ipairs(removal.indices) do
      table.remove(servers, index)
    end
    changed_paths[removal.source.path] = true
  end

  for path in pairs(changed_paths) do
    local ok, write_err = write_mcp_config(path, source_map[path].payload)
    if not ok then
      append_entry('error', 'Failed to write ' .. vim.fn.fnamemodify(path, ':~:.') .. ': ' .. tostring(write_err))
      return true
    end
  end

  append_entry('system', string.format('Deleted MCP server "%s" from %d entr%s', target_name, #entries, #entries == 1 and 'y' or 'ies'))
  invalidate_mcp_completion_cache()
  return true
end

local function mcp_set_disabled(target_name, disabled)
  target_name = vim.trim(target_name or '')
  if target_name == '' then
    append_entry('error', string.format('Usage: /mcp %s <name>', disabled and 'disable' or 'enable'))
    return true
  end

  local sources, source_err = load_mcp_sources()
  if source_err then
    append_entry('error', source_err)
    return true
  end

  local entries = find_mcp_entries(collect_mcp_entries(sources), target_name)
  if vim.tbl_isempty(entries) then
    append_entry('error', 'Unknown MCP server: ' .. target_name)
    return true
  end

  local source_map = mcp_source_map(sources)
  local changed_paths = {}
  for _, entry in ipairs(entries) do
    local source = source_map[entry.path]
    local servers = source and source.payload[entry.container] or nil
    if type(servers) ~= 'table' then
      append_entry('error', 'Invalid MCP config shape in ' .. vim.fn.fnamemodify(entry.path, ':~:.'))
      return true
    end

    if entry.list then
      local current = servers[entry.index]
      if type(current) == 'string' then
        servers[entry.index] = { name = entry.name, disabled = disabled }
      elseif type(current) == 'table' then
        current.disabled = disabled
      else
        append_entry('error', 'Cannot update MCP server "' .. entry.name .. '" in ' .. vim.fn.fnamemodify(entry.path, ':~:.'))
        return true
      end
    else
      local current = servers[entry.key]
      if type(current) ~= 'table' then
        append_entry('error', 'Cannot update MCP server "' .. entry.name .. '" in ' .. vim.fn.fnamemodify(entry.path, ':~:.'))
        return true
      end
      current.disabled = disabled
    end
    changed_paths[entry.path] = true
  end

  for path in pairs(changed_paths) do
    local ok, write_err = write_mcp_config(path, source_map[path].payload)
    if not ok then
      append_entry('error', 'Failed to write ' .. vim.fn.fnamemodify(path, ':~:.') .. ': ' .. tostring(write_err))
      return true
    end
  end

  append_entry('system', string.format('%s MCP server "%s" in %d entr%s', disabled and 'Disabled' or 'Enabled', target_name, #entries, #entries == 1 and 'y' or 'ies'))
  invalidate_mcp_completion_cache()
  return true
end

local function mcp_reload_command(args)
  if vim.trim(args or '') ~= '' then
    append_entry('error', 'Usage: /mcp reload')
    return true
  end
  if state.creating_session then
    append_entry('system', 'Session attach is already in progress')
    return true
  end

  local session_id = state.session_id
  if not session_id then
    append_entry('system', 'No active session. MCP changes will apply when the next session starts.')
    return true
  end

  append_entry('system', 'Reloading MCP config for session ' .. session_id .. '…')
  state.session_id = nil
  state.session_name = nil
  state.session_working_directory = nil
  refresh_statuslines()
  state.creating_session = true

  session.disconnect_session(session_id, false, function(disconnect_err)
    if disconnect_err then
      state.creating_session = false
      append_entry('error', 'MCP reload failed: ' .. disconnect_err)
      return
    end
    session.resume_session(session_id, function(_, resume_err)
      if resume_err then
        append_entry('error', 'MCP reload failed: ' .. resume_err)
        return
      end
      invalidate_mcp_completion_cache()
      append_entry('system', 'Reloaded MCP config for session ' .. session_id)
    end)
  end)

  return true
end

local function mcp_command(args)
  args = vim.trim(args or '')
  if args == '' then
    return mcp_edit_command('')
  end

  local action, rest = args:match('^(%S+)%s*(.*)$')
  action = (action or ''):lower()
  rest = vim.trim(rest or '')

  if action == 'add' then
    return mcp_add_command(rest)
  end
  if action == 'show' then
    return mcp_show_command(rest)
  end
  if action == 'edit' then
    return mcp_edit_command(rest)
  end
  if action == 'delete' then
    return mcp_delete_command(rest)
  end
  if action == 'disable' then
    return mcp_set_disabled(rest, true)
  end
  if action == 'enable' then
    return mcp_set_disabled(rest, false)
  end
  if action == 'reload' then
    return mcp_reload_command(rest)
  end

  return mcp_edit_command(args)
end
local M = {}
M.mcp_command = mcp_command
M.invalidate_mcp_completion_cache = invalidate_mcp_completion_cache
return M
