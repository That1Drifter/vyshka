// Vyshka DayZ plugin: map markers (spec section 8.3, state.entities).
//
// A marker is a thing on the map that is neither a player nor a vehicle: a
// base, an event site, a beacon a mod dropped. The plugin keeps the current
// set and publishes it as a state.entities snapshot, paced and held back the
// way the players and vehicles snapshots are, so a mod puts something on the
// panel's live map without knowing anything about envelopes.
//
// A snapshot is whole: it replaces its predecessor entirely and an entry
// absent from it is gone. So the empty list is a meaningful snapshot, and it
// is published once after the last marker goes away rather than on every
// poll, which is what s_Removed below is for.
//
// Ids are the mod's to choose and must be stable for the lifetime of the
// thing they name. Use {namespace}:{thing} so two mods cannot collide.

class VyshkaMapMarker
{
	static const int ID_MAX = 128;
	static const int KIND_MAX = 128;
	static const int LABEL_MAX = 200;

	string m_Id;
	string m_Kind;
	string m_Label;
	vector m_Position;
	ref VyshkaJsonValue m_Data;

	// Place puts a marker on the map, replacing any marker with the same id:
	// a mod that places the same thing twice moves it rather than doubling
	// it. data is the mod's own extras, carried under the entry's data
	// beside the label, or null for none. Returns the marker, or null when
	// the id was unusable or the registry is full.
	static VyshkaMapMarker Place(string id, string kind, string label, vector position, VyshkaJsonValue data = null)
	{
		string markerId = VyshkaAction.Bound(id, ID_MAX);
		if (markerId == "")
		{
			VyshkaLog.Warn("a map marker needs an id; nothing was placed");
			return null;
		}
		VyshkaMapMarker marker = VyshkaMapMarkers.Find(markerId);
		if (!marker)
		{
			if (!VyshkaMapMarkers.HasRoom())
			{
				int cap = VyshkaMapMarkers.MAX_MARKERS;
				VyshkaLog.Warn("the map already carries " + cap.ToString() + " markers, which is what a snapshot may hold (spec section 8.3); " + markerId + " was not placed");
				return null;
			}
			marker = new VyshkaMapMarker();
			marker.m_Id = markerId;
			VyshkaMapMarkers.Keep(marker);
		}
		marker.m_Kind = VyshkaAction.Bound(kind, KIND_MAX);
		marker.m_Label = VyshkaAction.Bound(label, LABEL_MAX);
		marker.m_Position = position;
		marker.m_Data = data;
		return marker;
	}

	static VyshkaMapMarker Find(string id)
	{
		return VyshkaMapMarkers.Find(id);
	}

	static int Count()
	{
		return VyshkaMapMarkers.Count();
	}

	void Move(vector position)
	{
		m_Position = position;
	}

	void SetData(VyshkaJsonValue data)
	{
		m_Data = data;
	}

	// Remove takes the marker off the map. The next snapshot says so, even
	// when it was the last one.
	void Remove()
	{
		VyshkaMapMarkers.Forget(this);
	}

	// Describe is one state.entities entry: the id and kind at the top, the
	// position when it is one the protocol can carry, and the label with the
	// mod's own extras under data. A mod that puts its own label in data
	// keeps it: its value is written over this one.
	VyshkaJsonValue Describe()
	{
		VyshkaJsonValue entry = VyshkaJsonValue.NewObject();
		entry.Set("id", VyshkaJsonValue.NewString(m_Id));
		if (m_Kind != "")
			entry.Set("kind", VyshkaJsonValue.NewString(m_Kind));
		VyshkaJsonValue position = VyshkaGeo.Position(m_Position);
		if (position)
			entry.Set("position", position);
		VyshkaJsonValue data = VyshkaJsonValue.NewObject();
		if (m_Label != "")
			data.Set("label", VyshkaJsonValue.NewString(m_Label));
		if (m_Data && m_Data.IsObject())
		{
			for (int i = 0; i < m_Data.Count(); i++)
			{
				string key = m_Data.KeyAt(i);
				VyshkaJsonValue value = m_Data.ValueAt(i);
				if (key != "" && value)
					data.Set(key, value);
			}
		}
		entry.Set("data", data);
		return entry;
	}
}

// VyshkaMapMarkers is the registry behind the markers: the current set and
// whether anything left it since the last capture.
class VyshkaMapMarkers
{
	// A snapshot carries at most 5000 entries (spec section 8.3), so the
	// registry refuses the 5001st rather than building a body the hub would
	// reject whole.
	static const int MAX_MARKERS = 5000;

	static ref array<ref VyshkaMapMarker> s_Markers;
	static bool s_Removed;   // a marker was taken off the map since the last capture

	static array<ref VyshkaMapMarker> All()
	{
		if (!s_Markers)
			s_Markers = new array<ref VyshkaMapMarker>;
		return s_Markers;
	}

	// Reset drops every marker, at the start of a mission, and owes the hub
	// one snapshot whatever the set held: the hub keeps the latest snapshot
	// per type across a restart (spec section 8.3), so it may still show a
	// marker the previous process placed, and a process that starts with
	// none has no other way to say so. The cost is one small empty snapshot
	// per boot on a server with no markers.
	static void Reset()
	{
		array<ref VyshkaMapMarker> markers = All();
		markers.Clear();
		s_Removed = true;
	}

	static VyshkaMapMarker Find(string id)
	{
		if (id == "")
			return null;
		array<ref VyshkaMapMarker> markers = All();
		for (int i = 0; i < markers.Count(); i++)
		{
			VyshkaMapMarker marker = markers.Get(i);
			if (marker.m_Id == id)
				return marker;
		}
		return null;
	}

	static int Count()
	{
		array<ref VyshkaMapMarker> markers = All();
		return markers.Count();
	}

	static bool HasRoom()
	{
		array<ref VyshkaMapMarker> markers = All();
		return markers.Count() < MAX_MARKERS;
	}

	static void Keep(VyshkaMapMarker marker)
	{
		if (!marker)
			return;
		array<ref VyshkaMapMarker> markers = All();
		markers.Insert(marker);
	}

	static void Forget(VyshkaMapMarker marker)
	{
		if (!marker)
			return;
		array<ref VyshkaMapMarker> markers = All();
		for (int i = 0; i < markers.Count(); i++)
		{
			VyshkaMapMarker held = markers.Get(i);
			if (held == marker)
			{
				markers.RemoveOrdered(i);
				s_Removed = true;
				return;
			}
		}
	}

	// Capture builds the state.entities body when there is something to say:
	// a marker exists, or the last one is gone and the hub has not been told.
	// Otherwise it returns "" and no snapshot is queued, which is what keeps
	// a server with no markers from publishing an empty list forever.
	static string Capture()
	{
		array<ref VyshkaMapMarker> markers = All();
		if (markers.Count() == 0 && !s_Removed)
			return "";
		VyshkaJsonValue entities = VyshkaJsonValue.NewArray();
		for (int i = 0; i < markers.Count(); i++)
		{
			VyshkaMapMarker marker = markers.Get(i);
			entities.Add(marker.Describe());
		}
		// Cleared here, as the body is built: the caller captures as it
		// builds the poll and queues what it gets. A body the outbox then
		// refused was never sent, and the caller says so through
		// Unpublished, so the removal is owed again.
		s_Removed = false;
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("capturedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
		body.Set("entities", entities);
		return body.Serialize();
	}

	// Unpublished is the caller's word that the body Capture last built was
	// not queued. With no marker left the next capture would have nothing
	// to say and the hub would keep the last marker forever; owing the
	// removal again makes the next capture speak.
	static void Unpublished()
	{
		s_Removed = true;
	}
}
