// Vyshka DayZ plugin: the moderation actions (issue #59).
//
// Kick, ban, unban, message, and broadcast: what a moderator does daily.
// Each is one manifest entry (spec section 6) plus the code that runs when
// the hub dispatches it (section 7). A player-context action takes the
// player's plain Steam64 id as its referenceKey, the same identity the
// telemetry publishes (section 8.2); kick, message, and the ban's own kick
// need the player online, ban and unban do not.
//
// Everything here uses what the engine gives every server: DisconnectPlayer
// is the only way script removes a client, the notification RPC and the
// chat line are vanilla client features, and a ban is the plugin's own
// record (VyshkaBans) because the engine has no scripted ban list.

class VyshkaModeration
{
	static const int MAX_REASON = 200;
	static const int MAX_MESSAGE = 1000;
	static const int MAX_TITLE = 100;
	static const int SECONDS_DEFAULT = 10;
	static const int SECONDS_MIN = 1;
	static const int SECONDS_MAX = 60;
	static const string STYLE_NOTIFICATION = "notification";
	static const string STYLE_CHAT = "chat";

	// Kick emits core.player.kick and asks the engine to drop the client.
	// The identity is read before the call, because the engine's own
	// disconnect path (which then runs as for any logout, disconnect event
	// included) may let go of it. cause is "action" for a dispatched kick
	// and "ban" for a banned identity refused at connect.
	static bool Kick(PlayerBase player, string reason, string cause, string actionId, out string error)
	{
		PlayerIdentity identity = player.GetIdentity();
		if (!identity)
		{
			error = "the player has no identity attached and cannot be kicked";
			return false;
		}
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", VyshkaPlayers.Identity(identity.GetPlainId()));
		data.Set("name", VyshkaJsonValue.NewString(identity.GetName()));
		if (reason != "")
			data.Set("reason", VyshkaJsonValue.NewString(reason));
		data.Set("cause", VyshkaJsonValue.NewString(cause));
		if (actionId != "")
			data.Set("actionId", VyshkaJsonValue.NewString(actionId));
		VyshkaPlugin.Emit("core.player.kick", data);
		VyshkaLog.Info("kicking " + identity.GetName() + " (" + identity.GetPlainId() + "), cause " + cause + ": " + reason);
		GetGame().DisconnectPlayer(identity, identity.GetId());
		return true;
	}

	// Show delivers one message to one player. The notification style is the
	// engine's own pop-up (title over detail, for seconds); the chat style is
	// a line in the player's chat window in the "important" color.
	static void Show(PlayerBase player, string style, string title, string message, int seconds)
	{
		if (style == STYLE_CHAT)
		{
			string line = message;
			if (title != "")
				line = title + ": " + message;
			player.MessageImportant(line);
			return;
		}
		PlayerIdentity identity = player.GetIdentity();
		if (!identity)
			return;
		if (title == "")
			NotificationSystem.SendNotificationToPlayerIdentityExtended(identity, seconds, message);
		else
			NotificationSystem.SendNotificationToPlayerIdentityExtended(identity, seconds, title, message);
	}

	// ReadMessage validates the shared message params of vyshka.message and
	// vyshka.broadcast; false with the reason when they cannot be used.
	static bool ReadMessage(VyshkaJsonValue params, out string style, out string title, out string message, out int seconds, out string error)
	{
		style = STYLE_NOTIFICATION;
		title = "";
		message = "";
		seconds = SECONDS_DEFAULT;
		if (!params || !params.IsObject())
		{
			error = "message is required";
			return false;
		}
		message = VyshkaAction.ReadText(params, "message", MAX_MESSAGE);
		if (message == "")
		{
			error = "message is required and must not be blank";
			return false;
		}
		title = VyshkaAction.ReadText(params, "title", MAX_TITLE);
		style = params.GetString("style", STYLE_NOTIFICATION);
		if (style != STYLE_NOTIFICATION && style != STYLE_CHAT)
		{
			error = "style must be " + STYLE_NOTIFICATION + " or " + STYLE_CHAT;
			return false;
		}
		seconds = params.GetInt("seconds", SECONDS_DEFAULT);
		if (seconds < SECONDS_MIN)
			seconds = SECONDS_MIN;
		if (seconds > SECONDS_MAX)
			seconds = SECONDS_MAX;
		return true;
	}

	// MessageSchema is the params schema vyshka.message and vyshka.broadcast
	// share, in the section 6.1 subset.
	static VyshkaJsonValue MessageSchema()
	{
		VyshkaJsonValue message = VyshkaJsonValue.NewObject();
		message.Set("type", VyshkaJsonValue.NewString("string"));

		VyshkaJsonValue title = VyshkaJsonValue.NewObject();
		title.Set("type", VyshkaJsonValue.NewString("string"));

		VyshkaJsonValue seconds = VyshkaJsonValue.NewObject();
		seconds.Set("type", VyshkaJsonValue.NewString("integer"));
		seconds.Set("minimum", VyshkaJsonValue.NewInt(SECONDS_MIN));
		seconds.Set("maximum", VyshkaJsonValue.NewInt(SECONDS_MAX));
		seconds.Set("default", VyshkaJsonValue.NewInt(SECONDS_DEFAULT));

		VyshkaJsonValue styles = VyshkaJsonValue.NewArray();
		styles.Add(VyshkaJsonValue.NewString(STYLE_NOTIFICATION));
		styles.Add(VyshkaJsonValue.NewString(STYLE_CHAT));
		VyshkaJsonValue style = VyshkaJsonValue.NewObject();
		style.Set("type", VyshkaJsonValue.NewString("string"));
		style.Set("enum", styles);
		style.Set("default", VyshkaJsonValue.NewString(STYLE_NOTIFICATION));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("message", message);
		properties.Set("title", title);
		properties.Set("seconds", seconds);
		properties.Set("style", style);

		VyshkaJsonValue required = VyshkaJsonValue.NewArray();
		required.Add(VyshkaJsonValue.NewString("message"));

		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("required", required);
		schema.Set("properties", properties);
		return schema;
	}

	static VyshkaJsonValue ReasonSchema()
	{
		VyshkaJsonValue reason = VyshkaJsonValue.NewObject();
		reason.Set("type", VyshkaJsonValue.NewString("string"));
		return reason;
	}
}

class VyshkaKickAction : VyshkaAction
{
	override string Code()    { return "vyshka.kick"; }
	override string Name()    { return "Kick player"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("reason", VyshkaModeration.ReasonSchema());
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (!player)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");
		string reason = VyshkaAction.ReadText(params, "reason", VyshkaModeration.MAX_REASON);
		string name = player.GetIdentity().GetName();
		string error;
		if (!VyshkaModeration.Kick(player, reason, "action", actionId, error))
			return VyshkaActionOutcome.Failure(error);
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("name", VyshkaJsonValue.NewString(name));
		if (reason != "")
			result.Set("reason", VyshkaJsonValue.NewString(reason));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaBanAction : VyshkaAction
{
	static const int DURATION_MAX_MINUTES = 5256000;   // ten years; beyond that, use 0

	override string Code()    { return "vyshka.ban"; }
	override string Name()    { return "Ban player"; }
	override string Context() { return "player"; }
	override string Danger()  { return "destructive"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue duration = VyshkaJsonValue.NewObject();
		duration.Set("type", VyshkaJsonValue.NewString("integer"));
		duration.Set("minimum", VyshkaJsonValue.NewInt(0));
		duration.Set("maximum", VyshkaJsonValue.NewInt(DURATION_MAX_MINUTES));
		duration.Set("default", VyshkaJsonValue.NewInt(0));

		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("reason", VyshkaModeration.ReasonSchema());
		properties.Set("durationMinutes", duration);
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		string error;
		if (!VyshkaBans.Writable(error))
			return VyshkaActionOutcome.Failure(error);

		string reason = VyshkaAction.ReadText(params, "reason", VyshkaModeration.MAX_REASON);
		int minutes = 0;
		if (params && params.IsObject())
			minutes = params.GetInt("durationMinutes", 0);
		if (minutes < 0)
			minutes = 0;
		if (minutes > DURATION_MAX_MINUTES)
			minutes = DURATION_MAX_MINUTES;

		// The entry keys on the plain id. An online player resolves either
		// form of the identity; an offline one must be given the plain id.
		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		VyshkaBanEntry entry = new VyshkaBanEntry();
		entry.m_Id = referenceKey;
		entry.m_Name = "";
		if (player)
		{
			entry.m_Id = player.GetIdentity().GetPlainId();
			entry.m_Name = player.GetIdentity().GetName();
		}
		entry.m_Reason = reason;
		entry.m_BannedAt = VyshkaClock.NowRfc3339();
		entry.m_ExpiresEpoch = 0;
		if (minutes > 0)
			entry.m_ExpiresEpoch = VyshkaClock.EpochSeconds() + minutes * 60;
		entry.m_ActionId = actionId;
		if (!VyshkaBans.Add(entry, error))
			return VyshkaActionOutcome.Failure(error);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", VyshkaPlayers.Identity(entry.m_Id));
		if (entry.m_Name != "")
			data.Set("name", VyshkaJsonValue.NewString(entry.m_Name));
		if (reason != "")
			data.Set("reason", VyshkaJsonValue.NewString(reason));
		if (entry.m_ExpiresEpoch > 0)
			data.Set("expiresAt", VyshkaJsonValue.NewString(VyshkaClock.FormatRfc3339(entry.m_ExpiresEpoch)));
		data.Set("actionId", VyshkaJsonValue.NewString(actionId));
		VyshkaPlugin.Emit("core.player.ban", data);

		bool kicked = false;
		if (player)
		{
			string kickReason = "banned";
			if (reason != "")
				kickReason = "banned: " + reason;
			string kickError;
			kicked = VyshkaModeration.Kick(player, kickReason, "ban", actionId, kickError);
		}

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("player", VyshkaPlayers.Identity(entry.m_Id));
		if (entry.m_Name != "")
			result.Set("name", VyshkaJsonValue.NewString(entry.m_Name));
		result.Set("kicked", VyshkaJsonValue.NewBool(kicked));
		if (entry.m_ExpiresEpoch > 0)
			result.Set("expiresAt", VyshkaJsonValue.NewString(VyshkaClock.FormatRfc3339(entry.m_ExpiresEpoch)));
		else
			result.Set("expiresAt", VyshkaJsonValue.NewNull());
		result.Set("activeBans", VyshkaJsonValue.NewInt(VyshkaBans.Count()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaUnbanAction : VyshkaAction
{
	override string Code()    { return "vyshka.unban"; }
	override string Name()    { return "Lift ban"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		VyshkaBanEntry removed;
		string error;
		if (!VyshkaBans.Remove(referenceKey, removed, error))
			return VyshkaActionOutcome.Failure(error);

		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		data.Set("player", VyshkaPlayers.Identity(removed.m_Id));
		if (removed.m_Name != "")
			data.Set("name", VyshkaJsonValue.NewString(removed.m_Name));
		data.Set("actionId", VyshkaJsonValue.NewString(actionId));
		VyshkaPlugin.Emit("vyshka.player.unban", data);

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("removed", removed.ToJson());
		result.Set("activeBans", VyshkaJsonValue.NewInt(VyshkaBans.Count()));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaMessageAction : VyshkaAction
{
	override string Code()    { return "vyshka.message"; }
	override string Name()    { return "Message player"; }
	override string Context() { return "player"; }
	override string Danger()  { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		return VyshkaModeration.MessageSchema();
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		string style;
		string title;
		string message;
		int seconds;
		string error;
		if (!VyshkaModeration.ReadMessage(params, style, title, message, seconds, error))
			return VyshkaActionOutcome.Failure(error);
		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (!player)
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online");
		VyshkaModeration.Show(player, style, title, message, seconds);

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("name", VyshkaJsonValue.NewString(player.GetIdentity().GetName()));
		result.Set("style", VyshkaJsonValue.NewString(style));
		return VyshkaActionOutcome.Success(result);
	}
}

class VyshkaBroadcastAction : VyshkaAction
{
	override string Code()    { return "vyshka.broadcast"; }
	override string Name()    { return "Broadcast message"; }
	override string Context() { return "world"; }
	override string Danger()  { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		return VyshkaModeration.MessageSchema();
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		string style;
		string title;
		string message;
		int seconds;
		string error;
		if (!VyshkaModeration.ReadMessage(params, style, title, message, seconds, error))
			return VyshkaActionOutcome.Failure(error);

		array<Man> men = new array<Man>;
		GetGame().GetPlayers(men);
		int recipients = 0;
		for (int i = 0; i < men.Count(); i++)
		{
			PlayerBase player = PlayerBase.Cast(men.Get(i));
			if (!player || !player.GetIdentity())
				continue;
			VyshkaModeration.Show(player, style, title, message, seconds);
			recipients++;
		}

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("recipients", VyshkaJsonValue.NewInt(recipients));
		result.Set("style", VyshkaJsonValue.NewString(style));
		return VyshkaActionOutcome.Success(result);
	}
}
