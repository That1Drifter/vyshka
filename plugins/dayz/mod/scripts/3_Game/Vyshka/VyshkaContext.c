// Vyshka DayZ plugin: custom contexts and their members (spec section 6.2).
//
// A context is what an action acts on. The hub knows four of them by name
// (world, player, vehicle, object) and a plugin may declare its own: a
// territory, a base, a landmark. An action whose context names a declared id
// takes that context's referenceKey on dispatch, which the hub passes
// through without interpreting it.
//
// A hub that wants to offer a context's members (a dropdown, a bot
// completion) sends context.enumerate and the plugin answers with
// context.entries. Enumeration is a read of what is: answering twice is
// harmless, a lost answer costs a repeat, and a request naming a context
// this plugin does not declare is still answered, with an empty list and a
// reason, so the hub learns that it and the manifest disagree instead of
// waiting.
//
// A mod subclasses VyshkaContext, registers it with the registry in its
// MissionServer.VyshkaRegister hook, and fills the list handed to Enumerate.

// VyshkaGeo renders world positions for the protocol. The world module has
// its own renderer for the same shape (VyshkaPlayers.Position), which the
// game module cannot see; this is the one contexts and map markers use.
class VyshkaGeo
{
	// Position renders a world position as [x, y, z], or null when a
	// component is not a number the JSON writer can carry (sections 6.2 and
	// 8.3 want finite numbers, and an entry with a bad position would have
	// the whole body rejected).
	static VyshkaJsonValue Position(vector pos)
	{
		VyshkaJsonValue position = VyshkaJsonValue.NewArray();
		for (int axis = 0; axis < 3; axis++)
		{
			VyshkaJsonValue component = VyshkaJsonValue.NewFloat(pos[axis]);
			if (!component)
				return null;
			position.Add(component);
		}
		return position;
	}
}

class VyshkaContext
{
	// Id is what an action's Context() names and what the hub enumerates by;
	// at most 64 code points and unique within the manifest. Use
	// {namespace}.{name} so two mods cannot collide.
	string Id()        { return ""; }
	string Name()      { return ""; }
	string Namespace() { return ""; }

	// Enumerate fills the list with this context's members. It runs on the
	// script thread while the poll that carries the answer is being handled,
	// so it does what a frame can afford and no more: a context whose members
	// take a round trip to learn keeps them in memory and answers from that.
	void Enumerate(VyshkaContextList list)
	{
	}

	VyshkaJsonValue Declaration()
	{
		VyshkaJsonValue declaration = VyshkaJsonValue.NewObject();
		declaration.Set("id", VyshkaJsonValue.NewString(VyshkaAction.Bound(Id(), VyshkaRegistry.CONTEXT_ID_MAX)));
		declaration.Set("name", VyshkaJsonValue.NewString(VyshkaAction.Bound(Name(), VyshkaRegistry.LABEL_MAX)));
		declaration.Set("namespace", VyshkaJsonValue.NewString(VyshkaAction.Bound(Namespace(), VyshkaRegistry.NAMESPACE_MAX)));
		return declaration;
	}
}

// VyshkaContextList is the entries of one enumeration, built by a context's
// Enumerate and serialized into the context.entries reply. The bounds of
// section 6.2 are applied here rather than left to the hub: a reply the hub
// rejects tells the operator nothing, where a shortened label still names
// the thing.
class VyshkaContextList
{
	static const int MAX_ENTRIES = 5000;
	static const int KEY_MAX = 128;
	static const int LABEL_MAX = 200;

	ref VyshkaJsonValue m_Entries;
	int m_Dropped;      // entries past the cap, for one log line at the end

	void VyshkaContextList()
	{
		m_Entries = VyshkaJsonValue.NewArray();
	}

	// Add records a member with no position: the referenceKey the hub hands
	// back on dispatch, and a label to show.
	void Add(string referenceKey, string label)
	{
		AddEntry(referenceKey, label, null, null);
	}

	// AddAt records a member that is somewhere on the map.
	void AddAt(string referenceKey, string label, vector position)
	{
		VyshkaJsonValue rendered = VyshkaGeo.Position(position);
		AddEntry(referenceKey, label, rendered, null);
	}

	// AddEntry records a member with whatever it has: position and data may
	// both be null. data is the mod's own extras and is carried through
	// untouched.
	void AddEntry(string referenceKey, string label, VyshkaJsonValue position, VyshkaJsonValue data)
	{
		string key = VyshkaAction.Bound(referenceKey, KEY_MAX);
		if (key == "")
		{
			VyshkaLog.Warn("a context entry without a referenceKey was dropped");
			return;
		}
		if (m_Entries.Count() >= MAX_ENTRIES)
		{
			m_Dropped++;
			return;
		}
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("referenceKey", VyshkaJsonValue.NewString(key));
		entry.Set("label", VyshkaJsonValue.NewString(VyshkaAction.Bound(label, LABEL_MAX)));
		if (position && position.IsArray())
			entry.Set("position", position);
		if (data && data.IsObject())
			entry.Set("data", data);
		m_Entries.Add(entry);
	}

	int Count()
	{
		return m_Entries.Count();
	}

	int Dropped()
	{
		return m_Dropped;
	}

	// ToJson is the entries array the reply carries.
	VyshkaJsonValue ToJson()
	{
		return m_Entries;
	}
}
