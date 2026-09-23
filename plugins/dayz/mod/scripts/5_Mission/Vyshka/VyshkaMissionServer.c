// Vyshka DayZ plugin: the hooks into the server mission.
//
// MissionServer.OnInit runs once when the dedicated server has loaded its
// mission, which is the earliest moment the world, the players list, and the
// RestApi are all usable. The plugin starts there and stops with the mission.
// The connect and disconnect hooks feed the player roster; the engine calls
// InvokeOnConnect for a new and for a loaded character alike, and
// InvokeOnDisconnect only once a logout is final (a cancelled logout never
// reaches it), which is exactly the pair a feed wants. The chat event
// reaches the mission through OnEvent, the same dispatcher the engine's own
// client and disconnect events use.
//
// The mission is also where another mod joins in: it overrides
// MissionServer.VyshkaRegister to add its own actions, contexts, events and
// key/value namespaces to the registry this file builds, before the plugin
// starts with it.

// VyshkaMissionDisconnector kicks through the mission's own logout
// finalization: InvokeOnDisconnect (the disconnect event), the character
// save, the body, and only then the engine's disconnect call, exactly what a
// logout timer running out does. The bare disconnect call alone does none of
// that (measured on DayZ 1.29, issue #59).
class VyshkaMissionDisconnector : VyshkaDisconnector
{
	override void Disconnect(PlayerBase player, PlayerIdentity identity)
	{
		MissionServer mission = MissionServer.Cast(GetGame().GetMission());
		if (mission)
			mission.VyshkaFinishLogout(player, identity);
		else
			super.Disconnect(player, identity);
	}
}

class VyshkaBoot
{
	// Start brings the plugin up for one mission. The per-mission state is
	// cleared first, so nothing a mod registers below can be wiped by it;
	// then the registry is filled, by the plugin's own hook and by every mod
	// that overrode it, and handed to the plugin, which publishes it as the
	// first manifest.
	static void Start(MissionServer mission)
	{
		VyshkaModeration.s_Disconnector = new VyshkaMissionDisconnector();
		VyshkaPlayers.Reset();
		VyshkaWorld.Reset();
		VyshkaWeather.Reset();
		VyshkaVehicles.Reset();
		VyshkaFlags.Reset();
		VyshkaMapMarkers.Reset();
		VyshkaBans.Reset();
		VyshkaBans.Load();
		VyshkaCatalog.Reset();
		// Read before the actions register, so the spawn action's schema
		// carries the blocklist from the first manifest.
		VyshkaSpawn.LoadBlocklist();

		VyshkaRegistry registry = new VyshkaRegistry();
		if (mission)
			mission.VyshkaRegister(registry);
		else
			VyshkaLog.Error("the plugin started without a mission, so nothing could register; no action is declared");
		VyshkaPlugin.Start(registry, new VyshkaPlayerSnapshots());
	}
}

modded class MissionServer
{
	override void OnInit()
	{
		super.OnInit();
		VyshkaBoot.Start(this);
	}

	// VyshkaRegister is the hook a mod overrides to add its actions,
	// contexts, events, and key/value namespaces. It runs once, before the
	// plugin starts, so everything registered here is in the first manifest
	// and counts towards the revision derived from it. Call super first, or
	// the plugin's own actions are lost:
	//
	//   modded class MissionServer
	//   {
	//       override void VyshkaRegister(VyshkaRegistry registry)
	//       {
	//           super.VyshkaRegister(registry);
	//           registry.DeclareNamespace("my-mod");
	//           registry.Register(new MyAction());
	//       }
	//   }
	void VyshkaRegister(VyshkaRegistry registry)
	{
		registry.Register(new VyshkaHealAction());
		registry.Register(new VyshkaKickAction());
		registry.Register(new VyshkaBanAction());
		registry.Register(new VyshkaUnbanAction());
		registry.Register(new VyshkaMessageAction());
		registry.Register(new VyshkaBroadcastAction());
		registry.Register(new VyshkaTeleportAction());
		registry.Register(new VyshkaSpawnAction());
		registry.Register(new VyshkaSetTimeAction());
		// The world: weather (with its presets' namespace) and freeze time.
		VyshkaWeather.Register(registry);
		registry.Register(new VyshkaUnstuckAction());
		registry.Register(new VyshkaDeleteDestroyedAction());
		registry.Register(new VyshkaRefuelAction());
		registry.Register(new VyshkaRepairAction());
		registry.Register(new VyshkaVitalsAction());
		registry.Register(new VyshkaStopBleedingAction());
		registry.Register(new VyshkaDryAction());
		registry.Register(new VyshkaBrokenLegsAction());
		registry.Register(new VyshkaBloodyHandsAction());
		registry.Register(new VyshkaFlagsAction());
		registry.Register(new VyshkaInventoryReadAction());
		registry.Register(new VyshkaInventoryStripAction());
		registry.Register(new VyshkaInventoryClearAction());
		// Presets: loadouts, locations, and vehicle presets kept in the
		// store, each kind in a namespace of its own.
		VyshkaPresets.Register(registry);
		// The item catalog: one custom context per item type (spec section
		// 6.2), which the spawn action's className draws on.
		VyshkaCatalog.RegisterContexts(registry);
	}

	override void OnMissionFinish()
	{
		VyshkaPlugin.Stop();
		super.OnMissionFinish();
	}

	override void OnUpdate(float timeslice)
	{
		super.OnUpdate(timeslice);
		VyshkaPlugin.OnFrame(timeslice);
	}

	// VyshkaFinishLogout finalizes a player's logout now, the way the
	// logout timer running out does. A player already counting down a
	// logout (the client left, the timer has not run out) is retired from
	// both logout queues first, exactly as the vanilla timer path removes
	// its entry before finalizing, so the timer cannot finalize the same
	// character a second time. A logout registered in the same tick still
	// has its AddNewPlayerLogout call queued; the override below refuses it.
	void VyshkaFinishLogout(PlayerBase player, PlayerIdentity identity)
	{
		if (m_LogoutPlayers)
			m_LogoutPlayers.Remove(player);
		if (m_NewLogoutPlayers)
			m_NewLogoutPlayers.Remove(player);
		PlayerDisconnected(player, identity, identity.GetId());
	}

	// The vanilla call moves a player from the pending queue to the timed
	// one unconditionally, even when the registration it belongs to was
	// withdrawn before it ran (by a kick here, or by the vanilla logout
	// cancellation, which clears the queues without cancelling the call).
	// Every such stale call is refused, however many are queued: a player
	// no longer pending has nothing to move, and moving a finalized
	// character would have the timer finalize it twice.
	override protected void AddNewPlayerLogout(PlayerBase player, notnull LogoutInfo info)
	{
		if (!m_NewLogoutPlayers || !m_NewLogoutPlayers.Contains(player))
			return;
		super.AddNewPlayerLogout(player, info);
	}

	override void OnEvent(EventType eventTypeId, Param params)
	{
		super.OnEvent(eventTypeId, params);
		if (eventTypeId == ChatMessageEventTypeID)
		{
			ChatMessageEventParams chat = ChatMessageEventParams.Cast(params);
			if (chat)
				VyshkaPlayers.OnChat(chat.param1, chat.param2, chat.param3);
		}
	}

	override void InvokeOnConnect(PlayerBase player, PlayerIdentity identity)
	{
		super.InvokeOnConnect(player, identity);
		VyshkaPlayers.OnConnect(player, identity);
	}

	override void InvokeOnDisconnect(PlayerBase player)
	{
		VyshkaPlayers.OnDisconnect(player);
		super.InvokeOnDisconnect(player);
	}
}
