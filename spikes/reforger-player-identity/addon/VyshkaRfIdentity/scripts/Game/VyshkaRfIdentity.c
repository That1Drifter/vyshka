// Vyshka spike #159: what a backend-registered Reforger dedicated server's script reads as
// a joining player's identity. Each read is one tab-separated line in the server's console
// log:
//
//   VYSHKA_RFID <tick ms> <hook> player=<id> identity=<..> (<n> chars) legacy=<..> (<n> chars) kind=<..> platformId=<..> name=<..>
//
// It does nothing else: no requests, no files, nothing sent to the client.

class VyshkaRfIdentity
{
	//------------------------------------------------------------------------------------------------
	static void Read(string hook, int playerId)
	{
		BackendApi api = GetGame().GetBackendApi();
		string identity = string.Format("%1", SCR_PlayerIdentityUtils.GetPlayerIdentityId(playerId));
		string legacy = api.GetPlayerIdentityId(playerId);
		string kind = typename.EnumToString(PlatformKind, api.GetPlayerPlatformKind(playerId));
		string platformId = api.GetPlayerPlatformId(playerId);
		string name = GetGame().GetPlayerManager().GetPlayerName(playerId);
		Print(string.Format("VYSHKA_RFID\t%1\t%2\tplayer=%3\tidentity=%4 (%5 chars)\tlegacy=%6 (%7 chars)\tkind=%8\tplatformId=%9", System.GetTickCount(), hook, playerId, identity, identity.Length(), legacy, legacy.Length(), kind, platformId) + "\tname=" + name, LogLevel.NORMAL);
	}

	//------------------------------------------------------------------------------------------------
	static void Later(int playerId)
	{
		Read("later.10s", playerId);
	}
}

//------------------------------------------------------------------------------------------------
modded class SCR_BaseGameMode
{
	protected override void OnGameStart()
	{
		super.OnGameStart();
		Print(string.Format("VYSHKA_RFID\t%1\thook.OnGameStart\tisServer=%2\trplMode=%3", System.GetTickCount(), Replication.IsServer(), typename.EnumToString(RplMode, RplSession.Mode())), LogLevel.NORMAL);
	}

	override void OnPlayerConnected(int playerId)
	{
		super.OnPlayerConnected(playerId);
		if (Replication.IsServer())
			VyshkaRfIdentity.Read("hook.OnPlayerConnected", playerId);
	}

	override void OnPlayerAuditSuccess(int iPlayerID)
	{
		super.OnPlayerAuditSuccess(iPlayerID);
		if (!Replication.IsServer())
			return;
		VyshkaRfIdentity.Read("hook.OnPlayerAuditSuccess", iPlayerID);
		GetGame().GetCallqueue().CallLater(VyshkaRfIdentity.Later, 10000, false, iPlayerID);
	}

	override void OnPlayerAuditFail(int iPlayerID)
	{
		super.OnPlayerAuditFail(iPlayerID);
		if (Replication.IsServer())
			VyshkaRfIdentity.Read("hook.OnPlayerAuditFail", iPlayerID);
	}

	protected override void OnPlayerRegistered(int playerId)
	{
		super.OnPlayerRegistered(playerId);
		if (Replication.IsServer())
			VyshkaRfIdentity.Read("hook.OnPlayerRegistered", playerId);
	}

	protected override void OnPlayerDisconnected(int playerId, KickCauseCode cause, int timeout)
	{
		if (Replication.IsServer())
			VyshkaRfIdentity.Read("hook.OnPlayerDisconnected", playerId);
		super.OnPlayerDisconnected(playerId, cause, timeout);
	}
}
