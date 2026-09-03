// Vyshka DayZ plugin: addon registration.
//
// CfgPatches declares the addon; CfgMods mounts the script folders into the
// engine's script modules. Protocol code (JSON, outbox, transport, the link
// itself) lives in 3_Game, the game-facing actions in 4_World where
// PlayerBase is visible, and the MissionServer hook in 5_Mission.
//
// This is a server-side mod: load it with -serverMod=@Vyshka. Clients never
// need it and are never asked for it.

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
