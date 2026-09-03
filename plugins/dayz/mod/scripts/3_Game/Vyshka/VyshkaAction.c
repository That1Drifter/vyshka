// Vyshka DayZ plugin: actions and the manifest they add up to.
//
// An action is one entry in the manifest (spec section 6) plus the code that
// runs when the hub dispatches it (section 7). Actions register with the
// registry at boot; the registry publishes the manifest and routes
// dispatches. Game-facing actions live in the world module, where PlayerBase
// and friends are visible; this file only knows the protocol shape.

class VyshkaActionOutcome
{
	bool m_Ok;
	ref VyshkaJsonValue m_Result;   // arbitrary JSON, null for none
	string m_Error;

	static VyshkaActionOutcome Success(VyshkaJsonValue result)
	{
		VyshkaActionOutcome outcome = new VyshkaActionOutcome();
		outcome.m_Ok = true;
		outcome.m_Result = result;
		return outcome;
	}

	static VyshkaActionOutcome Failure(string error)
	{
		VyshkaActionOutcome outcome = new VyshkaActionOutcome();
		outcome.m_Ok = false;
		outcome.m_Error = error;
		return outcome;
	}
}

class VyshkaAction
{
	// Code is globally unique within a server and SHOULD be namespace.name.
	string Code()      { return ""; }
	string Name()      { return ""; }
	string Context()   { return "world"; }
	string Namespace() { return "vyshka"; }
	string Danger()    { return "none"; }

	// ParamsSchema is the section 6.1 JSON Schema subset the hub validates
	// dispatch params against before they ever reach this plugin.
	VyshkaJsonValue ParamsSchema()
	{
		VyshkaJsonValue schema = VyshkaJsonValue.NewObject();
		schema.Set("type", VyshkaJsonValue.NewString("object"));
		schema.Set("properties", VyshkaJsonValue.NewObject());
		return schema;
	}

	// Execute runs the action. params is the dispatched params value, which
	// the hub has validated against ParamsSchema; it may still be null or
	// of a surprising shape when a non-conformant hub sends it, so
	// implementations read it defensively.
	VyshkaActionOutcome Execute(string context, string referenceKey, VyshkaJsonValue params)
	{
		return VyshkaActionOutcome.Failure("action not implemented");
	}

	VyshkaJsonValue Declaration()
	{
		VyshkaJsonValue declaration = VyshkaJsonValue.NewObject();
		declaration.Set("code", VyshkaJsonValue.NewString(Code()));
		declaration.Set("name", VyshkaJsonValue.NewString(Name()));
		declaration.Set("context", VyshkaJsonValue.NewString(Context()));
		declaration.Set("namespace", VyshkaJsonValue.NewString(Namespace()));
		declaration.Set("danger", VyshkaJsonValue.NewString(Danger()));
		declaration.Set("params", ParamsSchema());
		return declaration;
	}
}

class VyshkaActionRegistry
{
	// Bump when the set of actions or any schema changes; the hub ignores a
	// manifest whose revision is not above the one it stored (section 6.1).
	static const int MANIFEST_REVISION = 1;

	ref array<ref VyshkaAction> m_Actions;

	void VyshkaActionRegistry()
	{
		m_Actions = new array<ref VyshkaAction>;
	}

	void Register(VyshkaAction action)
	{
		if (!action || action.Code() == "")
			return;
		if (Find(action.Code()))
		{
			VyshkaLog.Warn("action " + action.Code() + " registered twice; keeping the first");
			return;
		}
		m_Actions.Insert(action);
	}

	VyshkaAction Find(string code)
	{
		for (int i = 0; i < m_Actions.Count(); i++)
		{
			if (m_Actions.Get(i).Code() == code)
				return m_Actions.Get(i);
		}
		return null;
	}

	int Count()
	{
		return m_Actions.Count();
	}

	VyshkaActionOutcome Execute(string code, string context, string referenceKey, VyshkaJsonValue params)
	{
		VyshkaAction action = Find(code);
		if (!action)
			return VyshkaActionOutcome.Failure("this plugin declares no action " + code);
		return action.Execute(context, referenceKey, params);
	}

	// ManifestBody is the manifest.publish body of spec section 6.
	string ManifestBody(string game, string pluginName, string pluginVersion)
	{
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("game", VyshkaJsonValue.NewString(game));
		VyshkaJsonValue plugin = VyshkaJsonValue.NewObject();
		plugin.Set("name", VyshkaJsonValue.NewString(pluginName));
		plugin.Set("version", VyshkaJsonValue.NewString(pluginVersion));
		body.Set("plugin", plugin);
		body.Set("manifestRevision", VyshkaJsonValue.NewInt(MANIFEST_REVISION));
		VyshkaJsonValue actions = VyshkaJsonValue.NewArray();
		for (int i = 0; i < m_Actions.Count(); i++)
			actions.Add(m_Actions.Get(i).Declaration());
		body.Set("actions", actions);
		body.Set("contexts", VyshkaJsonValue.NewArray());
		body.Set("events", VyshkaJsonValue.NewArray());
		return body.Serialize();
	}
}
