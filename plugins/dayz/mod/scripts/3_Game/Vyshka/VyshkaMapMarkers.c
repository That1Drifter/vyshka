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
	int m_Bytes;   // what the registry last counted this marker at (VyshkaMapMarkers.Resize)

	// Place puts a marker on the map, replacing any marker with the same id:
	// a mod that places the same thing twice moves it rather than doubling
	// it. data is the mod's own extras, carried under the entry's data
	// beside the label, or null for none; the marker keeps a copy of its
	// own, so what the caller does with the object afterwards changes
	// nothing on the map. Returns the marker, or null when nothing was
	// placed: the id was unusable, the registry is full, or the entries
	// would pass the byte budget (a marker already on the map then keeps
	// what it had, and Find gives it back).
	static VyshkaMapMarker Place(string id, string kind, string label, vector position, VyshkaJsonValue data = null)
	{
		string markerId = VyshkaAction.Bound(id, ID_MAX);
		if (markerId == "")
		{
			VyshkaLog.Warn("a map marker needs an id; nothing was placed");
			return null;
		}
		VyshkaMapMarker marker = VyshkaMapMarkers.Find(markerId);
		bool fresh = !marker;
		if (fresh)
		{
			if (!VyshkaMapMarkers.HasRoom())
			{
				int cap = VyshkaMapMarkers.MAX_MARKERS;
				VyshkaLog.Warn("the map already carries " + cap.ToString() + " markers, which is what a snapshot may hold (spec section 8.3); " + markerId + " was not placed");
				return null;
			}
			marker = new VyshkaMapMarker();
			marker.m_Id = markerId;
		}
		string kindBefore = marker.m_Kind;
		string labelBefore = marker.m_Label;
		vector positionBefore = marker.m_Position;
		VyshkaJsonValue dataBefore = marker.m_Data;
		marker.m_Kind = VyshkaAction.Bound(kind, KIND_MAX);
		marker.m_Label = VyshkaAction.Bound(label, LABEL_MAX);
		marker.m_Position = position;
		marker.m_Data = VyshkaMapMarkers.Own(data);
		if (!VyshkaMapMarkers.Fits(marker))
		{
			// Refused either way: a fresh marker is not placed, and one
			// already on the map keeps what it had (Find gives it back).
			VyshkaLog.Warn("marker " + markerId + " would put the map past the " + VyshkaMapMarkers.BYTE_BUDGET.ToString() + " bytes a snapshot may carry (spec section 8.3); it was not placed");
			if (fresh)
				return null;
			marker.m_Kind = kindBefore;
			marker.m_Label = labelBefore;
			marker.m_Position = positionBefore;
			marker.m_Data = dataBefore;
			return null;
		}
		if (fresh)
			VyshkaMapMarkers.Keep(marker);
		else
			VyshkaMapMarkers.Resize(marker);
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

	// Move puts the marker somewhere else. A position that would not fit
	// (a longer number) is refused the way a placement is, and the marker
	// stays where it was.
	void Move(vector position)
	{
		vector before = m_Position;
		m_Position = position;
		if (!VyshkaMapMarkers.Fits(this))
		{
			VyshkaLog.Warn("marker " + m_Id + " could not be moved: the map would pass the " + VyshkaMapMarkers.BYTE_BUDGET.ToString() + " bytes a snapshot may carry (spec section 8.3)");
			m_Position = before;
			return;
		}
		VyshkaMapMarkers.Resize(this);
	}

	// SetData replaces the marker's extras, unless they would not fit, in
	// which case the marker keeps what it had.
	void SetData(VyshkaJsonValue data)
	{
		VyshkaJsonValue before = m_Data;
		m_Data = VyshkaMapMarkers.Own(data);
		if (!VyshkaMapMarkers.Fits(this))
		{
			VyshkaLog.Warn("marker " + m_Id + " could not take its new data: the map would pass the " + VyshkaMapMarkers.BYTE_BUDGET.ToString() + " bytes a snapshot may carry (spec section 8.3)");
			m_Data = before;
			return;
		}
		VyshkaMapMarkers.Resize(this);
	}

	// Bytes is what this marker's entry costs in the snapshot body, with the
	// comma that separates it from the next.
	int Bytes()
	{
		VyshkaJsonValue entry = Describe();
		string text = entry.Serialize();
		return text.Length() + 1;
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

	// A snapshot body is capped at 262144 bytes as well (section 8.3), and a
	// body over the hub's request limit would wedge the whole poll stream
	// behind an immutable envelope (section 9.3). The entries share this
	// budget, which leaves room for the body's own members; a marker that
	// would pass it is refused at placement rather than dropped at capture,
	// so a capture always fits.
	static const int BYTE_BUDGET = 261000;
	static const int BODY_MAX_BYTES = 262144;   // the snapshot body cap itself (section 8.3)

	static ref array<ref VyshkaMapMarker> s_Markers;
	static bool s_Removed;   // a marker was taken off the map since the last capture
	static int s_Bytes;      // the entries' bytes as last counted, kept markers only

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
		s_Bytes = 0;
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

	// Own copies the extras a caller hands in, so the marker holds an object
	// nobody else can change after its size was counted: a caller adding to
	// its own object later would otherwise grow the entry past the budget
	// unseen. A value that is not an object reads as none. The copy goes
	// through the JSON text, which is what the entry is made of anyway.
	static VyshkaJsonValue Own(VyshkaJsonValue data)
	{
		if (!data || !data.IsObject())
			return null;
		string text = data.Serialize();
		VyshkaJsonValue copy = VyshkaJson.Parse(text);
		if (!copy || !copy.IsObject())
			return null;
		return copy;
	}

	// Fits says whether the marker, as it now reads, keeps the entries
	// inside the byte budget: the bytes of every other kept marker plus its
	// own. A kept marker is counted at its new size in place of its old one.
	static bool Fits(VyshkaMapMarker marker)
	{
		int others = s_Bytes;
		if (Find(marker.m_Id) == marker)
			others -= marker.m_Bytes;
		int own = marker.Bytes();
		return others + own <= BYTE_BUDGET;
	}

	// Resize recounts a kept marker after a change Fits allowed.
	static void Resize(VyshkaMapMarker marker)
	{
		if (Find(marker.m_Id) != marker)
			return;
		s_Bytes -= marker.m_Bytes;
		marker.m_Bytes = marker.Bytes();
		s_Bytes += marker.m_Bytes;
	}

	static void Keep(VyshkaMapMarker marker)
	{
		if (!marker)
			return;
		array<ref VyshkaMapMarker> markers = All();
		markers.Insert(marker);
		marker.m_Bytes = marker.Bytes();
		s_Bytes += marker.m_Bytes;
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
				s_Bytes -= held.m_Bytes;
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
		string text = body.Serialize();
		// The budget at placement keeps this from happening; should it
		// anyway (a count gone wrong), no body goes out: an envelope the hub
		// refuses whole tells the map nothing, and one over the request
		// limit would hold the whole outbox behind it (spec section 9.3).
		int bytes = text.Length();
		if (bytes > BODY_MAX_BYTES)
		{
			VyshkaLog.Error("the state.entities body is " + bytes.ToString() + " bytes, over the " + BODY_MAX_BYTES.ToString() + " a snapshot may carry; not published (the marker budget should have prevented this)");
			return "";
		}
		return text;
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
