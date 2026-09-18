// Vyshka sample mod: a landmark context, a beacon action, and the store.
//
// This is the whole surface a mod gets from the plugin, used once each:
//
//   a custom context (spec section 6.2) the hub can enumerate, so an admin
//   picks a landmark from a list instead of typing a key;
//   an action against it, which the hub dispatches with the landmark the
//   admin picked as its referenceKey;
//   a map marker, which rides the state.entities snapshot onto the panel's
//   live map;
//   a telemetry event, which lands in the feed;
//   the key/value store, which counts the beacons placed at each landmark
//   and survives a restart.
//
// The action completes late, after the store answers, which is what any
// action that needs a round trip does: it returns Pending and the plugin
// holds the dispatch open until VyshkaPlugin.Complete brings the outcome.
// The beacon is placed either way, so a store the hub cannot serve costs the
// count, not the beacon; the result says which.
//
// Everything here is behind #ifdef VYSHKA: without @Vyshka loaded ahead of
// this mod, the file compiles to nothing and the mod still loads.

#ifdef VYSHKA

// SampleLandmarks is the two places a beacon may go. Their positions
// are approximate Chernarus coordinates, close enough to put a marker on the
// map and not surveyed.
class SampleLandmarks
{
	static const string GREEN_MOUNTAIN = "green-mountain";
	static const string NWAF = "nwaf";

	static bool Known(string key)
	{
		return key == GREEN_MOUNTAIN || key == NWAF;
	}

	static string Name(string key)
	{
		if (key == GREEN_MOUNTAIN)
			return "Green Mountain";
		if (key == NWAF)
			return "Northwest Airfield";
		return "";
	}

	static vector Position(string key)
	{
		if (key == GREEN_MOUNTAIN)
			return Vector(3700, 402, 5980);
		if (key == NWAF)
			return Vector(4600, 340, 10400);
		return Vector(0, 0, 0);
	}

	// NameList is what a failure tells an admin who named something else.
	static string NameList()
	{
		return GREEN_MOUNTAIN + ", " + NWAF;
	}
}

// SampleLandmarkContext is the custom context: the hub asks it what it
// holds, and the plugin answers with these two entries. A real mod would
// enumerate whatever it keeps in memory (its territories, its bases) here.
class SampleLandmarkContext : VyshkaContext
{
	override string Id()        { return "sample.landmark"; }
	override string Name()      { return "Landmark"; }
	override string Namespace() { return "sample"; }

	override void Enumerate(VyshkaContextList list)
	{
		AddLandmark(list, SampleLandmarks.GREEN_MOUNTAIN);
		AddLandmark(list, SampleLandmarks.NWAF);
	}

	// AddLandmark puts one landmark in the list: the key the hub hands back
	// as an action's referenceKey, the name to show, and where it is, so a
	// hub with a map can offer the choice on one.
	void AddLandmark(VyshkaContextList list, string key)
	{
		string label = SampleLandmarks.Name(key);
		vector position = SampleLandmarks.Position(key);
		list.AddAt(key, label, position);
	}
}

// SampleBeaconCount is the store half of one dispatch: read the
// landmark's count, write it back one higher guarded by the revision it read,
// and complete the action with what the store ended up holding. A write that
// loses its compare-and-swap (another server placed a beacon at the same
// landmark in between) reads again and tries once more; the store is
// installation-wide, so that race is real rather than theoretical.
class SampleBeaconCount : VyshkaStoreCallback
{
	static const int PHASE_READ = 1;
	static const int PHASE_WRITE = 2;
	static const int ATTEMPTS = 2;             // the first write plus one retry
	static const int DEADLINE_MARGIN_MS = 5000;

	string m_ActionId;
	string m_Landmark;
	string m_Label;
	vector m_Position;
	ref VyshkaStore m_Store;
	int m_Phase;
	int m_Attempts;
	int m_Count;
	int m_DeadlineMs;

	void SampleBeaconCount(string actionId, string landmark, string label, vector position)
	{
		m_ActionId = actionId;
		m_Landmark = landmark;
		m_Label = label;
		m_Position = position;
		m_Attempts = 0;
		m_Count = 0;
		// The store gets less time than the dispatch has, so a call that runs
		// out of it fails this work while the plugin still holds the dispatch
		// and the failure is what the hub hears.
		m_DeadlineMs = VyshkaPlugin.CurrentDeadlineMs() - DEADLINE_MARGIN_MS;
		VyshkaLink link = GetVyshka();
		m_Store = link.Store("sample");
	}

	string Key()
	{
		return "beacons." + m_Landmark;
	}

	void Start()
	{
		m_Phase = PHASE_READ;
		string key = Key();
		m_Store.Get(key, this, m_DeadlineMs);
	}

	override void OnStore(VyshkaStoreResult result)
	{
		if (!result.m_Ok)
		{
			Finish(false, result.m_Error);
			return;
		}
		string key = Key();
		if (m_Phase == PHASE_READ)
		{
			m_Count = 0;
			if (result.m_Found && result.m_Value && result.m_Value.IsObject())
				m_Count = result.m_Value.GetInt("count", 0);
			m_Count++;
			VyshkaJsonValue value = VyshkaJsonValue.NewObject();
			value.Set("count", VyshkaJsonValue.NewInt(m_Count));
			value.Set("label", VyshkaJsonValue.NewString(m_Label));
			value.Set("updatedAt", VyshkaJsonValue.NewString(VyshkaClock.NowRfc3339()));
			m_Phase = PHASE_WRITE;
			m_Attempts++;
			// The revision read guards the write: "0" for a key that did not
			// exist means "only if it still does not".
			m_Store.Set(key, value, result.m_RevisionText, this, m_DeadlineMs);
			return;
		}
		if (m_Phase == PHASE_WRITE)
		{
			if (result.m_Mismatch)
			{
				if (m_Attempts >= ATTEMPTS)
				{
					Finish(false, "the beacon count at " + m_Landmark + " changed underneath this action " + m_Attempts.ToString() + " times");
					return;
				}
				m_Phase = PHASE_READ;
				m_Store.Get(key, this, m_DeadlineMs);
				return;
			}
			Finish(true, "");
			return;
		}
		Finish(false, "the beacon count reached a phase it does not know");
	}

	// Finish answers the dispatch. The beacon was placed before any of this
	// ran, so a store that could not be reached is reported beside a success,
	// not as one: stored says whether the count moved.
	void Finish(bool stored, string error)
	{
		VyshkaJsonValue result = VyshkaJsonValue.NewObject();
		result.Set("landmark", VyshkaJsonValue.NewString(m_Landmark));
		result.Set("label", VyshkaJsonValue.NewString(m_Label));
		VyshkaJsonValue position = VyshkaGeo.Position(m_Position);
		if (position)
			result.Set("position", position);
		result.Set("count", VyshkaJsonValue.NewInt(m_Count));
		result.Set("stored", VyshkaJsonValue.NewBool(stored));
		if (!stored)
			result.Set("storeError", VyshkaJsonValue.NewString(error));
		VyshkaPlugin.Complete(m_ActionId, VyshkaActionOutcome.Success(result));
	}
}

class SampleBeaconAction : VyshkaAction
{
	static const int LABEL_MAX = 200;

	override string Code()      { return "sample.beacon"; }
	override string Name()      { return "Place a beacon"; }
	override string Context()   { return "sample.landmark"; }
	override string Namespace() { return "sample"; }
	override string Danger()    { return "none"; }

	override VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue label = VyshkaJsonValue.NewObject();
		label.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("label", label);
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	// PlacedSchema is the payload of the event this mod declares. Declaring
	// an event is advisory (spec section 6.3): it tells a panel what to
	// expect, and an undeclared event is carried all the same.
	static VyshkaJsonValue PlacedSchema()
	{
		VyshkaJsonValue text = VyshkaJsonValue.NewObject();
		text.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue label = VyshkaJsonValue.NewObject();
		label.Set("type", VyshkaJsonValue.NewString("string"));
		VyshkaJsonValue properties = VyshkaJsonValue.NewObject();
		properties.Set("landmark", text);
		properties.Set("label", label);
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", properties);
		return schema;
	}

	override VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		// The referenceKey is whatever the hub was given: the key of an entry
		// this mod enumerated, when an admin picked one from the list, and
		// anything at all when one was typed.
		string landmark = referenceKey;
		if (!SampleLandmarks.Known(landmark))
			return VyshkaActionOutcome.Failure("referenceKey must name a landmark: " + SampleLandmarks.NameList());

		string label = VyshkaAction.ReadText(params, "label", LABEL_MAX);
		if (label == "")
			label = SampleLandmarks.Name(landmark);
		vector position = SampleLandmarks.Position(landmark);

		VyshkaLink link = GetVyshka();

		// The marker goes on the map now. Its id carries the mod's name so
		// no other mod's marker can collide with it, and placing the same
		// beacon twice moves the one marker rather than making a second.
		VyshkaJsonValue markerData = VyshkaJsonValue.NewObject();
		markerData.Set("landmark", VyshkaJsonValue.NewString(landmark));
		VyshkaMapMarker marker = link.Mark("sample:beacon:" + landmark, "beacon", label, position, markerData);
		if (!marker)
		{
			// The map refused it (full, or past what a snapshot may carry;
			// the plugin's log says which). Nothing was placed, so nothing is
			// announced or counted.
			return VyshkaActionOutcome.Failure("the map could not take the beacon at " + landmark + "; see the plugin's log");
		}

		VyshkaJsonValue placed = VyshkaJsonValue.NewObject();
		placed.Set("landmark", VyshkaJsonValue.NewString(landmark));
		placed.Set("label", VyshkaJsonValue.NewString(label));
		VyshkaJsonValue where = VyshkaGeo.Position(position);
		if (where)
			placed.Set("position", where);
		link.Emit("sample.beacon.placed", placed);

		// The count is a round trip away, so the action says it is pending
		// and the store's answer completes it.
		SampleBeaconCount count = new SampleBeaconCount(actionId, landmark, label, position);
		count.Start();
		return VyshkaActionOutcome.Pending();
	}
}

#endif
