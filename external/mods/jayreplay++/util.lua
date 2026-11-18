-- REPLAY++ MODULE by jay_ts
-- Utility Functions

local t = {}

function t.normpath(path)
	local components = {}
	for comp in path:gmatch("([^/\\]+)") do
		table.insert(components, comp)
	end
	for k,v in t.backwards(components) do
		if v == "." then
			table.remove(components, k)
		end
	end
	for k,v in t.backwards(components) do
		if v == ".." then
			table.remove(components, k)
			table.remove(components, k-1)
		end
	end
	return table.concat(components, "/")
end

function t.backwards(_t)
	return function(t2, i)
		i=i-1
		if i~=0 then
			return i, t2[i]
		end
	end, _t, #_t+1
end

function t.mergeBase(t_base)
	for k,_ in pairs(t_base) do
		if motif[k] == nil then motif[k] = t_base[k] end
		motif[k] = main.f_tableMerge(t_base[k], motif[k])
	end
end

function t.loadMotifExtensions(path)
	-- inject defaults to motif global, then
	local t_base = loadIni(path)
	t.mergeBase(t_base)

	-- inject our changes
	local extension_path = t.normpath(path)
	local extension = loadIni(extension_path)

	-- localization
	for k,_ in pairs(extension) do
		local locale = extension[gameOption("Config.Language"):lower() .. "." .. k]
		if locale then
			extension[k] = main.f_tableMerge(extension[k], locale)
		end
	end

	if not motif.AnimTable then
		motif.AnimTable = {}
	end

	main.f_tableMerge(motif.AnimTable, loadAnimTable(motif.def, motif.Sff))

	-- inject and load bgdef if needed
	-- and load fade data if needed
	for k, v in pairs(extension) do
		motif[k] = motif[k] or {}
		motif[k] = main.f_tableMerge(motif[k], v)
		local bgdef = k:lower():match("(.*)def")
		local ref = motif[k]
		if bgdef then
			if not ref["Sff"] then
				if type(ref["spr"]) == "string" and t.fileExists(ref["spr"]) then
					ref["Sff"] = sffNew(ref["spr"])
				else
					ref["Sff"] = motif.Sff
				end
			end
			if not ref["BGDef"] then
				ref["BGDef"] = bgNew(
					motif.Sff,
					extension_path,
					bgdef,
					nil
				)
			end
		end
		if ref.fadein then
			if not ref.fadein.FadeData then
				local def = ref.fadein
				local fade = fadeNew({
					["time"] = def.time,
					["color"] = def.col,
					["anim"] = motif.AnimTable[def.anim] and animNew(
						motif.Sff,
						motif.AnimTable[def.anim]
					) or nil,
					["sound"] = def.snd
				})
				ref.fadein.FadeData = fade
			end
		end
		if ref.fadeout then
			if not ref.fadeout.FadeData then
				local def = ref.fadeout
				local fade = fadeNew({
					["time"] = def.time,
					["color"] = def.col,
					["anim"] = motif.AnimTable[def.anim] and animNew(
						motif.Sff,
						motif.AnimTable[def.anim]
					) or nil,
					["sound"] = def.snd
				})
				ref.fadeout.FadeData = fade
			end
		end
	end

	return extension
end

-- chinese mario

return t
