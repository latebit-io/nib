VERSION = "0.1.0"

local micro = import("micro")
local config = import("micro/config")
local shell = import("micro/shell")
local buffer = import("micro/buffer")
local gotime = import("time")

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
local agent_buf = nil    -- BTScratch buffer for agent pane
local agent_bp = nil     -- BufPane for agent pane
local op_undo_count = 0  -- number of undo events for current pending_op
local agent_cursor_active = false -- whether agent cursor exists in code pane

-------------------------------------------------------------------------------
-- Agent pane
-------------------------------------------------------------------------------

local function open_agent_pane(bp)
    agent_buf = buffer.NewBuffer("", "agent")
    agent_buf.Type.Scratch = true
    agent_buf.Type.Readonly = true
    agent_bp = bp:VSplitBuf(agent_buf)
    -- VSplitBuf gives focus to the new (rightmost) pane; go back to code pane
    agent_bp:PreviousSplit()
end

local function close_agent_pane()
    if agent_bp ~= nil then
        agent_bp:Quit()
        agent_bp = nil
    end
    agent_buf = nil
end

local function agent_pane_append(text)
    if agent_buf == nil then return end
    -- Temporarily allow writes to append
    agent_buf.Type.Readonly = false
    local last_line = agent_buf:LinesNum() - 1
    local last_col = #agent_buf:Line(last_line)
    agent_buf:Insert(buffer.Loc(last_col, last_line), text)
    agent_buf.Type.Readonly = true
    -- Scroll agent pane to bottom without stealing focus
    if agent_bp ~= nil then
        local last_ln = agent_buf:LinesNum() - 1
        agent_bp.Cursor.Y = last_ln
        agent_bp.Cursor.X = 0
    end
end

-------------------------------------------------------------------------------
-- safe_loc clamps to valid buffer bounds to prevent Micro panics
-- when the LLM returns coordinates beyond the actual file size.
-------------------------------------------------------------------------------

local function safe_loc(line, col)
    if code_bp == nil or code_bp.Buf == nil then return buffer.Loc(0, 0) end
    local max_line = code_bp.Buf:LinesNum()
    local l = line - 1
    if l < 0 then l = 0 end
    if l >= max_line then l = max_line - 1 end
    local line_len = #code_bp.Buf:Line(l)
    local c = col - 1
    if c < 0 then c = 0 end
    if c > line_len then c = line_len end
    return buffer.Loc(c, l)
end

-------------------------------------------------------------------------------
-- Agent cursor (in code pane)
-------------------------------------------------------------------------------

-- SpawnCursorAtLoc creates a new cursor at the given Loc (0-indexed).
-- The user's cursor (cursor 0) stays active for keyboard input.
-- The agent cursor is always the last cursor in the list.

local function spawn_agent_cursor(line, col)
    if code_bp == nil then return end
    local target = safe_loc(line, col)
    micro.Log("agent: spawning cursor at col=" .. target.X .. " line=" .. target.Y)
    code_bp:SpawnCursorAtLoc(target)
    agent_cursor_active = true
    micro.Log("agent: cursors after spawn: " .. code_bp.Buf:NumCursors())
    -- Ensure user's cursor stays active for keyboard input
    code_bp.Buf:SetCurCursor(0)
    code_bp:Relocate()
end

local function move_agent_cursor(line, col)
    if not agent_cursor_active or code_bp == nil then return end
    -- Remove old agent cursor and spawn at new location
    local n = code_bp.Buf:NumCursors()
    if n > 1 then
        code_bp.Buf:RemoveCursor(n - 1)
    end
    code_bp:SpawnCursorAtLoc(safe_loc(line, col))
    code_bp.Buf:SetCurCursor(0)
    code_bp:Relocate()
end

local function remove_agent_cursor()
    if not agent_cursor_active or code_bp == nil then return end
    local n = code_bp.Buf:NumCursors()
    if n > 1 then
        code_bp.Buf:RemoveCursor(n - 1)
    end
    agent_cursor_active = false
    code_bp:Relocate()
end

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

-- Generation counter for animated inserts; incremented on each new animation
-- and on stop/disconnect so stale callbacks become no-ops.
local animation_generation = 0

-- Insert text character-by-character with 20ms delays, then call on_done().
-- Each character insert is one undo event so we can undo them all.
-- The code pane is locked (readonly) during animation to prevent user edits
-- from interleaving with agent inserts, which would break undo.
local function animated_insert(line, col, text, on_done)
    text = text or ""
    if #text == 0 then
        if on_done then on_done() end
        return
    end

    animation_generation = animation_generation + 1
    local my_gen = animation_generation
    local cur_line = line
    local cur_col = col
    local pos = 0

    -- Lock code pane to prevent user edits during animation
    if code_bp ~= nil then
        code_bp.Buf.Type.Readonly = true
    end

    local function insert_next_char()
        if my_gen ~= animation_generation then return end
        if code_bp == nil then return end
        pos = pos + 1
        local ch = text:sub(pos, pos)
        -- Temporarily unlock for programmatic insert
        code_bp.Buf.Type.Readonly = false
        code_bp.Buf:Insert(safe_loc(cur_line, cur_col), ch)
        code_bp.Buf.Type.Readonly = true
        op_undo_count = op_undo_count + 1

        if ch == "\n" then
            cur_line = cur_line + 1
            cur_col = 1
        else
            cur_col = cur_col + 1
        end

        -- Move agent cursor to follow
        move_agent_cursor(cur_line, cur_col)

        if pos < #text then
            micro.After(gotime.Millisecond * 20, insert_next_char)
        else
            -- Remove agent cursor before giving control back to the user,
            -- otherwise multi-cursor mode duplicates keystrokes.
            remove_agent_cursor()
            -- Unlock code pane for user editing (approve/reject/edit)
            code_bp.Buf.Type.Readonly = false
            if on_done then on_done() end
        end
    end

    insert_next_char()
end

-- offsets_to_coords converts a (start_idx, end_idx) byte range in `full`
-- to 1-indexed (s_line, s_col, e_line, e_col).
local function offsets_to_coords(full, start_idx, end_idx)
    local line = 1
    local col = 1
    local s_line, s_col, e_line, e_col
    for i = 1, end_idx do
        if i == start_idx then
            s_line = line
            s_col = col
        end
        if i == end_idx then
            e_line = line
            e_col = col
        end
        if full:sub(i, i) == "\n" then
            line = line + 1
            col = 1
        else
            col = col + 1
        end
    end
    if start_idx == 1 and s_line == nil then
        s_line = 1
        s_col = 1
    end
    if end_idx == 0 then
        e_line = s_line
        e_col = s_col
    end
    return s_line, s_col, e_line, e_col
end

-- get_buffer_text returns the full buffer content as a string.
local function get_buffer_text()
    if code_bp == nil or code_bp.Buf == nil then return "" end
    local lines = {}
    for i = 0, code_bp.Buf:LinesNum() - 1 do
        lines[#lines + 1] = code_bp.Buf:Line(i)
    end
    return table.concat(lines, "\n")
end

-- trim_line strips leading and trailing whitespace from a single line.
local function trim_line(s)
    return s:match("^%s*(.-)%s*$")
end

-- normalize_ws collapses all runs of whitespace to a single space and trims.
local function normalize_ws(s)
    return s:match("^%s*(.-)%s*$"):gsub("%s+", " ")
end

-- split_lines splits a string into an array of lines.
local function split_lines(s)
    local result = {}
    for line in s:gmatch("([^\n]*)\n?") do
        result[#result + 1] = line
    end
    -- gmatch produces a trailing empty entry; remove it
    if #result > 0 and result[#result] == "" then
        result[#result] = nil
    end
    return result
end

-- find_text locates a search string in the buffer using multiple strategies:
--   1. Exact match
--   2. Line-trimmed match (ignore leading/trailing whitespace per line)
--   3. Indentation-flexible match (ignore all leading whitespace)
--   4. Block-anchor match (first+last lines anchor, middle lines fuzzy)
-- Returns (start_line, start_col, end_line, end_col) in 1-indexed coords,
-- or nil if not found.
local function find_text(search)
    if code_bp == nil or code_bp.Buf == nil or search == nil or search == "" then
        return nil
    end
    local full = get_buffer_text()

    -- Strategy 1: Exact match
    local start_idx = full:find(search, 1, true)
    if start_idx then
        micro.Log("agent: find_text matched with strategy: exact")
        return offsets_to_coords(full, start_idx, start_idx + #search - 1)
    end

    -- For line-based strategies, split both search and buffer into lines
    local search_lines = split_lines(search)
    local buf_lines = split_lines(full)

    if #search_lines == 0 or #buf_lines == 0 then return nil end

    -- Strategy 2: Line-trimmed match
    -- Each search line is trimmed; find a contiguous run in the buffer where
    -- every trimmed buffer line matches the corresponding trimmed search line.
    local function try_trimmed()
        local first_trimmed = trim_line(search_lines[1])
        if first_trimmed == "" then return nil end
        for bi = 1, #buf_lines - #search_lines + 1 do
            if trim_line(buf_lines[bi]) == first_trimmed then
                local ok = true
                for si = 2, #search_lines do
                    if trim_line(buf_lines[bi + si - 1]) ~= trim_line(search_lines[si]) then
                        ok = false
                        break
                    end
                end
                if ok then return bi end
            end
        end
        return nil
    end

    local match_start = try_trimmed()
    if match_start then
        micro.Log("agent: find_text matched with strategy: line-trimmed")
        local match_end = match_start + #search_lines - 1
        local s_col = 1
        local e_col = #buf_lines[match_end]
        if e_col == 0 then e_col = 1 end
        return match_start, s_col, match_end, e_col
    end

    -- Strategy 3: Indentation-flexible match
    -- Strip all leading whitespace from each line before comparing.
    -- This handles cases where the LLM gets indentation wrong.
    local function try_indent_flex()
        local first_stripped = search_lines[1]:match("^%s*(.*)")
        if first_stripped == "" then return nil end
        for bi = 1, #buf_lines - #search_lines + 1 do
            if buf_lines[bi]:match("^%s*(.*)") == first_stripped then
                local ok = true
                for si = 2, #search_lines do
                    if buf_lines[bi + si - 1]:match("^%s*(.*)") ~= search_lines[si]:match("^%s*(.*)") then
                        ok = false
                        break
                    end
                end
                if ok then return bi end
            end
        end
        return nil
    end

    match_start = try_indent_flex()
    if match_start then
        micro.Log("agent: find_text matched with strategy: indent-flexible")
        local match_end = match_start + #search_lines - 1
        local s_col = 1
        local e_col = #buf_lines[match_end]
        if e_col == 0 then e_col = 1 end
        return match_start, s_col, match_end, e_col
    end

    -- Strategy 4: Block-anchor match
    -- Match first and last lines exactly (trimmed), allow middle lines to
    -- differ as long as the line count matches. This handles the case where
    -- the LLM gets the middle of a block slightly wrong but the boundaries
    -- are correct.
    if #search_lines >= 3 then
        local first_trimmed = trim_line(search_lines[1])
        local last_trimmed = trim_line(search_lines[#search_lines])
        if first_trimmed ~= "" and last_trimmed ~= "" then
            for bi = 1, #buf_lines - #search_lines + 1 do
                if trim_line(buf_lines[bi]) == first_trimmed then
                    local ei = bi + #search_lines - 1
                    if ei <= #buf_lines and trim_line(buf_lines[ei]) == last_trimmed then
                        micro.Log("agent: find_text matched with strategy: block-anchor")
                        local e_col = #buf_lines[ei]
                        if e_col == 0 then e_col = 1 end
                        return bi, 1, ei, e_col
                    end
                end
            end
        end
    end

    return nil
end

local function apply_op(op, on_done)
    if code_bp == nil then return false end
    if op.search == nil or op.search == "" then
        micro.InfoBar():Error("agent: malformed op: missing search")
        return false
    end
    op_undo_count = 0

    local s_line, s_col, e_line, e_col = find_text(op.search)
    if s_line == nil then
        micro.InfoBar():Error("agent: search text not found in buffer")
        micro.Log("agent: search not found: " .. op.search)
        return false
    end

    -- Position agent cursor at the match location
    if not agent_cursor_active then
        spawn_agent_cursor(s_line, s_col)
    else
        move_agent_cursor(s_line, s_col)
    end

    -- Remove the matched text (end_col + 1 to include the last char)
    code_bp.Buf:Remove(safe_loc(s_line, s_col), safe_loc(e_line, e_col + 1))
    op_undo_count = 1

    local replacement = op.replace or ""
    if replacement == "" then
        -- Delete only — no text to insert
        remove_agent_cursor()
        if on_done then on_done() end
        return true
    end

    -- Animated insert of the replacement text
    animated_insert(s_line, s_col, replacement, on_done)
    return true
end

local function undo_pending_op()
    if code_bp == nil then return end
    for _ = 1, op_undo_count do
        code_bp.Buf:UndoOneEvent()
    end
    op_undo_count = 0
end

-------------------------------------------------------------------------------
-- Approval prompt
-------------------------------------------------------------------------------

local function show_approval_prompt()
    if pending_op == nil then return end
    local op = pending_op
    local action = (op.replace == nil or op.replace == "") and "delete" or "replace"
    local desc = string.format(
        "Agent: %s (%s) — approve? (y/n) ",
        action, op.reason or "")
    micro.InfoBar():YNPrompt(desc, function(yes, cancelled)
        if cancelled then
            undo_pending_op()
            send({type = "reject", op_id = op.id})
            micro.InfoBar():Message("agent: cancelled " .. op.id)
            pending_op = nil
            return
        end
        if yes then
            send({type = "approve", op_id = op.id})
            micro.InfoBar():Message("agent: approved — edit freely, then :junto-next")
        else
            undo_pending_op()
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
        if not apply_op(msg.op, function()
            show_approval_prompt()
        end) then
            pending_op = nil
            return
        end
    elseif t == "approved" then
        micro.InfoBar():Message("agent: op " .. msg.op_id .. " applied")
    elseif t == "rejected" then
        micro.InfoBar():Message("agent: op " .. msg.op_id .. " rejected by server")
    elseif t == "error" then
        micro.InfoBar():Error("agent: " .. msg.message)
    elseif t == "token" then
        agent_pane_append(msg.text or "")
    elseif t == "step" then
        local prefix = string.format("\n--- Step %d/%d: ", msg.index or 0, msg.total or 0)
        agent_pane_append(prefix .. (msg.description or "") .. " ---\n")
    elseif t == "context" then
        -- Phase 3+
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
    animation_generation = animation_generation + 1
    -- Ensure code pane is unlocked if animation was in progress
    if code_bp ~= nil then
        code_bp.Buf.Type.Readonly = false
    end
    if pending_op ~= nil then
        undo_pending_op()
        pending_op = nil
    end
    remove_agent_cursor()
    micro.InfoBar():Message("agent: bridge exited")
    bridge_cmd = nil
    bridge_stdout_buf = ""
    sock_path = nil
    close_agent_pane()
end

-------------------------------------------------------------------------------
-- Commands
-------------------------------------------------------------------------------

function agentStart(bp, args)
    if #args < 1 then
        micro.InfoBar():Error("usage: junto <socket-path>")
        return
    end
    if bridge_cmd ~= nil then
        micro.InfoBar():Error("junto: already running. Use junto-stop first.")
        return
    end

    sock_path = args[1]
    code_bp = bp

    open_agent_pane(bp)

    bridge_cmd = shell.JobSpawn("junto-bridge", {sock_path},
        on_bridge_stdout, on_bridge_stderr, on_bridge_exit)

    if bridge_cmd == nil then
        close_agent_pane()
        micro.InfoBar():Error("agent: failed to start bridge. Is junto-bridge in PATH?")
        return
    end
    agent_pane_append("Agent connected to " .. sock_path .. "\n")
    micro.InfoBar():Message("agent: connected to " .. sock_path)
end

function agentStop(bp, args)
    if bridge_cmd == nil then
        micro.InfoBar():Message("agent: not running")
        return
    end
    animation_generation = animation_generation + 1
    if code_bp ~= nil then
        code_bp.Buf.Type.Readonly = false
    end
    if pending_op ~= nil then
        undo_pending_op()
        pending_op = nil
    end
    remove_agent_cursor()
    shell.JobStop(bridge_cmd)
    bridge_cmd = nil
    sock_path = nil
    close_agent_pane()
    micro.InfoBar():Message("agent: stopped")
end

function agentNext(bp, args)
    if bridge_cmd == nil then
        micro.InfoBar():Error("agent: not running")
        return
    end
    local content = ""
    if code_bp ~= nil and code_bp.Buf ~= nil then
        local lines = {}
        for i = 0, code_bp.Buf:LinesNum() - 1 do
            lines[#lines + 1] = code_bp.Buf:Line(i)
        end
        content = table.concat(lines, "\n")
    end
    send({type = "continue", content = content})
    micro.InfoBar():Message("agent: continuing to next step")
end

function agentSend(bp, args)
    if #args < 1 then
        micro.InfoBar():Error("usage: junto-send <goal>")
        return
    end
    if bridge_cmd == nil then
        micro.InfoBar():Error("agent: bridge not running")
        return
    end
    local parts = {}
    for i = 1, #args do
        parts[i] = args[i]
    end
    local goal = table.concat(parts, " ")
    local file_path = ""
    local content = ""
    if code_bp ~= nil and code_bp.Buf ~= nil then
        file_path = code_bp.Buf.Path or ""
        -- Get full buffer contents
        local lines = {}
        for i = 0, code_bp.Buf:LinesNum() - 1 do
            lines[#lines + 1] = code_bp.Buf:Line(i)
        end
        content = table.concat(lines, "\n")
    end
    send({
        type = "start",
        file = file_path,
        content = content,
        goal = goal,
    })
    micro.InfoBar():Message("agent: task sent — " .. goal)
end

function init()
    config.MakeCommand("junto", agentStart, config.NoComplete)
    config.MakeCommand("junto-stop", agentStop, config.NoComplete)
    config.MakeCommand("junto-next", agentNext, config.NoComplete)
    config.MakeCommand("junto-send", agentSend, config.NoComplete)
end
