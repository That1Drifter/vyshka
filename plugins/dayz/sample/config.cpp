// Vyshka sample mod: addon registration.
//
// The sample depends on nothing but the game. It is a mod that happens to
// use Vyshka when Vyshka is loaded, which is what every mod written against
// the plugin should be: its Vyshka-dependent code sits behind #ifdef VYSHKA,
// the symbol @Vyshka declares for the mods loaded after it, so the same PBO
// also loads on a server that does not run the plugin.
//
// Load it after the plugin: -serverMod=@Vyshka;@VyshkaSample. The other
// order compiles the mod without the VYSHKA symbol and it registers nothing.

class CfgPatches
{
	class VyshkaSample
	{
		units[] = {};
		weapons[] = {};
		requiredVersion = 0.1;
		requiredAddons[] = { "DZ_Data", "DZ_Scripts" };
	};
};

class CfgMods
{
	class VyshkaSample
	{
		dir = "VyshkaSample";
		name = "Vyshka sample mod";
		author = "Vyshka contributors";
		type = "mod";
		dependencies[] = { "World", "Mission" };

		class defs
		{
			class worldScriptModule
			{
				value = "";
				files[] = { "VyshkaSample/scripts/4_World" };
			};

			class missionScriptModule
			{
				value = "";
				files[] = { "VyshkaSample/scripts/5_Mission" };
			};
		};
	};
};
