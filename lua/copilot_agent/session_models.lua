-- Copyright 2026 ray-x. All rights reserved.
-- Use of this source code is governed by an Apache 2.0
-- license that can be found in the LICENSE file.

-- Persist per-session model selections to disk so they survive Neovim restarts.

local M = {}

local cache

local function state_file()
  return vim.fn.stdpath('state') .. '/copilot-agent-session-models.json'
end

local function load_cache()
  if cache then
    return cache
  end

  cache = {}
  local ok, raw = pcall(function()
    local f = io.open(state_file(), 'r')
    if not f then
      return nil
    end
    local data = f:read('*a')
    f:close()
    return data
  end)
  if not ok or type(raw) ~= 'string' or raw == '' then
    return cache
  end

  local ok_decode, decoded = pcall(vim.json.decode, raw)
  if ok_decode and type(decoded) == 'table' then
    cache = decoded
  end
  return cache
end

local function save_cache()
  local path = state_file()
  vim.fn.mkdir(vim.fn.fnamemodify(path, ':h'), 'p')
  local f, err = io.open(path, 'w')
  if not f then
    return nil, err
  end
  f:write(vim.json.encode(load_cache()))
  f:close()
  return true
end

--- Get the persisted model for a session.
---@param session_id string
---@return string|nil
function M.get(session_id)
  if type(session_id) ~= 'string' or session_id == '' then
    return nil
  end
  local value = load_cache()[session_id]
  if type(value) == 'string' and value ~= '' then
    return value
  end
  return nil
end

--- Persist a model for a session.
---@param session_id string
---@param model string|nil
function M.set(session_id, model)
  if type(session_id) ~= 'string' or session_id == '' then
    return nil, 'session_id is required'
  end

  model = type(model) == 'string' and vim.trim(model) or ''
  if model == '' then
    load_cache()[session_id] = nil
  else
    load_cache()[session_id] = model
  end
  return save_cache()
end

--- Prune entries for sessions that no longer exist.
---@param valid_session_ids table list of session IDs to keep
function M.prune(valid_session_ids)
  local keep = {}
  for _, id in ipairs(valid_session_ids or {}) do
    keep[id] = true
  end
  local data = load_cache()
  for id in pairs(data) do
    if not keep[id] then
      data[id] = nil
    end
  end
  save_cache()
end

return M
