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
			mission.PlayerDisconnected(player, identity, identity.GetId());
		else
			super.Disconnect(player, identity);
	}
}

class VyshkaBoot
{
	static void Start()
	{
		VyshkaModeration.s_Disconnector = new VyshkaMissionDisconnector();
		VyshkaActionRegistry registry = new VyshkaActionRegistry();
		registry.Register(new VyshkaHealAction());
		registry.Register(new VyshkaKickAction());
		registry.Register(new VyshkaBanAction());
		registry.Register(new VyshkaUnbanAction());
		registry.Register(new VyshkaMessageAction());
		registry.Register(new VyshkaBroadcastAction());
		VyshkaPlayers.Reset();
		VyshkaBans.Reset();
		VyshkaBans.Load();
		VyshkaPlugin.Start(registry, new VyshkaPlayerSnapshots());
	}
}

modded class MissionServer
{
	override void OnInit()
	{
		super.OnInit();
		VyshkaBoot.Start();
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
