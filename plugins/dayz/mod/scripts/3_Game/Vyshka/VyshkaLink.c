// Vyshka DayZ plugin: the surface another mod writes against.
//
// Everything a mod needs from the plugin at runtime is here, behind one
// object reached with GetVyshka(): what version is loaded, whether the link
// is up, how to emit an event, how to reach the key/value store, and how to
// put something on the live map. Registration is the other half of the
// surface and happens once, at boot, through the MissionServer.VyshkaRegister
// hook (VyshkaMissionServer.c).
//
// The facade is always there, whether or not the plugin is running: a mod
// loaded on a server with no hub configured still compiles, still runs, and
// gets told what happened rather than crashing. Emit drops the event, Store
// answers its callback with a failure that says the plugin is not connected,
// and Mark places a marker that no snapshot will carry until it is.
//
// A mod that loads after @Vyshka compiles against this under #ifdef VYSHKA,
// the define CfgMods declares (config.cpp), so the same mod also loads on a
// server without the plugin.

class VyshkaLink
{
	protected static ref VyshkaLink s_Link;

	// Instance is the one facade; GetVyshka() is how a mod asks for it.
	static VyshkaLink Instance()
	{
		if (!s_Link)
			s_Link = new VyshkaLink();
		return s_Link;
	}

	// Version is the plugin's version, as the manifest publishes it.
	string Version()
	{
		return VyshkaPlugin.PLUGIN_VERSION;
	}

	// LinkState is stopped, connected, degraded or buffering (spec section
	// 9.4). Buffering and degraded both mean the plugin is keeping what it
	// is given; stopped means it is not running at all.
	string LinkState()
	{
		return VyshkaPlugin.LinkState();
	}

	// IsRunning says whether the plugin booted with a usable config and has
	// not stopped. A mod that wants to know whether its telemetry is going
	// anywhere asks LinkState as well.
	bool IsRunning()
	{
		return VyshkaPlugin.IsRunning();
	}

	// Emit queues one telemetry event (spec section 8.1). t is a
	// {namespace}.{name} type and data the payload object, or null for none.
	// Declaring the type in the manifest (registry.DeclareEvent) is optional
	// and only makes the hub display it better.
	void Emit(string t, VyshkaJsonValue data)
	{
		VyshkaPlugin.Emit(t, data);
	}

	// Store is a handle on one key/value namespace (spec section 12). The
	// namespace must be one the mod declared with
	// registry.DeclareNamespace, or every call through the handle is
	// refused before it reaches the hub.
	VyshkaStore Store(string namespace)
	{
		return new VyshkaStore(namespace);
	}

	// Mark puts something on the live map, or moves it when the id is one
	// already placed (spec section 8.3, state.entities). Use
	// {namespace}:{thing} ids so two mods cannot collide. The returned
	// marker moves, carries new data, and comes off the map again.
	VyshkaMapMarker Mark(string id, string kind, string label, vector position, VyshkaJsonValue data = null)
	{
		return VyshkaMapMarker.Place(id, kind, label, position, data);
	}
}

// GetVyshka is the entry point a mod uses: GetVyshka().IsRunning(), and so
// on. Take the facade into a local before calling more than one thing on it,
// as everything in this plugin does with anything a call returned.
VyshkaLink GetVyshka()
{
	return VyshkaLink.Instance();
}
