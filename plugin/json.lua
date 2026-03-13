-- Minimal JSON decode/encode for Micro Lua plugins.
-- Handles: strings, numbers, booleans, null, objects, arrays.
-- No unicode escape decoding beyond \uXXXX basics.

local json = {}

-- Decode

local function skip_ws(s, i)
    return s:match("^%s*()", i)
end

local function decode_string(s, i)
    -- i points at the opening "
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

local decode_value -- forward declaration

local function decode_object(s, i)
    -- i points at {
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
            return tonumber(num_str), i + #num_str
        end
    end
    error("unexpected character at " .. i .. ": " .. c)
end

function json.decode(s)
    local val, _ = decode_value(s, 1)
    return val
end

-- Encode

local function encode_string(s)
    s = s:gsub('\\', '\\\\')
    s = s:gsub('"', '\\"')
    s = s:gsub('\n', '\\n')
    s = s:gsub('\r', '\\r')
    s = s:gsub('\t', '\\t')
    return '"' .. s .. '"'
end

local encode_value -- forward declaration

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
        if #v > 0 or next(v) == nil then
            -- treat as array if has sequential keys or is empty
            -- check if it's really an array
            local is_arr = true
            local count = 0
            for _ in pairs(v) do count = count + 1 end
            if count == #v then
                return encode_array(v)
            end
        end
        return encode_object(v)
    end
    error("cannot encode type: " .. t)
end

function json.encode(v)
    return encode_value(v)
end

return json
