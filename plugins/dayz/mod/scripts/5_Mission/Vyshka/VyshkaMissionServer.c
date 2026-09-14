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

class VyshkaBoot
{
	static void Start()
	{
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
