// Vyshka DayZ plugin: actions and the manifest they add up to.
//
// An action is one entry in the manifest (spec section 6) plus the code that
// runs when the hub dispatches it (section 7). Actions register with the
// registry at boot; the registry publishes the manifest and routes
// dispatches. Game-facing actions live in the world module, where PlayerBase
// and friends are visible; this file only knows the protocol shape.
//
// The registry holds the rest of what a manifest says as well: the custom
// contexts the plugin can enumerate (VyshkaContext, section 6.2), the custom
// event types it declares (section 6.3), and the key/value namespaces it
// uses (section 6.6). Another mod adds its own through the
// MissionServer.VyshkaRegister hook, which runs before the plugin starts, so
// everything it registers is in the first manifest.

class VyshkaActionOutcome
{
	bool m_Ok;
	bool m_Pending;                 // the outcome comes later, through VyshkaPlugin.Complete
	ref VyshkaJsonValue m_Result;   // arbitrary JSON, null for none
	string m_Error;

	// Pending is the outcome of an action that has more to do before it can
	// say how it went: one that waits on the hub's key/value store, for
	// instance. The plugin holds the dispatch open and sends no result until
	// the action calls VyshkaPlugin.Complete with the real outcome, or the
	// hold runs out (VyshkaPlugin.PENDING_MAX_MS) and it is failed.
	static VyshkaActionOutcome Pending()
	{
		VyshkaActionOutcome outcome = new VyshkaActionOutcome();
		outcome.m_Pending = true;
		return outcome;
	}

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

	// Execute runs the action. actionId is the hub's id for this dispatch,
	// which an action puts in any event it emits so a feed entry can be
	// joined to the audit log. params is the dispatched params value, which
	// the hub has validated against ParamsSchema; it may still be null or
	// of a surprising shape when a non-conformant hub sends it, so
	// implementations read it defensively.
	VyshkaActionOutcome Execute(string actionId, string context, string referenceKey, VyshkaJsonValue params)
	{
		return VyshkaActionOutcome.Failure("action not implemented");
	}

	// ReadText reads a string param, trimmed and cut to maxLength characters;
	// "" when absent or not a string. The section 6.1 subset has no length
	// keyword, so the bound lives here, and a value over it is shortened
	// rather than refused because a long reason is still a reason.
	static string ReadText(VyshkaJsonValue params, string key, int maxLength)
	{
		if (!params || !params.IsObject())
			return "";
		VyshkaJsonValue value = params.Get(key);
		if (!value || !value.IsString())
			return "";
		string text = value.m_Text;
		return Bound(text.Trim(), maxLength);
	}

	// Bound cuts text to maxLength characters, counted and cut in UTF-8
	// characters rather than bytes, so a multi-byte character at the bound
	// is dropped whole instead of leaving a partial sequence behind.
	static string Bound(string text, int maxLength)
	{
		if (text.LengthUtf8() > maxLength)
			return text.SubstringUtf8(0, maxLength);
		return text;
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

class VyshkaRegistry
{
	// The key/value namespace the plugin's own actions use (spec section
	// 6.6). A mod declares its own with DeclareNamespace; the manifest
	// carries the union, and the store client confines every call to that
	// list before the hub does.
	static const string KV_NAMESPACE = "vyshka";

	// The manifest's length bounds, counted in code points as the companion
	// schema counts them (spec sections 6.2 and 6.3).
	static const int CONTEXT_ID_MAX = 64;
	static const int EVENT_ID_MAX = 128;
	static const int LABEL_MAX = 200;
	static const int NAMESPACE_MAX = 64;

	ref array<ref VyshkaAction> m_Actions;
	ref array<ref VyshkaContext> m_Contexts;
	ref array<string> m_EventIds;              // the declared event ids, for the duplicate check
	ref array<ref VyshkaJsonValue> m_Events;   // one declaration each, parallel to m_EventIds
	ref array<string> m_Namespaces;

	void VyshkaRegistry()
	{
		m_Actions = new array<ref VyshkaAction>;
		m_Contexts = new array<ref VyshkaContext>;
		m_EventIds = new array<string>;
		m_Events = new array<ref VyshkaJsonValue>;
		m_Namespaces = new array<string>;
		m_Namespaces.Insert(KV_NAMESPACE);
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
			VyshkaAction action = m_Actions.Get(i);
			if (action.Code() == code)
				return action;
		}
		return null;
	}

	int Count()
	{
		return m_Actions.Count();
	}

	// RegisterContext adds a custom context (spec section 6.2), which the
	// plugin declares in the manifest and enumerates on request. A duplicate
	// id keeps the first, as a duplicate action code does: the manifest must
	// declare each id once, and a mod arriving second should not take a
	// context away from the mod that owns it.
	void RegisterContext(VyshkaContext context)
	{
		if (!context)
			return;
		string id = context.Id();
		if (id == "")
		{
			VyshkaLog.Warn("a context with no id was not registered");
			return;
		}
		// An id past the bound is refused rather than shortened: the manifest
		// would carry the short form and the plugin would look the long one
		// up, so the context could never be enumerated.
		if (id.LengthUtf8() > CONTEXT_ID_MAX)
		{
			VyshkaLog.Warn("context " + id + " has an id longer than " + CONTEXT_ID_MAX.ToString() + " characters (spec section 6.2) and was not registered");
			return;
		}
		if (FindContext(id))
		{
			VyshkaLog.Warn("context " + id + " registered twice; keeping the first");
			return;
		}
		m_Contexts.Insert(context);
	}

	VyshkaContext FindContext(string id)
	{
		if (id == "")
			return null;
		for (int i = 0; i < m_Contexts.Count(); i++)
		{
			VyshkaContext context = m_Contexts.Get(i);
			if (context.Id() == id)
				return context;
		}
		return null;
	}

	int ContextCount()
	{
		return m_Contexts.Count();
	}

	// DeclareEvent declares a custom telemetry type (spec section 6.3).
	// Declaration is advisory: it drives panel display and webhook filtering,
	// and an undeclared event is carried all the same. payloadSchema is the
	// section 6.1 schema subset for the event's data, or null for none.
	void DeclareEvent(string id, string name, string namespace, VyshkaJsonValue payloadSchema = null)
	{
		if (id == "")
		{
			VyshkaLog.Warn("an event with no id was not declared");
			return;
		}
		if (m_EventIds.Find(id) >= 0)
		{
			VyshkaLog.Warn("event " + id + " declared twice; keeping the first");
			return;
		}
		VyshkaJsonValue declaration = VyshkaJsonValue.NewObject();
		declaration.Set("id", VyshkaJsonValue.NewString(VyshkaAction.Bound(id, EVENT_ID_MAX)));
		declaration.Set("name", VyshkaJsonValue.NewString(VyshkaAction.Bound(name, LABEL_MAX)));
		declaration.Set("namespace", VyshkaJsonValue.NewString(VyshkaAction.Bound(namespace, NAMESPACE_MAX)));
		if (payloadSchema)
			declaration.Set("payload", payloadSchema);
		m_EventIds.Insert(id);
		m_Events.Insert(declaration);
	}

	// DeclareNamespace adds a key/value namespace the mod uses (spec section
	// 6.6). A name the store's own grammar refuses (section 12.1) is not
	// declared: the hub would reject the manifest over it, which would take
	// every other mod's actions down with it.
	void DeclareNamespace(string namespace)
	{
		if (m_Namespaces.Find(namespace) >= 0)
			return;
		if (!VyshkaStoreClient.ValidName(namespace, NAMESPACE_MAX))
		{
			VyshkaLog.Warn("the key/value namespace " + namespace + " is not a name the store accepts (spec section 12.1) and was not declared");
			return;
		}
		m_Namespaces.Insert(namespace);
	}

	// Namespaces is the declared key/value namespaces, sorted and without
	// repeats: what the manifest publishes and what the store client confines
	// itself to. The order is fixed rather than the order they were declared
	// in, so two boots that register the same mods in a different order
	// produce the same manifest content and so the same revision.
	array<string> Namespaces()
	{
		array<string> sorted = new array<string>;
		for (int i = 0; i < m_Namespaces.Count(); i++)
		{
			string name = m_Namespaces.Get(i);
			int at = 0;
			while (at < sorted.Count() && Before(sorted.Get(at), name))
				at++;
			sorted.InsertAt(name, at);
		}
		return sorted;
	}

	// Before orders two names by their characters. Enforce Script compares
	// strings for equality only, so the comparison is made on the character
	// codes; every name here is from the store's grammar, which is ASCII.
	static bool Before(string first, string second)
	{
		int firstLength = first.Length();
		int secondLength = second.Length();
		int shortest = firstLength;
		if (secondLength < shortest)
			shortest = secondLength;
		for (int i = 0; i < shortest; i++)
		{
			string a = first.Get(i);
			string b = second.Get(i);
			int codeA = a.ToAscii();
			int codeB = b.ToAscii();
			if (codeA != codeB)
				return codeA < codeB;
		}
		return firstLength < secondLength;
	}

	VyshkaActionOutcome Execute(string actionId, string code, string context, string referenceKey, VyshkaJsonValue params)
	{
		VyshkaAction action = Find(code);
		if (!action)
			return VyshkaActionOutcome.Failure("this plugin declares no action " + code);
		return action.Execute(actionId, context, referenceKey, params);
	}

	// Manifest is everything the manifest.publish body of spec section 6 says
	// about what this plugin can do, without the revision that says when it
	// last changed.
	VyshkaJsonValue Manifest(string game, string pluginName, string pluginVersion)
	{
		VyshkaJsonValue body = VyshkaJsonValue.NewObject();
		body.Set("game", VyshkaJsonValue.NewString(game));
		VyshkaJsonValue plugin = VyshkaJsonValue.NewObject();
		plugin.Set("name", VyshkaJsonValue.NewString(pluginName));
		plugin.Set("version", VyshkaJsonValue.NewString(pluginVersion));
		body.Set("plugin", plugin);
		VyshkaJsonValue actions = VyshkaJsonValue.NewArray();
		for (int i = 0; i < m_Actions.Count(); i++)
		{
			VyshkaAction action = m_Actions.Get(i);
			actions.Add(action.Declaration());
		}
		body.Set("actions", actions);
		VyshkaJsonValue contexts = VyshkaJsonValue.NewArray();
		for (int j = 0; j < m_Contexts.Count(); j++)
		{
			VyshkaContext context = m_Contexts.Get(j);
			contexts.Add(context.Declaration());
		}
		body.Set("contexts", contexts);
		VyshkaJsonValue events = VyshkaJsonValue.NewArray();
		for (int k = 0; k < m_Events.Count(); k++)
			events.Add(m_Events.Get(k));
		body.Set("events", events);
		VyshkaJsonValue namespaces = VyshkaJsonValue.NewArray();
		array<string> declared = Namespaces();
		for (int n = 0; n < declared.Count(); n++)
			namespaces.Add(VyshkaJsonValue.NewString(declared.Get(n)));
		body.Set("kvNamespaces", namespaces);
		return body;
	}

	// ManifestBody is the manifest.publish body of spec section 6. The
	// revision is the plugin's (VyshkaPlugin.ResolveManifestRevision): the
	// hub ignores a manifest whose revision is not above the one it stored
	// (section 6.1).
	string ManifestBody(string game, string pluginName, string pluginVersion, int revision)
	{
		VyshkaJsonValue body = Manifest(game, pluginName, pluginVersion);
		body.Set("manifestRevision", VyshkaJsonValue.NewInt(revision));
		return body.Serialize();
	}

	// ManifestContent is the same body without the revision: what the plugin
	// compares against the content it published last, to tell a boot that
	// changed nothing from one that did.
	string ManifestContent(string game, string pluginName, string pluginVersion)
	{
		VyshkaJsonValue body = Manifest(game, pluginName, pluginVersion);
		return body.Serialize();
	}
}
