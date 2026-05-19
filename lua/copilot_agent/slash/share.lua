-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

local cfg = require('copilot_agent.config')
local render = require('copilot_agent.render')
local service = require('copilot_agent.service')
local utils = require('copilot_agent.utils')

local state = cfg.state
local notify = cfg.notify
local log = cfg.log
local append_entry = render.append_entry
local split_lines = utils.split_lines
local working_directory = service.working_directory

local M = {}

local function transcript_lines()
  local lines = {}
  local function sanitize_export_text(text)
    if text == nil then
      return ''
    end
    if type(text) ~= 'string' then
      text = tostring(text)
    end
    return text:gsub('%z', ''):gsub('\r\n?', '\n')
  end
  local function fallback_entry_lines(entry)
    local kind = type(entry) == 'table' and entry.kind or 'system'
    local label = ({
      activity = 'Activity',
      assistant = 'Response',
      error = 'Error',
      system = 'System',
      user = 'Prompt',
    })[kind] or 'System'
    local content = sanitize_export_text(type(entry) == 'table' and entry.content or '')
    if kind == 'assistant' and vim.trim(content) == '' then
      return {}
    end
    local entry_lines = { label .. ':' }
    for _, line in ipairs(split_lines(content)) do
      entry_lines[#entry_lines + 1] = '  ' .. sanitize_export_text(line)
    end
    if kind == 'user' and type(entry) == 'table' and type(entry.attachments) == 'table' then
      for _, attachment in ipairs(entry.attachments) do
        local display = sanitize_export_text(attachment.display or attachment.path or attachment.type or '')
        entry_lines[#entry_lines + 1] = '  📎 ' .. display
      end
    end
    entry_lines[#entry_lines + 1] = ''
    return entry_lines
  end
  for idx, entry in ipairs(state.entries) do
    local ok, entry_lines = pcall(render.entry_lines, entry, idx, false)
    if not ok or type(entry_lines) ~= 'table' then
      log(string.format('share export falling back to raw transcript lines for entry %d: %s', idx, tostring(entry_lines)), vim.log.levels.WARN)
      entry_lines = fallback_entry_lines(entry)
    end
    for _, line in ipairs(entry_lines) do
      lines[#lines + 1] = sanitize_export_text(line)
    end
  end
  return lines
end

local function markdown_document()
  return table.concat(transcript_lines(), '\n') .. '\n'
end

local function html_escape(text)
  return (text:gsub('&', '&amp;'):gsub('<', '&lt;'):gsub('>', '&gt;'):gsub('"', '&quot;'))
end

local function html_document()
  local body = html_escape(table.concat(transcript_lines(), '\n'))
  return table.concat({
    '<!DOCTYPE html>',
    '<html lang="en">',
    '<head>',
    '  <meta charset="utf-8">',
    '  <meta name="viewport" content="width=device-width, initial-scale=1">',
    '  <title>Copilot Agent Session Export</title>',
    '  <style>',
    '    body { margin: 0; background: #0d1117; color: #c9d1d9; font: 14px/1.5 ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, monospace; }',
    '    main { max-width: 960px; margin: 0 auto; padding: 24px; }',
    '    pre { white-space: pre-wrap; word-break: break-word; }',
    '  </style>',
    '</head>',
    '<body>',
    '  <main>',
    '    <pre>' .. body .. '</pre>',
    '  </main>',
    '</body>',
    '</html>',
  }, '\n')
end

local function write_export(path, content)
  path = vim.fn.fnamemodify(path, ':p')
  vim.fn.mkdir(vim.fn.fnamemodify(path, ':h'), 'p')
  local f, err = io.open(path, 'w')
  if not f then
    return nil, err
  end
  f:write(content)
  f:close()
  return path
end

local function export_session(format_name, path)
  if vim.tbl_isempty(state.entries) then
    notify('No transcript entries to share', vim.log.levels.INFO)
    return
  end

  local content = format_name == 'html' and html_document() or markdown_document()
  local written, err = write_export(path, content)
  if not written then
    append_entry('error', 'Failed to export session: ' .. tostring(err))
    return
  end
  append_entry('system', 'Session export written to ' .. vim.fn.fnamemodify(written, ':~'))
end

local function default_export_path(format_name)
  local ext = format_name == 'html' and 'html' or 'md'
  return string.format('%s/copilot-session-%s.%s', working_directory(), os.date('%Y%m%d-%H%M%S'), ext)
end

local function resolve_share_request(args)
  local format_name
  local path
  local first, rest = vim.trim(args or ''):match('^(%S+)%s*(.*)$')
  if first == 'html' then
    format_name = 'html'
    path = rest
  elseif first == 'markdown' or first == 'md' or first == 'file' then
    format_name = 'markdown'
    path = rest
  elseif first and first ~= '' then
    path = args
  end
  return format_name, vim.trim(path or '')
end

function M.share_session(args)
  local requested_format, requested_path = resolve_share_request(args)
  local function prompt_path(format_name)
    vim.ui.input({
      prompt = 'Export path: ',
      default = requested_path ~= '' and requested_path or default_export_path(format_name),
      completion = 'file',
    }, function(path)
      path = vim.trim(path or '')
      if path ~= '' then
        export_session(format_name, path)
      end
    end)
  end

  if requested_format then
    -- If the caller provided an explicit path, write the export synchronously and skip
    -- prompting the user (headless environments or programmatic callers expect this).
    if requested_path and requested_path ~= '' then
      export_session(requested_format, requested_path)
      return true
    end
    prompt_path(requested_format)
    return true
  end

  vim.ui.select({
    { id = 'markdown', label = 'Markdown (.md)' },
    { id = 'html', label = 'HTML (.html)' },
  }, {
    prompt = 'Share session as',
    format_item = function(item)
      return item.label
    end,
  }, function(choice)
    if choice then
      vim.schedule(function()
        prompt_path(choice.id)
      end)
    end
  end)
  return true
end

return M
