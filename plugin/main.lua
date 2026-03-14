VERSION = "0.1.0"

local micro = import("micro")
local config = import("micro/config")
local shell = import("micro/shell")
local buffer = import("micro/buffer")

-------------------------------------------------------------------------------
-- Minimal JSON decode/encode
-------------------------------------------------------------------------------

local json = {}

local function skip_ws(s, i)
    return s:match("^%s*()", i)
end

local function decode_string(s, i)
    local j = i + 1
    local parts = {}
    while j <= #s do
        local c = s:sub(j, j)
        if c == '"' then
            return table.concat(parts), j + 1
        elseif c == '\\' then
            j = j + 1
            local esc = s:sub(j, j)
            if esc == '"' then parts[#parts+1] = '"'
            elseif esc == '\\' then parts[#parts+1] = '\\'
            elseif esc == '/' then parts[#parts+1] = '/'
            elseif esc == 'n' then parts[#parts+1] = '\n'
            elseif esc == 't' then parts[#parts+1] = '\t'
            elseif esc == 'r' then parts[#parts+1] = '\r'
            elseif esc == 'b' then parts[#parts+1] = '\b'
            elseif esc == 'f' then parts[#parts+1] = '\f'
            elseif esc == 'u' then
                local hex = s:sub(j+1, j+4)
                j = j + 4
                local cp = tonumber(hex, 16)
                if cp and cp < 128 then
                    parts[#parts+1] = string.char(cp)
                else
                    parts[#parts+1] = "\\u" .. hex
                end
            end
            j = j + 1
        else
            parts[#parts+1] = c
            j = j + 1
        end
    end
    error("unterminated string at " .. i)
end

local decode_value

local function decode_object(s, i)
    local obj = {}
    i = skip_ws(s, i + 1)
    if s:sub(i, i) == '}' then return obj, i + 1 end
    while true do
        i = skip_ws(s, i)
        if s:sub(i, i) ~= '"' then error("expected string key at " .. i) end
        local key
        key, i = decode_string(s, i)
        i = skip_ws(s, i)
        if s:sub(i, i) ~= ':' then error("expected ':' at " .. i) end
        i = skip_ws(s, i + 1)
        local val
        val, i = decode_value(s, i)
        obj[key] = val
        i = skip_ws(s, i)
        local c = s:sub(i, i)
        if c == '}' then return obj, i + 1 end
        if c ~= ',' then error("expected ',' or '}' at " .. i) end
        i = i + 1
    end
end

local function decode_array(s, i)
    local arr = {}
    i = skip_ws(s, i + 1)
    if s:sub(i, i) == ']' then return arr, i + 1 end
    while true do
        local val
        val, i = decode_value(s, i)
        arr[#arr+1] = val
        i = skip_ws(s, i)
        local c = s:sub(i, i)
        if c == ']' then return arr, i + 1 end
        if c ~= ',' then error("expected ',' or ']' at " .. i) end
        i = skip_ws(s, i + 1)
    end
end

decode_value = function(s, i)
    i = skip_ws(s, i)
    local c = s:sub(i, i)
    if c == '"' then return decode_string(s, i)
    elseif c == '{' then return decode_object(s, i)
    elseif c == '[' then return decode_array(s, i)
    elseif c == 't' then
        if s:sub(i, i+3) == "true" then return true, i + 4 end
    elseif c == 'f' then
        if s:sub(i, i+4) == "false" then return false, i + 5 end
    elseif c == 'n' then
        if s:sub(i, i+3) == "null" then return nil, i + 4 end
    else
        local num_str = s:match("^-?%d+%.?%d*[eE]?[+-]?%d*", i)
        if num_str then
            local num = tonumber(num_str)
            if not num then error("invalid number at " .. i .. ": " .. num_str) end
            return num, i + #num_str
        end
    end
    error("unexpected character at " .. i .. ": " .. c)
end

function json.decode(s)
    local val, i = decode_value(s, 1)
    i = skip_ws(s, i)
    if i <= #s then
        error("trailing characters at " .. i)
    end
    return val
end

local function encode_string(s)
    s = s:gsub('\\', '\\\\')
    s = s:gsub('"', '\\"')
    s = s:gsub('\n', '\\n')
    s = s:gsub('\r', '\\r')
    s = s:gsub('\t', '\\t')
    s = s:gsub('\b', '\\b')
    s = s:gsub('\f', '\\f')
    -- Escape any remaining control characters (bytes < 0x20)
    s = s:gsub('[%z\1-\31]', function(c)
        return string.format('\\u%04X', c:byte())
    end)
    return '"' .. s .. '"'
end

local encode_value

local function encode_array(arr)
    local parts = {}
    for i = 1, #arr do
        parts[i] = encode_value(arr[i])
    end
    return '[' .. table.concat(parts, ',') .. ']'
end

local function encode_object(obj)
    local parts = {}
    for k, v in pairs(obj) do
        parts[#parts+1] = encode_string(tostring(k)) .. ':' .. encode_value(v)
    end
    return '{' .. table.concat(parts, ',') .. '}'
end

encode_value = function(v)
    local t = type(v)
    if v == nil then return 'null'
    elseif t == 'boolean' then return tostring(v)
    elseif t == 'number' then return tostring(v)
    elseif t == 'string' then return encode_string(v)
    elseif t == 'table' then
        local count = 0
        for _ in pairs(v) do count = count + 1 end
        if count == #v then
            return encode_array(v)
        end
        return encode_object(v)
    end
    error("cannot encode type: " .. t)
end

function json.encode(v)
    return encode_value(v)
end

-------------------------------------------------------------------------------
-- Plugin state
-------------------------------------------------------------------------------

local bridge_cmd = nil   -- running bridge process
local sock_path = nil    -- socket path for this session
local pending_op = nil   -- current pending EditOp from server
local code_bp = nil      -- BufPane where code edits happen

-------------------------------------------------------------------------------
-- Bridge communication
-------------------------------------------------------------------------------

local function send(msg)
    if bridge_cmd == nil then
        micro.InfoBar():Error("agent: bridge not running")
        return
    end
    shell.JobSend(bridge_cmd, json.encode(msg) .. "\n")
end

-------------------------------------------------------------------------------
-- Op application
-------------------------------------------------------------------------------

-- Protocol uses 1-indexed line/col; buffer.Loc takes 0-indexed (col, line).
local function loc(line, col)
    return buffer.Loc(col - 1, line - 1)
end

local function apply_op(op)
    if code_bp == nil then return end
    if op.line == nil or op.col == nil then
        micro.InfoBar():Error("agent: malformed op: missing line/col")
        return
    end
    if op.kind == "insert" then
        code_bp.Buf:Insert(loc(op.line, op.col), op.text or "")
    elseif op.kind == "replace" then
        if op.end_line == nil or op.end_col == nil then
            micro.InfoBar():Error("agent: malformed replace op: missing end_line/end_col")
            return
        end
        local start = loc(op.line, op.col)
        local finish = loc(op.end_line, op.end_col)
        code_bp.Buf:Remove(start, finish)
        code_bp.Buf:Insert(start, op.text or "")
    elseif op.kind == "delete" then
        if op.end_line == nil or op.end_col == nil then
            micro.InfoBar():Error("agent: malformed delete op: missing end_line/end_col")
            return
        end
        code_bp.Buf:Remove(loc(op.line, op.col), loc(op.end_line, op.end_col))
    else
        micro.InfoBar():Error("agent: unknown op kind: " .. tostring(op.kind))
    end
end

local function undo_op(op)
    if code_bp == nil then return end
    if op ~= nil and op.kind == "replace" then
        -- replace is Remove + Insert = two undo events
        code_bp.Buf:UndoOneEvent()
        code_bp.Buf:UndoOneEvent()
    else
        code_bp.Buf:UndoOneEvent()
    end
end

-------------------------------------------------------------------------------
-- Approval prompt
-------------------------------------------------------------------------------

local function show_approval_prompt()
    if pending_op == nil then return end
    local op = pending_op
    local desc = string.format(
        "Agent: %s at line %d (%s) — approve? (y/n) ",
        op.kind, op.line, op.reason or "")
    micro.InfoBar():YNPrompt(desc, function(yes, cancelled)
        if cancelled then
            undo_op(op)
            send({type = "reject", op_id = op.id})
            micro.InfoBar():Message("agent: cancelled " .. op.id)
            pending_op = nil
            return
        end
        if yes then
            send({type = "approve", op_id = op.id})
            micro.InfoBar():Message("agent: approved " .. op.id)
        else
            undo_op(op)
            send({type = "reject", op_id = op.id})
            micro.InfoBar():Message("agent: rejected " .. op.id)
        end
        pending_op = nil
    end)
end

-------------------------------------------------------------------------------
-- Message dispatch
-------------------------------------------------------------------------------

local function handle_message(msg)
    local t = msg.type
    if t == "pending_op" then
        pending_op = msg.op
        apply_op(msg.op)
        show_approval_prompt()
    elseif t == "approved" then
        micro.InfoBar():Message("agent: op " .. msg.op_id .. " applied")
    elseif t == "rejected" then
        micro.InfoBar():Message("agent: op " .. msg.op_id .. " rejected by server")
    elseif t == "error" then
        micro.InfoBar():Error("agent: " .. msg.message)
    elseif t == "token" or t == "step" or t == "context" then
        -- Phase 2+
    end
end

-- Buffer for partial stdout lines; stdout can be chunked arbitrarily.
local bridge_stdout_buf = ""

local function on_bridge_stdout(output)
    bridge_stdout_buf = bridge_stdout_buf .. output
    while true do
        local nl = bridge_stdout_buf:find("\n", 1, true)
        if not nl then break end
        local line = bridge_stdout_buf:sub(1, nl - 1):gsub("\r$", "")
        bridge_stdout_buf = bridge_stdout_buf:sub(nl + 1)
        if line ~= "" then
            local ok, msg = pcall(json.decode, line)
            if ok and type(msg) == "table" then
                handle_message(msg)
            else
                micro.Log("agent: bad JSON from bridge: " .. line)
            end
        end
    end
end

local function on_bridge_stderr(output)
    micro.Log("agent bridge stderr: " .. output)
end

local function on_bridge_exit()
    if pending_op ~= nil then
        undo_op(pending_op)
        pending_op = nil
    end
    micro.InfoBar():Message("agent: bridge exited")
    bridge_cmd = nil
    bridge_stdout_buf = ""
    sock_path = nil
end

-------------------------------------------------------------------------------
-- Commands
-------------------------------------------------------------------------------

function agentStart(bp, args)
    if #args < 1 then
        micro.InfoBar():Error("usage: agent-start <socket-path>")
        return
    end
    if bridge_cmd ~= nil then
        micro.InfoBar():Error("agent: already running. Use agent-stop first.")
        return
    end

    sock_path = args[1]
    code_bp = bp

    bridge_cmd = shell.JobSpawn("junto-bridge", {sock_path},
        on_bridge_stdout, on_bridge_stderr, on_bridge_exit)

    if bridge_cmd == nil then
        micro.InfoBar():Error("agent: failed to start bridge. Is junto-bridge in PATH?")
        return
    end
    micro.InfoBar():Message("agent: connected to " .. sock_path)
end

function agentStop(bp, args)
    if bridge_cmd == nil then
        micro.InfoBar():Message("agent: not running")
        return
    end
    if pending_op ~= nil then
        undo_op(pending_op)
        pending_op = nil
    end
    shell.JobStop(bridge_cmd)
    bridge_cmd = nil
    sock_path = nil
    micro.InfoBar():Message("agent: stopped")
end

function agentSend(bp, args)
    if #args < 1 then
        micro.InfoBar():Error("usage: agent-send <json>")
        return
    end
    if bridge_cmd == nil then
        micro.InfoBar():Error("agent: bridge not running")
        return
    end
    shell.JobSend(bridge_cmd, table.concat(args, " ") .. "\n")
    micro.InfoBar():Message("agent: sent")
end

function init()
    config.MakeCommand("agent-start", agentStart, config.NoComplete)
    config.MakeCommand("agent-stop", agentStop, config.NoComplete)
    config.MakeCommand("agent-send", agentSend, config.NoComplete)
end
