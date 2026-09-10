// Vyshka DayZ plugin: a refusal the hub delivered inline (spec section 2.3).
//
// The engine hands script nothing but an opaque error code for a non-2xx
// response, so every request the plugin sends asks the hub for its refusals
// inline: a 200 whose body is the protocol error with the status the hub
// would have used. A success body never carries a top-level "error" member,
// which is what makes the two distinguishable in OnSuccess.

class VyshkaHubError
{
	string m_Code;
	int m_Status;
	string m_Message;
	int m_Index;   // details.index of an envelope_invalid refusal; -1 when absent
	int m_Seq;     // details.seq of the same, 0 when absent

	// FromBody returns the refusal a response body carries, or null when the
	// body is a success (no top-level "error" object).
	static VyshkaHubError FromBody(VyshkaJsonValue root)
	{
		if (!root || !root.IsObject())
			return null;
		VyshkaJsonValue failure = root.Get("error");
		if (!failure || !failure.IsObject())
			return null;

		VyshkaHubError refusal = new VyshkaHubError();
		refusal.m_Code = failure.GetString("code", "");
		refusal.m_Status = failure.GetInt("status", 0);
		refusal.m_Message = failure.GetString("message", "");
		refusal.m_Index = -1;
		refusal.m_Seq = 0;
		VyshkaJsonValue details = failure.Get("details");
		if (details && details.IsObject())
		{
			VyshkaJsonValue index = details.Get("index");
			if (index && index.IsNumber() && index.m_IsInteger)
				refusal.m_Index = index.m_Int;
			refusal.m_Seq = details.GetInt("seq", 0);
		}
		return refusal;
	}

	bool IsUnauthorized()
	{
		return m_Status == 401;
	}

	bool IsServerError()
	{
		return m_Status >= 500;
	}

	// Describe renders the refusal for a log line: code, status, message.
	string Describe()
	{
		string text = m_Code;
		if (text == "")
			text = "unnamed error";
		if (m_Status > 0)
			text += " (" + m_Status.ToString() + ")";
		if (m_Message != "")
			text += ": " + m_Message;
		return text;
	}
}
