// Vyshka DayZ plugin: admin flags (issue #71).
//
// Five server-authoritative per-player flags an admin sets with one action:
// god (no damage), freeze (no movement), unlimited stamina, unlimited ammo,
// and ignored by AI. A flag belongs to the identity, not the character: it
// is kept in the hub's key/value store under vyshka/flags.<Steam64> (spec
// section 12), so it survives a reconnect, a respawn, and a restart, and
// follows the identity to every server enrolled in the installation, since
// the store is installation-wide. The store is the truth and the game
// follows it: the action writes first and applies afterwards, and a
// character is given its identity's flags as it attaches, from the store.
//
// Everything here uses what the engine gives every server. God mode is the
// engine's own SetAllowDamage(false), the same call its invincibility cheat
// makes, which also stops the bleeding manager from opening a source. Freeze
// is the input controller's movement-speed override held at zero, the
// override the engine's own developer "server walk" drives a character with
// from the server. Unlimited stamina and ignored by AI reproduce what the
// engine's diagnostic builds do behind DIAG_DEVELOPER (a disabled stamina
// handler, an untargetable character), which a retail server does not
// compile; unlimited ammo refills the magazine after every shot the way the
// engine's own debug option does at five rounds. Invisibility and
// no-collision are not here: the spike under spikes/dayz-admin-flags says
// what the engine allows a server to do about them.

// VyshkaFlagSet is the five flags, true or false each.
class VyshkaFlagSet
{
	static const string GOD = "god";
	static const string FREEZE = "freeze";
	static const string UNLIMITED_STAMINA = "unlimitedStamina";
	static const string UNLIMITED_AMMO = "unlimitedAmmo";
	static const string IGNORED_BY_AI = "ignoredByAi";

	bool m_God;
	bool m_Freeze;
	bool m_UnlimitedStamina;
	bool m_UnlimitedAmmo;
	bool m_IgnoredByAi;

	// Names lists every flag, in the order the manifest and the result
	// carry them.
	static array<string> Names()
	{
		array<string> names = new array<string>;
		names.Insert(GOD);
		names.Insert(FREEZE);
		names.Insert(UNLIMITED_STAMINA);
		names.Insert(UNLIMITED_AMMO);
		names.Insert(IGNORED_BY_AI);
		return names;
	}

	static string NameList()
	{
		array<string> names = Names();
		string list = "";
		for (int i = 0; i < names.Count(); i++)
		{
			if (i > 0)
				list += ", ";
			list += names.Get(i);
		}
		return list;
	}

	bool Get(string name)
	{
		if (name == GOD)
			return m_God;
		if (name == FREEZE)
			return m_Freeze;
		if (name == UNLIMITED_STAMINA)
			return m_UnlimitedStamina;
		if (name == UNLIMITED_AMMO)
			return m_UnlimitedAmmo;
		if (name == IGNORED_BY_AI)
			return m_IgnoredByAi;
		return false;
	}

	void Set(string name, bool value)
	{
		if (name == GOD)
			m_God = value;
		else if (name == FREEZE)
			m_Freeze = value;
		else if (name == UNLIMITED_STAMINA)
			m_UnlimitedStamina = value;
		else if (name == UNLIMITED_AMMO)
			m_UnlimitedAmmo = value;
		else if (name == IGNORED_BY_AI)
			m_IgnoredByAi = value;
	}

	bool Any()
	{
		return m_God || m_Freeze || m_UnlimitedStamina || m_UnlimitedAmmo || m_IgnoredByAi;
	}

	bool Equals(VyshkaFlagSet other)
	{
		if (!other)
			return false;
		return m_God == other.m_God && m_Freeze == other.m_Freeze && m_UnlimitedStamina == other.m_UnlimitedStamina && m_UnlimitedAmmo == other.m_UnlimitedAmmo && m_IgnoredByAi == other.m_IgnoredByAi;
	}

	VyshkaFlagSet Copy()
	{
		VyshkaFlagSet copy = new VyshkaFlagSet();
		copy.m_God = m_God;
		copy.m_Freeze = m_Freeze;
		copy.m_UnlimitedStamina = m_UnlimitedStamina;
		copy.m_UnlimitedAmmo = m_UnlimitedAmmo;
		copy.m_IgnoredByAi = m_IgnoredByAi;
		return copy;
	}

	// ToJson renders the set flags only, as {"god": true, ...}: the shape
	// the snapshot carries, absent when nothing is set.
	VyshkaJsonValue ToJson()
	{
		VyshkaJsonValue flags = VyshkaJsonValue.NewObject();
		array<string> names = Names();
		for (int i = 0; i < names.Count(); i++)
		{
			if (Get(names.Get(i)))
				flags.Set(names.Get(i), VyshkaJsonValue.NewBool(true));
		}
		return flags;
	}

	// ToJsonAll renders all five, true or false: the shape the action's
	// result carries, so a reader sees the whole state after the change.
	VyshkaJsonValue ToJsonAll()
	{
		VyshkaJsonValue flags = VyshkaJsonValue.NewObject();
		array<string> names = Names();
		for (int i = 0; i < names.Count(); i++)
			flags.Set(names.Get(i), VyshkaJsonValue.NewBool(Get(names.Get(i))));
		return flags;
	}

	// FromJson reads a flags object as the store or a hand-written key may
	// hold it: every recognized member that is a boolean counts, anything
	// else (a missing member, a string, an unknown name) reads as unset.
	static VyshkaFlagSet FromJson(VyshkaJsonValue flags)
	{
		// Not named "set": that is a keyword in Enforce Script.
		VyshkaFlagSet parsed = new VyshkaFlagSet();
		if (!flags || !flags.IsObject())
			return parsed;
		array<string> names = Names();
		for (int i = 0; i < names.Count(); i++)
		{
			VyshkaJsonValue value = flags.Get(names.Get(i));
			if (value && value.IsBool())
				parsed.Set(names.Get(i), value.m_Bool);
		}
		return parsed;
	}
}

// VyshkaFlagsRecord is one identity's key in the store:
//   { "flags": { "god": true }, "name": "Survivor",
//     "updatedAt": "2026-09-17T12:00:00Z", "actionId": "01M..." }
// flags carries the set flags only; the rest says who and when, for the
// operator reading the key in the panel's store browser.
class VyshkaFlagsRecord
{
	ref VyshkaFlagSet m_Flags;
	string m_Name;
	string m_UpdatedAt;
	string m_ActionId;

	static VyshkaFlagsRecord FromJson(VyshkaJsonValue value)
	{
		VyshkaFlagsRecord record = new VyshkaFlagsRecord();
		record.m_Flags = new VyshkaFlagSet();
		if (!value || !value.IsObject())
			return record;
		record.m_Flags = VyshkaFlagSet.FromJson(value.Get("flags"));
		record.m_Name = VyshkaAction.Bound(value.GetString("name", ""), 200);
		record.m_UpdatedAt = VyshkaAction.Bound(value.GetString("updatedAt", ""), 40);
		record.m_ActionId = VyshkaAction.Bound(value.GetString("actionId", ""), 64);
		return record;
	}

	VyshkaJsonValue ToJson()
	{
		VyshkaJsonValue value = VyshkaJsonValue.NewObject();
		value.Set("flags", m_Flags.ToJson());
		if (m_Name != "")
			value.Set("name", VyshkaJsonValue.NewString(m_Name));
		if (m_UpdatedAt != "")
			value.Set("updatedAt", VyshkaJsonValue.NewString(m_UpdatedAt));
		if (m_ActionId != "")
			value.Set("actionId", VyshkaJsonValue.NewString(m_ActionId));
		return value;
	}
}

class VyshkaFlags
{
	static const string KEY_PREFIX = "flags.";
	static const int LOOKUP_RETRY_MS = 30000;
	static const int CHANGE_ATTEMPTS = 3;
	static const int DEADLINE_MARGIN_MS = 5000;

	// What this process last learned about each identity's flags, from the
	// store or from its own writes: applied at once when the identity's
	// character attaches, ahead of the store's answer.
	static ref map<string, ref VyshkaFlagSet> s_Known;
	// Identities with a store lookup in flight, so a reconnect during one
	// does not start a second.
	static ref map<string, bool> s_Loading;

	static map<string, ref VyshkaFlagSet> Known()
	{
		if (!s_Known)
			s_Known = new map<string, ref VyshkaFlagSet>;
		return s_Known;
	}

	static map<string, bool> Loading()
	{
		if (!s_Loading)
			s_Loading = new map<string, bool>;
		return s_Loading;
	}

	static void Reset()
	{
		Known().Clear();
		Loading().Clear();
	}

	static string Key(string id)
	{
		return KEY_PREFIX + id;
	}

	// Has is the hooks' question: does this character's identity carry the
	// flag right now. Read from the character, where Apply left the set,
	// so the stamina tick and the fire hook pay for no lookup.
	static bool Has(PlayerBase player, string flag)
	{
		if (!player || !player.m_VyshkaFlags)
			return false;
		return player.m_VyshkaFlags.Get(flag);
	}

	// Current is what the plugin knows of an identity's flags: an empty
	// set for one it has never heard of.
	static VyshkaFlagSet Current(string id)
	{
		VyshkaFlagSet known = Known().Get(id);
		if (known)
			return known.Copy();
		return new VyshkaFlagSet();
	}

	static void Remember(string id, VyshkaFlagSet flags)
	{
		Known().Set(id, flags.Copy());
	}

	// OnConnect runs as a character attaches to an identity (first join,
	// respawn, reconnect): what this process last knew is applied at once,
	// and the store is asked for its word, which may have changed while
	// the player was away or on another server.
	static void OnConnect(PlayerBase player, string id)
	{
		VyshkaFlagSet known = Known().Get(id);
		if (known)
			Apply(player, known);
		Lookup(id);
	}

	// Lookup asks the store for an identity's flags and applies the answer
	// to whatever character the identity has by then. Without a running
	// plugin there is no store to ask and nothing is known.
	static void Lookup(string id)
	{
		if (Loading().Contains(id))
			return;
		VyshkaStore store = VyshkaPlugin.Store();
		if (!store)
			return;
		Loading().Set(id, true);
		store.Get(VyshkaActionRegistry.KV_NAMESPACE, Key(id), new VyshkaFlagsLookup(id));
	}

	// OnLookup is the store's answer to Lookup.
	static void OnLookup(string id, VyshkaStoreResult result)
	{
		Loading().Remove(id);
		VyshkaRosterEntry entry = VyshkaPlayers.Roster().Get(id);
		if (!result.m_Ok)
		{
			// The identity may be frozen or in god mode and the store did
			// not answer: the last known set stays applied, and the store
			// is asked again while the player is online.
			if (entry)
			{
				VyshkaLog.Warn("flags of " + id + " could not be read from the store (" + result.m_Error + "); trying again in " + (LOOKUP_RETRY_MS / 1000).ToString() + " s");
				GetGame().GetCallQueue(CALL_CATEGORY_SYSTEM).CallLater(Lookup, LOOKUP_RETRY_MS, false, id);
			}
			return;
		}
		VyshkaFlagSet flags = new VyshkaFlagSet();
		if (result.m_Found)
		{
			VyshkaFlagsRecord record = VyshkaFlagsRecord.FromJson(result.m_Value);
			flags = record.m_Flags;
		}
		Remember(id, flags);
		if (entry && entry.m_Player)
			Apply(entry.m_Player, flags);
	}

	// Apply puts a flag set into effect on a character and leaves it there
	// for the hooks to read. Only what changed is touched: a character the
	// plugin never flagged keeps whatever another mod did to its damage
	// or movement, and a flag turned off restores the engine's default.
	static void Apply(PlayerBase player, VyshkaFlagSet flags)
	{
		if (!player || !flags)
			return;
		VyshkaFlagSet before = player.m_VyshkaFlags;
		if (!before)
			before = new VyshkaFlagSet();
		// Nothing to change: the store confirming what the process already
		// applied (every reconnect), or an identity with no flags attaching
		// (every ordinary player). Neither is worth a line.
		if (before.Equals(flags))
		{
			if (!player.m_VyshkaFlags)
				player.m_VyshkaFlags = flags.Copy();
			return;
		}
		player.m_VyshkaFlags = flags.Copy();

		if (flags.m_God != before.m_God)
			player.SetAllowDamage(!flags.m_God);

		if (flags.m_Freeze != before.m_Freeze)
		{
			HumanInputController input = player.GetInputController();
			if (input)
			{
				if (flags.m_Freeze)
					input.OverrideMovementSpeed(HumanInputControllerOverrideType.ENABLED, 0);
				else
					input.OverrideMovementSpeed(HumanInputControllerOverrideType.DISABLED, 0);
			}
		}

		// Stamina, ammo, and AI targeting are read by the hooks below from
		// the set left on the character; stamina is filled now as well, so
		// the flag is felt at once rather than after the idle regeneration.
		if (flags.m_UnlimitedStamina && !before.m_UnlimitedStamina)
		{
			StaminaHandler stamina = player.GetStaminaHandler();
			if (stamina)
				stamina.SetStamina(stamina.GetStaminaMax());
		}
		// Each rendering goes through a local: a method called on an object
		// a call just returned runs on an instance the engine has already
		// released (measured on DayZ 1.29 while building this slice: the
		// serializer read a null member array and the script VM stopped,
		// which took the whole plugin down with it).
		VyshkaJsonValue beforeJson = before.ToJson();
		VyshkaJsonValue afterJson = flags.ToJson();
		string beforeText = beforeJson.Serialize();
		string afterText = afterJson.Serialize();
		VyshkaLog.Info("flags of " + Describe(player) + ": " + beforeText + " to " + afterText);
	}

	// Refill tops a weapon up after a shot for a character with unlimited
	// ammo: the attached magazine to its maximum (the engine's own debug
	// option does the same at five rounds), or an internal magazine with
	// cartridges of the type just fired.
	static void Refill(Weapon_Base weapon, string ammoType)
	{
		if (!weapon)
			return;
		int muzzle = weapon.GetCurrentMuzzle();
		Magazine magazine = weapon.GetMagazine(muzzle);
		if (magazine)
		{
			if (magazine.GetAmmoCount() < magazine.GetAmmoMax())
				magazine.ServerSetAmmoMax();
			return;
		}
		if (!weapon.HasInternalMagazine(muzzle) || ammoType == "")
			return;
		int guard = 0;
		while (!weapon.IsInternalMagazineFull(muzzle) && guard < 64)
		{
			if (!weapon.PushCartridgeToInternalMagazine(muzzle, 0, ammoType))
				break;
			guard++;
		}
	}

	static string Describe(PlayerBase player)
	{
		PlayerIdentity identity = player.GetIdentity();
		if (!identity)
			return "an unidentified player";
		return identity.GetName() + " (" + identity.GetPlainId() + ")";
	}
}

// VyshkaFlagsLookup carries a connect-time lookup to its answer.
class VyshkaFlagsLookup : VyshkaStoreCallback
{
	string m_Id;

	void VyshkaFlagsLookup(string id)
	{
		m_Id = id;
	}

	override void OnStore(VyshkaStoreResult result)
	{
		VyshkaFlags.OnLookup(m_Id, result);
	}
}

// VyshkaFlagsChange is one dispatch of vyshka.flags from its first store
// call to its completion: read the identity's record, merge the requested
// flags in, write the result back guarded by the revision read (or delete
// the key when no flag remains), and, once the store holds it, apply it to
// the character if one is online and complete the dispatch. A write that
// loses its compare-and-swap (a bot or another server wrote in between)
// starts over from a fresh read, a bounded number of times.
class VyshkaFlagsChange : VyshkaStoreCallback
{
	static const int PHASE_READ = 1;
	static const int PHASE_WRITE = 2;

	string m_ActionId;
	string m_Id;
	string m_Name;                 // the player's name when online at dispatch, else the record's
	ref array<string> m_Named;     // the flags the dispatch named
	ref VyshkaFlagSet m_Requested; // their requested values
	ref VyshkaFlagSet m_Target;    // the merged set being written
	int m_DeadlineMs;              // the dispatch's own deadline less a margin: every store call ends by then
	int m_Phase;
	int m_Attempts;
	bool m_WroteOnce;              // a set has been sent at least once; the store may hold it whatever happened after

	void VyshkaFlagsChange(string actionId, string id, string name, array<string> named, VyshkaFlagSet requested)
	{
		m_ActionId = actionId;
		m_Id = id;
		m_Name = name;
		m_Named = named;
		m_Requested = requested;
		m_Attempts = 0;
		// The store gets less than the dispatch has, so a store call that
		// runs out of time fails this change while the plugin still holds
		// the dispatch, and the failure is what the hub hears; nothing can
		// land after the plugin has reported the action failed for time.
		m_DeadlineMs = VyshkaPlugin.CurrentDeadlineMs() - VyshkaFlags.DEADLINE_MARGIN_MS;
	}

	void Start()
	{
		VyshkaStore store = VyshkaPlugin.Store();
		if (!store)
		{
			Fail("the plugin is not connected to a hub, so the flags cannot be stored");
			return;
		}
		m_Phase = PHASE_READ;
		store.Get(VyshkaActionRegistry.KV_NAMESPACE, VyshkaFlags.Key(m_Id), this, m_DeadlineMs);
	}

	override void OnStore(VyshkaStoreResult result)
	{
		if (!result.m_Ok)
		{
			Fail("the flags could not be stored: " + result.m_Error);
			return;
		}
		VyshkaStore store = VyshkaPlugin.Store();
		if (!store)
		{
			Fail("the plugin stopped before the flags were stored");
			return;
		}
		if (m_Phase == PHASE_READ)
		{
			VyshkaFlagsRecord record = new VyshkaFlagsRecord();
			record.m_Flags = new VyshkaFlagSet();
			if (result.m_Found)
				record = VyshkaFlagsRecord.FromJson(result.m_Value);
			m_Target = record.m_Flags.Copy();
			for (int i = 0; i < m_Named.Count(); i++)
				m_Target.Set(m_Named.Get(i), m_Requested.Get(m_Named.Get(i)));
			if (m_Name == "")
				m_Name = record.m_Name;

			// A key that never existed and a change that sets nothing: there
			// is nothing to write, and nothing to apply beyond the empty set.
			if (!m_Target.Any() && !result.m_Found)
			{
				Finish("0");
				return;
			}
			// A change that clears the last flag writes the record back with
			// an empty set rather than deleting the key: the store's delete
			// is unconditional (section 12.2), so a delete could erase a
			// flag another writer set between this read and now, while a
			// guarded write of the marker cannot. The empty record stays
			// until an operator removes it.

			if (!VyshkaPlugin.IsPending(m_ActionId))
			{
				// The dispatch was failed while the read was out (its
				// deadline passed, or the plugin stopped): nothing is
				// written on its behalf now.
				VyshkaLog.Warn("flags change " + m_ActionId + " for " + m_Id + " is no longer pending; the store is left as read");
				return;
			}
			VyshkaFlagsRecord next = new VyshkaFlagsRecord();
			next.m_Flags = m_Target;
			next.m_Name = m_Name;
			next.m_UpdatedAt = VyshkaClock.NowRfc3339();
			next.m_ActionId = m_ActionId;
			m_Phase = PHASE_WRITE;
			m_WroteOnce = true;
			// The revision read guards the write: "0" for a key that did not
			// exist means "only if it still does not".
			store.Set(VyshkaActionRegistry.KV_NAMESPACE, VyshkaFlags.Key(m_Id), next.ToJson(), result.m_RevisionText, this, m_DeadlineMs);
			return;
		}
		if (m_Phase == PHASE_WRITE)
		{
			if (result.m_Mismatch)
			{
				m_Attempts++;
				if (m_Attempts >= VyshkaFlags.CHANGE_ATTEMPTS)
				{
					Fail("the flags of " + m_Id + " changed underneath this action " + m_Attempts.ToString() + " times; try again");
					return;
				}
				m_Phase = PHASE_READ;
				store.Get(VyshkaActionRegistry.KV_NAMESPACE, VyshkaFlags.Key(m_Id), this, m_DeadlineMs);
				return;
			}
			Finish(result.m_RevisionText);
			return;
		}
		Fail("the flags change reached a phase it does not know");
	}

	// Finish applies the stored set to the character, if one is online, and
	// completes the dispatch. The set is applied whether or not the dispatch
	// is still pending: the store holds it now, and the game follows the
	// store; a dispatch already failed for time only loses the report.
	void Finish(string revision)
	{
		VyshkaFlags.Remember(m_Id, m_Target);
		PlayerBase player = VyshkaHealAction.FindPlayer(m_Id);
		if (player)
			VyshkaFlags.Apply(player, m_Target);

		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("player", VyshkaPlayers.Identity(m_Id));
		if (m_Name != "")
			result.Set("name", VyshkaJsonValue.NewString(m_Name));
		result.Set("online", VyshkaJsonValue.NewBool(player != null));
		result.Set("flags", m_Target.ToJsonAll());
		VyshkaJsonValue changed = VyshkaJsonValue.NewArray();
		for (int i = 0; i < m_Named.Count(); i++)
			changed.Add(VyshkaJsonValue.NewString(m_Named.Get(i)));
		result.Set("changed", changed);
		VyshkaJsonValue revisionNumber = VyshkaJson.Parse(revision);
		if (!revisionNumber || !revisionNumber.IsNumber())
			revisionNumber = VyshkaJsonValue.NewInt(0);
		result.Set("revision", revisionNumber);
		VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Success(result));
	}

	// Fail answers the dispatch with the error and, when a write was ever
	// sent, asks the store for the identity's record again: a write
	// abandoned for time may still have landed (a retry of it answering
	// revision_mismatch is one sign), and the game follows the store either
	// way, whatever phase the change was in when it gave up.
	void Fail(string error)
	{
		VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Failure(error));
		if (m_WroteOnce)
			VyshkaFlags.Lookup(m_Id);
	}
}

class VyshkaFlagsAction : VyshkaAction
{
	override string Code()    { return "vyshka.flags"; }
	override string Name()    { return "Set admin flags"; }
	override string Context() { return "player"; }
	override string Danger()  { return "warning"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		array<string> names = VyshkaFlagSet.Names();
		for (int i = 0; i < names.Count(); i++)
		{
			VyshkaJsonValue flag = VyshkaJsonValue.NewObject();
			flag.Set("type", VyshkaJsonValue.NewString("boolean"));
			properties.Set(names.Get(i), flag);
		}
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		// Every flag is optional and an absent one is left as it is, so a
		// dispatch must name at least one; a named flag must be a boolean.
		array<string> named = new array<string>;
		VyshkaFlagSet requested = new VyshkaFlagSet();
		array<string> names = VyshkaFlagSet.Names();
		if (params && params.IsObject())
		{
			for (int i = 0; i < names.Count(); i++)
			{
				VyshkaJsonValue value = params.Get(names.Get(i));
				if (!value)
					continue;
				if (!value.IsBool())
					return VyshkaActionOutcome.Failure(names.Get(i) + " must be true or false");
				named.Insert(names.Get(i));
				requested.Set(names.Get(i), value.m_Bool);
			}
		}
		if (named.Count() == 0)
			return VyshkaActionOutcome.Failure("name at least one flag to set or clear: " + VyshkaFlagSet.NameList());

		if (referenceKey == "")
			return VyshkaActionOutcome.Failure("a player-context action needs the player's identity as referenceKey");
		if (!VyshkaPlugin.Store())
			return VyshkaActionOutcome.Failure("the plugin is not connected to a hub, so the flags cannot be stored");

		// The record keys on the plain id. An online player resolves either
		// form of the identity; an offline one must be given the plain id,
		// which must be a name the store accepts.
		string id = referenceKey;
		string name = "";
		PlayerBase player = VyshkaHealAction.FindPlayer(referenceKey);
		if (player && player.GetIdentity())
		{
			id = player.GetIdentity().GetPlainId();
			name = player.GetIdentity().GetName();
		}
		if (!VyshkaStore.ValidName(VyshkaFlags.Key(id), 128))
			return VyshkaActionOutcome.Failure("player " + referenceKey + " is not online, and the identity is not a plain id the store can key on");

		VyshkaFlagsChange change = new VyshkaFlagsChange(actionId, id, name, named, requested);
		change.Start();
		return VyshkaActionOutcome.Pending();
	}
}

// The stamina handler is the engine's own; a character with unlimited
// stamina is never depleted by it (sprint, jump, melee, a vault) and
// regenerates as if idle whatever it is doing. The client predicts its own
// bar and the server's sync (every half second) corrects it, so the bar may
// dip and snap back on the player's screen while the server's value, the
// one that gates sprinting, stays full.
modded class StaminaHandler
{
	override void DepleteStaminaEx(EStaminaModifiers modifier, float dT = -1, float coef = 1.0)
	{
		if (VyshkaFlags.Has(m_Player, VyshkaFlagSet.UNLIMITED_STAMINA))
			return;
		super.DepleteStaminaEx(modifier, dT, coef);
	}

	override protected void ProcessMovementState()
	{
		super.ProcessMovementState();
		if (m_StaminaDelta < 0 && VyshkaFlags.Has(m_Player, VyshkaFlagSet.UNLIMITED_STAMINA))
			m_StaminaDelta = GameConstants.STAMINA_GAIN_IDLE_PER_SEC;
	}
}

// The weapon's fire event runs on the server after each shot; a character
// with unlimited ammo has the magazine refilled there.
modded class Weapon_Base
{
	override void EEFired(int muzzleType, int mode, string ammoType)
	{
		super.EEFired(muzzleType, mode, ammoType);
		if (!GetGame().IsServer())
			return;
		PlayerBase player = PlayerBase.Cast(GetHierarchyRootPlayer());
		if (VyshkaFlags.Has(player, VyshkaFlagSet.UNLIMITED_AMMO))
			VyshkaFlags.Refill(this, ammoType);
	}
}
