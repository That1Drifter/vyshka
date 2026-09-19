// Vyshka DayZ plugin: addon registration.
//
// CfgPatches declares the addon; CfgMods mounts the script folders into the
// engine's script modules. Protocol code (JSON, outbox, transport, the link
// itself) lives in 3_Game, the game-facing actions in 4_World where
// PlayerBase is visible, and the MissionServer hook in 5_Mission.
//
// This is a server-side mod: load it with -serverMod=@Vyshka. Clients never
// need it and are never asked for it.
//
// CfgMods defines[] declares the symbol VYSHKA for every mod loaded after
// this one, so another mod guards its Vyshka-dependent code with
// #ifdef VYSHKA and loads on a server without the plugin as well. A mod that
// wants the symbol must come after @Vyshka in -serverMod=, because the
// engine defines it only for what it loads afterwards; see
// plugins/dayz/sample for one that does.

class CfgPatches
{
	class Vyshka
	{
		units[] = {};
		weapons[] = {};
		requiredVersion = 0.1;
		requiredAddons[] = { "DZ_Data", "DZ_Scripts" };
	};
};

class CfgMods
{
	class Vyshka
	{
		dir = "Vyshka";
		name = "Vyshka";
		author = "Vyshka contributors";
		type = "mod";
		dependencies[] = { "Game", "World", "Mission" };
		defines[] = { "VYSHKA" };

		class defs
		{
			class gameScriptModule
			{
				value = "";
				files[] = { "Vyshka/scripts/3_Game" };
			};

			class worldScriptModule
			{
				value = "";
				files[] = { "Vyshka/scripts/4_World" };
			};

			class missionScriptModule
			{
				value = "";
				files[] = { "Vyshka/scripts/5_Mission" };
			};
		};
	};
};
